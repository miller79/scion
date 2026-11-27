#!/usr/bin/env python3
"""Export a committed image recipe and its declared download inventory."""

import argparse
import base64
import gzip
import hashlib
import json
import os
import re
import shlex
import subprocess
import sys
import tempfile
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path


CONTEXTS = (
    "image-build", "harnesses", "cmd", "internal", "pkg", "proto",
    "resources", "third_party", "web", "go.mod", "go.sum",
)


def git(root, *args):
    return subprocess.check_output(["git", "-C", str(root), *args])


def read(root, commit, path):
    return git(root, "show", f"{commit}:{path}").decode()


class RegistryRedirectHandler(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, response, code, message, headers, url):
        safe_url(url)
        before = urllib.parse.urlsplit(request.full_url)
        after = urllib.parse.urlsplit(url)
        if (before.hostname, before.port or 443) != (after.hostname, after.port or 443):
            raise ValueError("cross-origin registry/API redirect refused")
        return super().redirect_request(request, response, code, message, headers, url)


def fetch(url, headers=None):
    safe_url(url)
    request = urllib.request.Request(
        url, headers={"User-Agent": "scion-release-bundle", **(headers or {})})
    return urllib.request.build_opener(RegistryRedirectHandler()).open(request, timeout=120)


def safe_url(url):
    parsed = urllib.parse.urlsplit(url)
    if parsed.scheme != "https" or not parsed.netloc or parsed.username or parsed.password or parsed.fragment:
        raise ValueError("download/API URL must be HTTPS without credentials or fragments")
    return url


def concrete(version):
    if not isinstance(version, str) or not re.fullmatch(r"\d+(?:\.\d+){1,3}", version):
        raise ValueError("expected a concrete stable version, not a tag or range")
    return version


def api_json(url):
    with fetch(url) as response:
        return json.load(response)


def download_hash(url, integrity=None):
    digest = hashlib.sha256()
    verification = None
    expected = None
    if integrity:
        # npm normally publishes SHA512 SRI; accept only strong hash algorithms.
        entries = dict(part.split("-", 1) for part in integrity.split())
        algorithm = next((a for a in ("sha512", "sha384", "sha256") if a in entries), None)
        if not algorithm:
            raise ValueError("npm integrity has no supported SHA2 digest")
        verification = hashlib.new(algorithm)
        expected = base64.b64decode(entries[algorithm], validate=True)
    with fetch(url) as response:
        while chunk := response.read(1024 * 1024):
            digest.update(chunk)
            if verification:
                verification.update(chunk)
    if verification and verification.digest() != expected:
        raise ValueError("npm dist.integrity mismatch")
    return digest.hexdigest()


NPM_HARNESSES = {
    "claude": ("@anthropic-ai/claude-code", "CLAUDE_CLI_VERSION"),
    "codex": ("@openai/codex", "CODEX_CLI_VERSION"),
    "gemini-cli": ("@google/gemini-cli", "GEMINI_CLI_VERSION"),
    "opencode": ("opencode-ai", "OPENCODE_VERSION"),
    "pi": ("@earendil-works/pi-coding-agent", "PI_VERSION"),
}
OPAQUE_HARNESSES = {
    "antigravity": ("ANTIGRAVITY_CLI", "agy", "antigravity.google/cli/install.sh"),
    "grok-build": ("GROK_CLI", "grok", "x.ai/cli/install.sh; GROK_VERSION is unused"),
    "muse-code": ("MUSE_CLI", "muse", "dev.meta.ai/install.sh"),
}


