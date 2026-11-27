#!/usr/bin/env python3
"""Offline protocol-shaped fixtures; live APIs and Docker builds are unverified."""

import base64
import copy
import hashlib
import importlib.util
import io
import json
import os
import re
import subprocess
import sys
import tarfile
import tempfile
import unittest
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from unittest.mock import patch

ROOT = Path(sys.argv[1])
sys.argv = sys.argv[:1]
spec = importlib.util.spec_from_file_location("bundle", ROOT / "scripts/release/generate-image-bundle.py")
bundle = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bundle)


class Response(io.BytesIO):
    def __init__(self, payload, headers=None):
        super().__init__(payload)
        self.headers = headers or {}


def response_json(value):
    return Response(json.dumps(value).encode())


class ResolverTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="scion-resolver-test-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name) / "repo"
        self.root.mkdir()
        self.core = b"core artifact fixture"
        self.npm = b"npm artifact fixture"
        self.pypi = b"PyPI source artifact fixture"
        self.kubectl = b"kubectl artifact fixture"
        stream = io.BytesIO()
        with tarfile.open(fileobj=stream, mode="w:gz") as archive:
            for arch in ("amd64", "arm64"):
                member = tarfile.TarInfo(f"golangci-lint-1.2.3-linux-{arch}/golangci-lint")
                payload = b"lint artifact fixture"
                member.size = len(payload)
                archive.addfile(member, io.BytesIO(payload))
        self.lint = stream.getvalue()
        self.image = json.dumps({"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json",
                                 "manifests": []}).encode()
        self.calls = []
        for context in bundle.CONTEXTS:
            destination = self.root / context
            if context in ("go.mod", "go.sum"):
                destination.write_bytes((ROOT / context).read_bytes())
            else:
                destination.mkdir(parents=True, exist_ok=True)
                (destination / ".fixture").write_text("fixture\n")
        for folder in ("harnesses", "image-build"):
            for source in (ROOT / folder).rglob("Dockerfile"):
                destination = self.root / source.relative_to(ROOT)
                destination.parent.mkdir(parents=True, exist_ok=True)
                text = source.read_text()
                text = re.sub(r"ARG HELM_SHA256_(AMD64|ARM64)=.*",
                              lambda m: f"ARG HELM_SHA256_{m[1]}={hashlib.sha256(self.core).hexdigest()}", text)
                destination.write_text(text)
        installer = self.root / "image-build/lib/install-core-toolchain.sh"
        installer.parent.mkdir(parents=True)
        installer.write_bytes((ROOT / "image-build/lib/install-core-toolchain.sh").read_bytes())
        subprocess.run(["git", "init", "-q", str(self.root)], check=True)
        subprocess.run(["git", "-C", str(self.root), "add", "."], check=True)
        subprocess.run(["git", "-C", str(self.root), "-c", "user.name=Fixture",
                        "-c", "user.email=fixture@example.invalid", "commit", "-qm", "offline fixture"], check=True)

    def fetch(self, url, headers=None):
        self.calls.append((url, headers))
        if url.endswith("/stable.txt"):
            return Response(b"v1.2.3\n")
        if url.endswith("/releases/latest"):
            return response_json({"tag_name": "v1.2.3"})
        if "/v2/" in url:
            return Response(self.image, {"Docker-Content-Digest": "sha256:" + hashlib.sha256(self.image).hexdigest()})
        if "mirror.invalid/npm/" in url:
            version = "1.3.0" if url.endswith("/1.3.0") else "1.2.3"
            return response_json({
                "name": urllib.parse.unquote(url.rsplit("/", 2)[1]), "version": version,
                "dist": {"tarball": "https://mirror.invalid/artifacts/npm.tgz",
                         "integrity": "sha512-" + base64.b64encode(hashlib.sha512(self.npm).digest()).decode()},
            })
        if "mirror.invalid/pypi/" in url:
            return response_json({
                "info": {"version": "1.2.3"},
                "urls": [{"packagetype": "sdist", "filename": "hermes_agent-1.2.3.tar.gz",
                          "url": "https://mirror.invalid/artifacts/hermes.tar.gz",
                          "digests": {"sha256": hashlib.sha256(self.pypi).hexdigest()}}],
            })
        if url.endswith("/npm.tgz"):
            return Response(self.npm)
        if url.endswith("/hermes.tar.gz"):
            return Response(self.pypi)
        if "/bin/linux/" in url and url.endswith("/kubectl"):
            return Response(self.kubectl)
        if "/golangci-lint/releases/download/" in url:
            return Response(self.lint)
        return Response(self.core)

    def locks(self):
        return {name: {
            "version": "1.2.3", "linux": {
                arch: {"url": f"https://mirror.invalid/{name}-{arch}.tar.gz",
                       "sha256": hashlib.sha256(self.core).hexdigest()}
                for arch in ("amd64", "arm64")}}
            for name in bundle.OPAQUE_HARNESSES}

    def generate(self, locks=None, complete=True):
        return bundle.generate(self.root, "HEAD", self.root.parent / "release", True, complete,
                               "https://mirror.invalid/npm", "https://mirror.invalid/pypi", locks)

    def test_real_manifest_env_installer_and_sbom_path(self):
        with patch.object(bundle, "fetch", self.fetch):
            manifest = self.generate(self.locks())
        output = self.root.parent / "release"
        self.assertEqual(14, len(manifest["harness_tools"]))
        self.assertTrue(all(t["status"] == "resolved" for t in manifest["harness_tools"]))
        self.assertTrue(all(t["version"] != "latest" for t in manifest["harness_tools"]))
        self.assertEqual("1.0.114", next(t["version"] for t in manifest["harness_tools"] if t["name"] == "copilot"))
        self.assertEqual("1.3.0", next(t["version"] for t in manifest["harness_tools"] if t["name"] == "pi"))
        kubectl = output / "fixture-kubectl"
        lint = output / "fixture-lint"
        kubectl.write_bytes(self.kubectl)
        lint.write_bytes(self.lint)
        subprocess.run(["bash", str(ROOT / "image-build/lib/test-install-core-pins.sh"),
                        str(output / "core-pins.env"), str(kubectl), str(lint)], check=True,
                       stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        kubectl.unlink()
        lint.unlink()
        subprocess.run(["sha256sum", "-c", "SHA256SUMS"], cwd=output, check=True, stdout=subprocess.PIPE)
        env = dict(line.split("=", 1) for line in (output / "harness-pins.env").read_text().splitlines())
        self.assertEqual(env["HERMES_URL"], "https://mirror.invalid/artifacts/hermes.tar.gz")
        self.assertIn("COPILOT_CLI_SHA256_ARM64", env)
        self.assertIn("GROK_CLI_URL_AMD64", env)
        core = dict(line.split("=", 1) for line in (output / "core-pins.env").read_text().splitlines())
        self.assertIn("@sha256:", core["NODE_IMAGE"])
        bom = json.loads((output / "sbom.cdx.json").read_text())
        self.assertEqual(("CycloneDX", "1.5"), (bom["bomFormat"], bom["specVersion"]))
        self.assertEqual([{"phase": "pre-build"}], bom["metadata"]["lifecycles"])
        self.assertEqual(24, len([c for c in bom["components"] if c["type"] == "application"]))
        library = next(c for c in bom["components"] if c["name"] == "github.com/google/uuid")
        self.assertEqual("v1.6.0", library["version"])
        self.assertTrue(any(p["value"].startswith("h1:") for p in library["properties"]))
        self.assertNotIn("hashes", library)  # Go h1 is not an archive SHA256.
        first = {p.name: p.read_bytes() for p in output.iterdir()}
        with patch.object(bundle, "fetch", self.fetch):
            self.generate(self.locks())
        self.assertEqual(first, {p.name: p.read_bytes() for p in output.iterdir()})

    def test_opaque_refusal_and_no_output_mutation(self):
        with patch.object(bundle, "fetch", self.fetch):
            with self.assertRaisesRegex(ValueError, "harnesses unresolved"):
                self.generate()
        self.assertFalse((self.root.parent / "release").exists())

    def test_offline_does_not_fetch_or_claim_pins(self):
        with patch.object(bundle, "fetch", side_effect=AssertionError("offline network")):
            manifest = bundle.generate(self.root, "HEAD", self.root.parent / "release")
        self.assertTrue(all(t["status"] == "unresolved" for t in manifest["harness_tools"]))
        self.assertTrue(all(b["digest"] is None for b in manifest["base_images"]))
        self.assertEqual("", (self.root.parent / "release/harness-pins.env").read_text())
        self.assertTrue(all("reason" in t for t in manifest["harness_tools"] if t["name"] in bundle.OPAQUE_HARNESSES))

    def test_npm_integrity_failure(self):
        with patch.object(bundle, "fetch", return_value=Response(b"changed")):
            with self.assertRaisesRegex(ValueError, "integrity mismatch"):
                bundle.download_hash("https://mirror.invalid/npm.tgz",
                                     "sha512-" + base64.b64encode(hashlib.sha512(self.npm).digest()).decode())

    def test_npm_missing_or_weak_integrity(self):
        with self.assertRaisesRegex(ValueError, "supported SHA2"):
            bundle.download_hash("https://mirror.invalid/npm.tgz", "sha1-YQ==")
        original = self.fetch
        def missing(url, headers=None):
            if "mirror.invalid/npm/" in url:
                return response_json({"name": urllib.parse.unquote(url.rsplit("/", 2)[1]),
                                      "version": "1.2.3", "dist": {"tarball": "https://mirror.invalid/npm.tgz"}})
            return original(url, headers)
        with patch.object(bundle, "fetch", missing), self.assertRaises(KeyError):
            self.generate(self.locks())

    def test_pypi_hash_and_yanked_refusal(self):
        for yanked in (False, True):
            def changed(url, headers=None):
                if "mirror.invalid/pypi/" in url:
                    return response_json({"info": {"version": "1.2.3"}, "urls": [{
                        "packagetype": "sdist", "filename": "hermes.tar.gz", "yanked": yanked,
                        "url": "https://mirror.invalid/artifacts/hermes.tar.gz",
                        "digests": {"sha256": "0" * 64}}]})
                return self.fetch(url, headers)
            with patch.object(bundle, "fetch", changed), self.assertRaises(ValueError):
                self.generate(self.locks())

    def test_operator_lock_floating_hash_and_unknown_refusal(self):
        for change in ("version", "sha", "unknown"):
            locks = self.locks()
            if change == "version":
                locks["grok-build"]["version"] = "latest"
            elif change == "sha":
                locks["grok-build"]["linux"]["arm64"]["sha256"] = "0" * 64
            else:
                locks["unknown"] = locks["grok-build"]
            with patch.object(bundle, "fetch", self.fetch), self.assertRaises(ValueError):
                self.generate(locks)

    def test_registry_digest_mismatch(self):
        with patch.object(bundle, "fetch", return_value=Response(self.image, {"Docker-Content-Digest": "sha256:" + "0" * 64})):
            with self.assertRaisesRegex(ValueError, "digest mismatch"):
                bundle.registry_digest("node:24-trixie-slim")

    def test_registry_anonymous_auth_pull_scope(self):
        calls = []
        def auth(url, headers=None):
            calls.append((url, headers))
            if len(calls) == 1:
                raise urllib.error.HTTPError(url, 401, "Unauthorized", {
                    "WWW-Authenticate": 'Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:wrong:push"'},
                    io.BytesIO())
            if url.startswith("https://auth.docker.io/token?"):
                self.assertIn("scope=repository%3Alibrary%2Fnode%3Apull", url)
                return response_json({"access_token": "fixture"})
            self.assertEqual("Bearer fixture", headers["Authorization"])
            return Response(self.image)
        with patch.object(bundle, "fetch", auth):
            self.assertEqual("sha256:" + hashlib.sha256(self.image).hexdigest(),
                             bundle.registry_digest("node:24-trixie-slim"))

    def test_registry_stage_and_unbound_base_are_not_external(self):
        with patch.object(bundle, "fetch", self.fetch):
            manifest = self.generate(self.locks())
        stages = [b for b in manifest["base_images"] if b["kind"] == "stage"]
        self.assertTrue(any(b["reference"] == "builder-source" for b in stages))
        dependencies = [b for b in manifest["base_images"] if b["kind"] == "build-dependency"]
        self.assertTrue(dependencies)
        self.assertTrue(all(b["digest"] is None for b in stages + dependencies))
        broken = copy.deepcopy(manifest)
        next(b for b in broken["base_images"] if b["kind"] == "external")["digest"] = None
        with self.assertRaisesRegex(ValueError, "base images unresolved"):
            bundle.check_complete(broken)

    def test_urls_and_versions_are_not_credential_or_shell_channels(self):
        for url in ("http://mirror.invalid/x", "https://user@mirror.invalid/x", "https://mirror.invalid/x#fragment"):
            with self.assertRaises(ValueError):
                bundle.safe_url(url)
        for version in ("latest", "^1.2.3", "1.2.3;true", "1.2.3-rc.1"):
            with self.assertRaises(ValueError):
                bundle.concrete(version)

    def run_consumer(self, name, inputs, payload=None):
        work = Path(tempfile.mkdtemp(dir=self.temporary.name))
        bin_dir = work / "bin"
        bin_dir.mkdir()
        (work / "archive").write_bytes(self.npm if payload is None else payload)
        scripts = {
            "curl": '''#!/bin/sh
printf 'download\\n' >> "$TEST_WORK/events"
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-o" ]; then cp "$TEST_WORK/archive" "$2"; exit; fi
  shift
done
cat "$TEST_WORK/archive"
''',
            "npm": '''#!/bin/sh
printf '%s\\n' "$*" >> "$TEST_WORK/npm"
''',
            "tar": '''#!/bin/sh
printf 'extract\\n' >> "$TEST_WORK/events"
cat >/dev/null
''',
            "uname": '#!/bin/sh\nprintf "%s\\n" "$TEST_ARCH"\n',
            "chmod": "#!/bin/sh\nexit 0\n",
            "ln": "#!/bin/sh\nexit 0\n",
            "pi": "#!/bin/sh\nexit 0\n",
            "node": '''#!/bin/sh
if [ "$1" = "-p" ]; then printf '1.3.0\\n'; else exec "$TEST_NODE" "$@"; fi
''',
        }
        for name_, script in scripts.items():
            path = bin_dir / name_
            path.write_text(script)
            path.chmod(0o755)
        text = (ROOT / f"harnesses/{name}/Dockerfile").read_text()
        commands = []
        lines = iter(text.splitlines())
        for line in lines:
            if not line.startswith("RUN "):
                continue
            command = line[4:]
            while command.endswith("\\"):
                command = command[:-1] + " " + next(lines)
            commands.append(command)
        marker = "ARCH=$(uname" if name == "copilot" else "id=npmrc"
        command = next(c for c in commands if marker in c)
        command = re.sub(r"^--mount=\S+\s+", "", command.replace("\\\n", " "))
        command = command.replace("/tmp/copilot.tar.gz", str(work / "copilot.tar.gz"))
        command = command.replace("/opt/scion-image-build/lib/install-pinned-npm.sh",
                                  str(ROOT / "image-build/lib/install-pinned-npm.sh"))
        env = {**os.environ, **inputs, "PATH": str(bin_dir) + ":" + os.environ["PATH"],
               "TEST_WORK": str(work), "TEST_ARCH": inputs.get("TEST_ARCH", "x86_64"),
               "TEST_NODE": subprocess.check_output(["which", "node"], text=True).strip()}
        result = subprocess.run(["sh", "-c", command], env=env, capture_output=True, text=True)
        events = (work / "events").read_text() if (work / "events").exists() else ""
        npm = (work / "npm").read_text() if (work / "npm").exists() else ""
        return result, events, npm

    def test_npm_real_producer_to_consumer_identity(self):
        with patch.object(bundle, "fetch", self.fetch):
            self.generate(self.locks())
        env = dict(line.split("=", 1) for line in
                   (self.root.parent / "release/harness-pins.env").read_text().splitlines())
        for name, (_, argument) in bundle.NPM_HARNESSES.items():
            with self.subTest(harness=name):
                prefix = argument.removesuffix("_VERSION")
                result, events, npm = self.run_consumer(name, env, b"different archive same package/version")
                self.assertNotEqual(0, result.returncode, result.stderr)
                self.assertNotRegex(npm, r"install|ci")
                self.assertIn("download", events)
                self.assertEqual("https://mirror.invalid/artifacts/npm.tgz", env.get(prefix + "_URL"))
                result, _, npm = self.run_consumer(name, env)
                self.assertEqual(0, result.returncode, result.stderr)
                self.assertRegex(npm, r"install .*[/]package.tgz")
                self.assertNotIn(bundle.NPM_HARNESSES[name][0] + "@", npm)
                for suffix, value in (("_SHA256", "0" * 64), ("_INTEGRITY", "sha512-" +
                                      base64.b64encode(b"\0" * 64).decode())):
                    result, _, npm = self.run_consumer(name, {**env, prefix + suffix: value})
                    self.assertNotEqual(0, result.returncode, result.stderr)
                    self.assertNotRegex(npm, r"install|ci")
                for suffix in ("_URL", "_SHA256", "_INTEGRITY"):
                    result, events, npm = self.run_consumer(name, {**env, prefix + suffix: ""})
                    self.assertNotEqual(0, result.returncode, result.stderr)
                    self.assertNotIn("download", events)
                    self.assertNotRegex(npm, r"install|ci")
                for key, value in ((argument, "latest"), (argument, "1.2.3\n"),
                                   (prefix + "_SHA256", "0" * 64 + "\n"),
                                   (prefix + "_INTEGRITY", "sha512-invalid"),
                                   (prefix + "_URL", "http://mirror.invalid/npm.tgz")):
                    result, events, npm = self.run_consumer(name, {**env, key: value})
                    self.assertNotEqual(0, result.returncode, result.stderr)
                    self.assertNotIn("download", events)
                    self.assertNotRegex(npm, r"install|ci")

    def test_npm_no_pin_paths(self):
        for name, (package, argument) in bundle.NPM_HARNESSES.items():
            version = "1.3.0" if name == "pi" else "" if name == "opencode" else "latest"
            result, events, npm = self.run_consumer(name, {argument: version})
            self.assertEqual(0, result.returncode, result.stderr)
            self.assertNotIn("download", events)
            if name == "pi":
                self.assertIn("ci --prefix /opt/scion-pi", npm)
            else:
                expected = package if name == "opencode" else package + "@latest"
                self.assertIn("install -g " + expected + "\n", npm)
        result, _, npm = self.run_consumer("opencode", {"OPENCODE_VERSION": "1.2.3"})
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertIn("install -g opencode-ai@1.2.3\n", npm)

    def test_copilot_pin_mode_fail_closed(self):
        digest = hashlib.sha256(self.npm).hexdigest()
        for machine, target, other in (("x86_64", "AMD64", "ARM64"), ("aarch64", "ARM64", "AMD64")):
            valid = {"TEST_ARCH": machine, "COPILOT_CLI_VERSION": "1.0.114",
                     "COPILOT_CLI_SHA256_" + target: digest, "COPILOT_CLI_SHA256_" + other: ""}
            result, events, _ = self.run_consumer("copilot", valid)
            self.assertEqual(0, result.returncode, result.stderr)
            self.assertIn("extract", events)
            for change in (
                {"COPILOT_CLI_SHA256_" + target: "", "COPILOT_CLI_SHA256_" + other: digest},
                {"COPILOT_CLI_SHA256_" + target: "malformed"},
                {"COPILOT_CLI_SHA256_" + target: digest + "\n"},
                {"COPILOT_CLI_VERSION": "latest"}, {"COPILOT_CLI_VERSION": ""},
                {"COPILOT_CLI_VERSION": "1.0.114;true"},
                {"COPILOT_CLI_VERSION": "1.0.114\n"},
            ):
                with self.subTest(machine=machine, change=change):
                    result, events, _ = self.run_consumer("copilot", {**valid, **change})
                    self.assertNotEqual(0, result.returncode, result.stderr)
                    self.assertNotIn("download", events)
                    self.assertNotIn("extract", events)
            result, events, _ = self.run_consumer("copilot", valid, b"mismatch")
            self.assertNotEqual(0, result.returncode, result.stderr)
            self.assertIn("download", events)
            self.assertNotIn("extract", events)
            legacy = {**valid, "COPILOT_CLI_VERSION": "latest", "COPILOT_CLI_SHA256_" + target: ""}
            result, events, _ = self.run_consumer("copilot", legacy)
            self.assertEqual(0, result.returncode, result.stderr)
            self.assertIn("extract", events)

    def test_redirects_require_same_https_origin(self):
        handler = bundle.RegistryRedirectHandler()
        request = urllib.request.Request("https://registry.invalid/a",
                                         headers={"Authorization": "Bearer fixture"})
        redirected = handler.redirect_request(request, None, 302, "redirect", {},
                                               "https://registry.invalid/b")
        self.assertEqual("Bearer fixture", redirected.get_header("Authorization"))
        for url in ("https://other.invalid/b", "http://registry.invalid/b",
                    "https://registry.invalid:444/b", "https://user@registry.invalid/b"):
            with self.subTest(url=url), self.assertRaises(ValueError):
                handler.redirect_request(request, None, 302, "redirect", {}, url)

    def test_dockerfile_shell_syntax_and_pin_inputs(self):
        for name in (*bundle.NPM_HARNESSES, "copilot", "hermes", *bundle.OPAQUE_HARNESSES):
            text = (ROOT / f"harnesses/{name}/Dockerfile").read_text()
            lines = text.splitlines()
            index = 0
            while index < len(lines):
                line = lines[index]
                index += 1
                if not line.startswith("RUN ") or "<<" in line:
                    continue
                command = line[4:]
                while command.endswith("\\"):
                    command = command[:-1] + " " + lines[index]
                    index += 1
                command = re.sub(r"^--mount=\S+\s+", "", command)
                subprocess.run(["sh", "-n"], input=command.encode(), check=True)
            if name in bundle.OPAQUE_HARNESSES:
                prefix = bundle.OPAQUE_HARNESSES[name][0]
                self.assertIn(f"ARG {prefix}_SHA256_ARM64", text)
                self.assertLess(text.index("sha256sum -c"), text.index("tar -xzf"))
            elif name == "copilot":
                self.assertLess(text.index("sha256sum -c"), text.index("tar xzf"))
            elif name == "hermes":
                self.assertLess(text.index("sha256sum -c"), text.index('pip install --no-cache-dir', text.index("sha256sum -c")))
        for name, (_, argument) in bundle.NPM_HARNESSES.items():
            self.assertIn("${" + argument + "}", (ROOT / f"harnesses/{name}/Dockerfile").read_text())


if __name__ == "__main__":
    unittest.main()