def harness_inventory(harness_args, resolve, npm_registry, pypi_api, locks):
    records = []
    for name, (package, argument) in NPM_HARNESSES.items():
        args = harness_args[f"harnesses/{name}/Dockerfile"]
        declared = args.get(argument) or "latest"
        record = {
            "name": name, "package": package, "os": "linux", "arch": "any",
            "version": concrete(declared) if declared != "latest" else None,
            "url": None, "sha256": None, "integrity": None, "build_arg": argument,
            "source": "npm", "status": "unresolved",
        }
        if resolve:
            metadata = api_json(
                f"{npm_registry.rstrip('/')}/{urllib.parse.quote(package, safe='')}/{declared}")
            if metadata["name"] != package:
                raise ValueError(f"npm returned a different package for {name}")
            record["version"] = concrete(metadata["version"])
            if declared != "latest" and record["version"] != declared:
                raise ValueError(f"npm returned a different version for {name}")
            record["url"] = safe_url(metadata["dist"]["tarball"])
            record["integrity"] = metadata["dist"]["integrity"]
            if not record["integrity"]:
                raise ValueError(f"missing dist.integrity for {name}")
            record["sha256"] = download_hash(record["url"], record["integrity"])
            record["status"] = "resolved"
        records.append(record)
    args = harness_args["harnesses/copilot/Dockerfile"]
    version = args["COPILOT_CLI_VERSION"]
    if version == "latest":
        version = (api_json("https://api.github.com/repos/github/copilot-cli/releases/latest")[
            "tag_name"].removeprefix("v") if resolve else None)
    if version:
        concrete(version)
    for arch, upstream in (("amd64", "x64"), ("arm64", "arm64")):
        url = (f"https://github.com/github/copilot-cli/releases/download/v{version}/"
               f"copilot-linux-{upstream}.tar.gz") if version else None
        records.append({
            "name": "copilot", "os": "linux", "arch": arch, "version": version,
            "url": url, "sha256": download_hash(url) if resolve else None,
            "build_arg": "COPILOT_CLI_VERSION", "source": "github",
            "status": "resolved" if resolve else "unresolved",
        })
    declared = harness_args["harnesses/hermes/Dockerfile"].get("HERMES_VERSION", "")
    hermes = {
        "name": "hermes", "package": "hermes-agent", "os": "linux", "arch": "any",
        "version": concrete(declared) if declared else None,
        "url": None, "sha256": None, "build_arg": "HERMES_VERSION",
        "source": "pypi-sdist", "status": "unresolved",
    }
    if resolve:
        endpoint = f"{pypi_api.rstrip('/')}/hermes-agent"
        metadata = api_json(f"{endpoint}/{declared}/json" if declared else f"{endpoint}/json")
        hermes["version"] = concrete(metadata["info"]["version"])
        if declared and hermes["version"] != declared:
            raise ValueError("PyPI returned a different Hermes version")
        candidates = [item for item in metadata["urls"]
                      if item["packagetype"] == "sdist" and item["filename"].endswith(".tar.gz")
                      and not item.get("yanked", False)]
        if len(candidates) != 1:
            raise ValueError("Hermes requires one non-yanked .tar.gz source distribution")
        hermes["url"] = safe_url(candidates[0]["url"])
        hermes["sha256"] = download_hash(hermes["url"])
        if hermes["sha256"] != candidates[0]["digests"]["sha256"]:
            raise ValueError("Hermes distribution SHA256 mismatch")
        hermes["status"] = "resolved"
    records.append(hermes)
    if set(locks) - set(OPAQUE_HARNESSES):
        raise ValueError("operator lock contains an unknown opaque harness")
    for name, (prefix, binary, installer) in OPAQUE_HARNESSES.items():
        for arch in ("amd64", "arm64"):
            record = {
                "name": name, "os": "linux", "arch": arch, "version": None,
                "url": None, "sha256": None, "build_arg": f"{prefix}_VERSION",
                "source": "operator-tar.gz", "archive_member": binary, "status": "unresolved",
                "reason": f"opaque floating {installer}; no documented versioned artifact API supplied",
            }
            if name in locks:
                lock = locks[name]
                record["version"] = concrete(lock["version"])
                artifact = lock["linux"][arch]
                record["url"] = safe_url(artifact["url"])
                record["sha256"] = artifact["sha256"]
                if not re.fullmatch(r"[a-f0-9]{64}", record["sha256"]):
                    raise ValueError("operator lock requires a SHA256 per architecture")
                if resolve and download_hash(record["url"]) != record["sha256"]:
                    raise ValueError(f"operator lock checksum mismatch for {name}/{arch}")
                record["status"] = "resolved" if resolve else "declared"
                record.pop("reason")
            records.append(record)
    return records


def registry_digest(reference):
    if "@" in reference:
        image, selector = reference.split("@", 1)
    else:
        image = reference
        selector = "latest"
        if ":" in image.rsplit("/", 1)[-1]:
            image, selector = image.rsplit(":", 1)
    parts = image.split("/")
    if len(parts) > 1 and ("." in parts[0] or ":" in parts[0] or parts[0] == "localhost"):
        host, repository = parts[0], "/".join(parts[1:])
    else:
        host = "registry-1.docker.io"
        repository = image if "/" in image else f"library/{image}"
    url = f"https://{host}/v2/{repository}/manifests/{selector}"
    headers = {"Accept": ", ".join((
        "application/vnd.oci.image.index.v1+json",
        "application/vnd.docker.distribution.manifest.list.v2+json",
        "application/vnd.oci.image.manifest.v1+json",
        "application/vnd.docker.distribution.manifest.v2+json",
    ))}
    try:
        response = fetch(url, headers)
    except urllib.error.HTTPError as error:
        if error.code != 401:
            raise
        challenge = error.headers.get("WWW-Authenticate", "")
        error.close()
        if not challenge.lower().startswith("bearer "):
            raise ValueError("registry does not offer anonymous Bearer authentication")
        fields = dict(re.findall(r'(\w+)="([^"]*)"', challenge))
        realm = safe_url(fields["realm"])
        # Do not send a registry challenge to arbitrary external auth services.
        if urllib.parse.urlsplit(realm).netloc not in {host, "auth.docker.io"}:
            raise ValueError("unexpected registry token authority")
        query = urllib.parse.urlencode({
            "service": fields.get("service", host), "scope": f"repository:{repository}:pull",
        })
        token = api_json(f"{realm}?{query}")
        headers["Authorization"] = "Bearer " + (token.get("token") or token["access_token"])
        response = fetch(url, headers)
    with response:
        payload = response.read()
        digest = "sha256:" + hashlib.sha256(payload).hexdigest()
        advertised = response.headers.get("Docker-Content-Digest")
        if advertised and advertised != digest:
            raise ValueError("registry manifest digest mismatch")
        if selector.startswith("sha256:") and selector != digest:
            raise ValueError("registry returned different bytes for requested digest")
        metadata = json.loads(payload)
        if metadata.get("schemaVersion") != 2:
            raise ValueError("registry did not return a schemaVersion 2 manifest")
    return digest


def base_inventory(root, commit, paths, resolve):
    bases = []
    cache = {}
    for path in paths:
        args, stages = {}, set()
        for number, line in enumerate(read(root, commit, path).splitlines(), 1):
            declaration = re.match(r"ARG (\w+)=(.*)", line)
            if declaration:
                args[declaration[1]] = declaration[2]
            if not line.startswith("FROM "):
                continue
            tokens = shlex.split(line)[1:]
            if tokens[0].startswith("--platform="):
                tokens.pop(0)
            original = tokens[0]
            reference = re.sub(r"\$\{(\w+)\}|\$(\w+)",
                               lambda m: args.get(m[1] or m[2], m[0]), original)
            kind = "external"
            if reference in stages or reference == "scratch":
                kind = "stage"
            elif reference == "${BASE_IMAGE}" or reference == "$BASE_IMAGE":
                kind = "build-dependency"
            elif "$" in reference:
                kind = "unresolved-expression"
            digest = None
            if kind == "external":
                if "@sha256:" in reference:
                    digest = reference.split("@", 1)[1]
                    if not re.fullmatch(r"sha256:[a-f0-9]{64}", digest):
                        raise ValueError("invalid declared image digest")
                if resolve:
                    if reference not in cache:
                        cache[reference] = registry_digest(reference)
                    digest = cache[reference]
            argument = re.fullmatch(r"\$\{(\w+)\}|\$(\w+)", original)
            bases.append({
                "dockerfile": path, "line": number, "reference": reference,
                "digest": digest, "kind": kind,
                "build_arg": (argument[1] or argument[2]) if argument else None,
            })
            if len(tokens) >= 3 and tokens[1].lower() == "as":
                stages.add(tokens[2])
    return bases


def inventory(root, commit, resolve, npm_registry="https://registry.npmjs.org/",
              pypi_api="https://pypi.org/pypi", locks=None):
    recipe = read(root, commit, "image-build/core-base/Dockerfile")
    installer = read(root, commit, "image-build/lib/install-core-toolchain.sh")
    args = dict(re.findall(r"^ARG (\w+)=(.*)$", recipe, re.M))
    versions = {
        "go": args["GO_VERSION"], "gh": args["GH_VERSION"],
        "helm": args["HELM_VERSION"], "kubectl": None, "golangci-lint": None,
    }
    if resolve:
        with fetch("https://dl.k8s.io/release/stable.txt") as response:
            versions["kubectl"] = response.read(256).decode().strip().removeprefix("v")
        with fetch("https://api.github.com/repos/golangci/golangci-lint/releases/latest") as response:
            versions["golangci-lint"] = json.load(response)["tag_name"].removeprefix("v")
    tools = []
    for name, version in versions.items():
        if version and not re.fullmatch(r"\d+\.\d+\.\d+", version):
            raise ValueError(f"invalid {name} version")
        for arch in ("amd64", "arm64"):
            urls = {
                "go": f"https://go.dev/dl/go{version}.linux-{arch}.tar.gz",
                "gh": f"https://github.com/cli/cli/releases/download/v{version}/gh_{version}_linux_{arch}.tar.gz",
                "helm": f"https://get.helm.sh/helm-v{version}-linux-{arch}.tar.gz",
                "kubectl": f"https://dl.k8s.io/release/v{version}/bin/linux/{arch}/kubectl",
                "golangci-lint": f"https://github.com/golangci/golangci-lint/releases/download/v{version}/golangci-lint-{version}-linux-{arch}.tar.gz",
            }
            url = urls[name] if version else None
            expected = args[f"HELM_SHA256_{arch.upper()}"] if name == "helm" else None
            sha = download_hash(url) if resolve else expected
            if expected and sha != expected:
                raise ValueError(f"helm checksum mismatch for linux/{arch}")
            tools.append({
                "name": name, "os": "linux", "arch": arch,
                "version": version, "url": url, "sha256": sha,
                "policy": "exact" if name == "helm" else (
                    "minimum; keep newer installed version" if name in ("go", "gh")
                    else "latest at install time; keep existing installation"
                ),
                "status": "resolved" if version and url and sha else "unresolved",
            })
    common = re.search(r'COMMON_PACKAGES="\n(.*?)\n"', installer, re.S)
    if not common:
        raise ValueError("COMMON_PACKAGES declaration not found")
    harness_args = {}
    paths = git(root, "ls-tree", "-r", "--name-only", commit, "harnesses").decode().splitlines()
    for path in paths:
        if path.endswith("/Dockerfile"):
            harness_args[path] = dict(re.findall(r"^ARG (\w+)=(.*)$", read(root, commit, path), re.M))
    recipes = [p for p in git(root, "ls-tree", "-r", "--name-only", commit,
                             "image-build", "harnesses").decode().splitlines()
               if p.endswith("/Dockerfile")]
    return {
        "schema_version": 2, "source_commit": commit,
        "platforms": ["linux/amd64", "linux/arm64"],
        "base_images": base_inventory(root, commit, recipes, resolve), "tools": tools,
        "harness_tools": harness_inventory(harness_args, resolve, npm_registry, pypi_api, locks or {}),
        "apt": {
            "common_packages": common.group(1).split(),
            "conditional_packages": ["libcurl4t64", "libcurl4", "chromium", "google-chrome-stable"],
            "additional_packages": ["gcsfuse", "google-cloud-cli"],
            "policy": "repository candidates at build time, unpinned; preserve adequate existing tools",
            "complete_package_inventory": False,
            "source": "image-build/lib/install-core-toolchain.sh",
        },
        "unresolved_auxiliary_tools": [
            {"name": name, "version": None, "url": None, "sha256": None,
             "reason": reason}
            for name, reason in (
                ("gcsfuse", "apt candidate; keep existing installation"),
                ("gcloud", "apt candidate; keep existing installation"),
                ("Chrome/Chromium", "apt candidate with capability-based fallback"),
                ("bun", "copied from floating oven/bun image"),
                ("git", "copied from GIT_IMAGE"),
                ("chrome-devtools-mcp", "npm unpinned"),
                ("@playwright/cli", "npm latest"),
            )
        ],
        "harness_build_args": harness_args,
        "limitations": [
            "Build-input inventory/SBOM, not an installed-image scan.",
            "Apply generated core/harness build args; apt and unlocked transitive dependencies still float.",
            "BASE_IMAGE is a caller-built dependency; use the published core/scion image digest.",
            "Opaque harnesses require operator locks; never infer pins from floating installer scripts.",
            "Only Linux amd64/arm64 image toolchains are supported.",
        ],
    }


def pin_files(manifest):
    core, harness = {}, {}
    for tool in manifest["tools"]:
        if tool["status"] != "resolved":
            continue
        prefix = {"go": "GO", "gh": "GH", "helm": "HELM",
                  "kubectl": "KUBECTL", "golangci-lint": "GOLANGCI_LINT"}[tool["name"]]
        core[f"{prefix}_VERSION"] = tool["version"]
        if prefix in {"HELM", "KUBECTL", "GOLANGCI_LINT"}:
            core[f"{prefix}_SHA256_{tool['arch'].upper()}"] = tool["sha256"]
    for base in manifest["base_images"]:
        if base["dockerfile"] == "image-build/core-base/Dockerfile" and base["digest"] and base["build_arg"]:
            core[base["build_arg"]] = base["reference"].split("@")[0] + "@" + base["digest"]
    for tool in manifest["harness_tools"]:
        if tool["status"] != "resolved":
            continue
        harness[tool["build_arg"]] = tool["version"]
        prefix = tool["build_arg"].removesuffix("_VERSION")
        if tool["source"] == "npm":
            harness[f"{prefix}_URL"] = tool["url"]
            harness[f"{prefix}_SHA256"] = tool["sha256"]
            harness[f"{prefix}_INTEGRITY"] = tool["integrity"]
        elif tool["name"] == "copilot":
            harness[f"{prefix}_SHA256_{tool['arch'].upper()}"] = tool["sha256"]
        elif tool["source"] == "operator-tar.gz":
            harness[f"{prefix}_URL_{tool['arch'].upper()}"] = tool["url"]
            harness[f"{prefix}_SHA256_{tool['arch'].upper()}"] = tool["sha256"]
        elif tool["name"] == "hermes":
            harness["HERMES_URL"] = tool["url"]
            harness["HERMES_SHA256"] = tool["sha256"]
    def env(values):
        if any(re.search(r"[\r\n]", value) for value in values.values()):
            raise ValueError("build argument contains a newline")
        return "".join(f"{key}={value}\n" for key, value in sorted(values.items()))
    return env(core), env(harness)


def sbom(root, commit, manifest):
    module_text = read(root, commit, "go.mod")
    sums = {}
    for line in read(root, commit, "go.sum").splitlines():
        name, version, digest = line.split()
        sums.setdefault((name, version.removesuffix("/go.mod")), []).append(
            {"name": "scion:go:go.mod:h1" if version.endswith("/go.mod") else "scion:go:module:h1",
             "value": digest})
    components = []
    requirements = []
    in_require = False
    for line in module_text.splitlines():
        line = line.split("//", 1)[0].strip()
        if line == "require (":
            in_require = True
        elif line == ")":
            in_require = False
        elif line.startswith("require "):
            requirements.append(line.removeprefix("require ").split())
        elif in_require and line:
            requirements.append(line.split())
    for name, version in requirements:
        purl = f"pkg:golang/{urllib.parse.quote(name, safe='/')}@{version}"
        components.append({
            "type": "library", "name": name, "version": version, "purl": purl,
            "bom-ref": purl, "properties": sums.get((name, version), []),
        })
    for tool in manifest["tools"] + manifest["harness_tools"]:
        if tool["status"] != "resolved":
            continue
        identity = f"tool:{tool['name']}:{tool['version']}:{tool['os']}:{tool['arch']}"
        component = {
            "type": "application", "name": tool.get("package", tool["name"]),
            "version": tool["version"], "bom-ref": identity,
            "hashes": [{"alg": "SHA-256", "content": tool["sha256"]}],
            "externalReferences": [{"type": "distribution", "url": tool["url"]}],
            "properties": [{"name": "scion:platform", "value": f"{tool['os']}/{tool['arch']}"}],
        }
        if tool.get("integrity"):
            component["properties"].append({"name": "scion:npm:dist.integrity", "value": tool["integrity"]})
        components.append(component)
    module = re.search(r"^module (\S+)", module_text, re.M)[1]
    return {
        "bomFormat": "CycloneDX", "specVersion": "1.5", "version": 1,
        "metadata": {
            "lifecycles": [{"phase": "pre-build"}],
            "component": {"type": "application", "name": module, "version": commit},
            "properties": [{"name": "scion:sbom:scope", "value": "build inputs; not installed packages"}],
        },
        "components": sorted(components, key=lambda c: c["bom-ref"]),
    }


def check_complete(manifest):
    if any(t["status"] != "resolved" for t in manifest["tools"] + manifest["harness_tools"]):
        raise ValueError("required tools or harnesses unresolved; use --resolve-tools and opaque harness locks")
    if any(b["kind"] == "unresolved-expression" or (b["kind"] == "external" and not b["digest"])
           for b in manifest["base_images"]):
        raise ValueError("external base images unresolved; use --resolve-tools")


def generate(root, ref, output, resolve=False, require_complete=False,
             npm_registry="https://registry.npmjs.org/", pypi_api="https://pypi.org/pypi", locks=None):
    commit = git(root, "rev-parse", "--verify", f"{ref}^{{commit}}").decode().strip()
    manifest = inventory(root, commit, resolve, npm_registry, pypi_api, locks)
    if require_complete:
        check_complete(manifest)
    core_pins, harness_pins = pin_files(manifest)
    bom = sbom(root, commit, manifest)
    output.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="scion-recipe-") as temporary:
        staging = Path(temporary)
        archive = git(root, "archive", "--format=tar", commit, *CONTEXTS)
        with (staging / "image-build-recipe.tar.gz").open("wb") as stream:
            with gzip.GzipFile(filename="", mode="wb", fileobj=stream, mtime=0) as compressed:
                compressed.write(archive)
        (staging / "toolchain.json").write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n")
        (staging / "sbom.cdx.json").write_text(json.dumps(bom, indent=2, sort_keys=True) + "\n")
        (staging / "core-pins.env").write_text(core_pins)
        (staging / "harness-pins.env").write_text(harness_pins)
        for name in ("image-build-recipe.tar.gz", "toolchain.json", "sbom.cdx.json",
                     "core-pins.env", "harness-pins.env"):
            destination = output / name
            if destination.is_symlink():
                raise ValueError(f"refusing output symlink: {name}")
            (staging / name).replace(destination)
        sums = []
        for asset in sorted(output.iterdir()):
            if asset.name == "SHA256SUMS":
                continue
            if not asset.is_file() or asset.is_symlink() or re.search(r"[\r\n\\]", asset.name):
                raise ValueError(f"unsupported release asset: {asset.name!r}")
            with asset.open("rb") as stream:
                sha = hashlib.file_digest(stream, "sha256").hexdigest()
            sums.append(f"{sha}  {asset.name}\n")
        checksum = output / "SHA256SUMS"
        if checksum.is_symlink():
            raise ValueError("refusing SHA256SUMS symlink")
        checksum.write_text("".join(sums))
    return manifest


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", type=Path, default=Path(__file__).resolve().parents[2])
    parser.add_argument("--ref", default="HEAD", help="committed release tag or SHA")
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--resolve-tools", action="store_true", help="resolve/hash tools, harnesses and registry manifests; never execute")
    parser.add_argument("--require-complete", action="store_true", help="reject unresolved tools/harnesses/external bases")
    parser.add_argument("--npm-registry", default=os.environ.get("NPM_CONFIG_REGISTRY", "https://registry.npmjs.org/"))
    parser.add_argument("--pypi-api", default="https://pypi.org/pypi", help="PyPI-compatible JSON API root (mirror supported)")
    parser.add_argument("--harness-lock", type=Path, help="operator JSON locks for opaque harness archives")
    options = parser.parse_args()
    try:
        locks = json.loads(options.harness_lock.read_text()) if options.harness_lock else {}
        manifest = generate(options.repo, options.ref, options.output, options.resolve_tools,
                            options.require_complete, options.npm_registry, options.pypi_api, locks)
        print(f"Generated recipe and manifest for {manifest['source_commit']}")
    except (OSError, ValueError, KeyError, TypeError, subprocess.CalledProcessError) as error:
        print(f"{type(error).__name__}: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
