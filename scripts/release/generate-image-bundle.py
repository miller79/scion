#!/usr/bin/env python3
"""Export a committed image recipe and its declared download inventory."""

import argparse
import gzip
import hashlib
import json
import re
import subprocess
import sys
import tempfile
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


def fetch(url):
    request = urllib.request.Request(url, headers={"User-Agent": "scion-release-bundle"})
    return urllib.request.urlopen(request, timeout=120)


def download_hash(url):
    digest = hashlib.sha256()
    with fetch(url) as response:
        while chunk := response.read(1024 * 1024):
            digest.update(chunk)
    return digest.hexdigest()


def inventory(root, commit, resolve):
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
    bases = []
    for reference in re.findall(r"^FROM (\S+)", recipe, re.M):
        reference = reference.replace("${GIT_IMAGE}", args["GIT_IMAGE"])
        bases.append({
            "reference": reference,
            "digest": reference.split("@", 1)[1] if "@sha256:" in reference else None,
        })
    harness_args = {}
    paths = git(root, "ls-tree", "-r", "--name-only", commit, "harnesses").decode().splitlines()
    for path in paths:
        if path.endswith("/Dockerfile"):
            harness_args[path] = dict(re.findall(r"^ARG (\w+)=(.*)$", read(root, commit, path), re.M))
    return {
        "schema_version": 1, "source_commit": commit,
        "platforms": ["linux/amd64", "linux/arm64"],
        "base_images": bases, "tools": tools,
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
            "Download inventory, not an installed-image SBOM.",
            "Existing installers do not consume this manifest; downstream must apply pins.",
            "Floating base, apt and harness tools remain unresolved.",
            "Only Linux amd64/arm64 image toolchains are supported.",
        ],
    }


def generate(root, ref, output, resolve=False, require_complete=False):
    commit = git(root, "rev-parse", "--verify", f"{ref}^{{commit}}").decode().strip()
    manifest = inventory(root, commit, resolve)
    if require_complete and any(tool["status"] != "resolved" for tool in manifest["tools"]):
        raise ValueError("required tools unresolved; use --resolve-tools for release publication")
    output.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="scion-recipe-") as temporary:
        staging = Path(temporary)
        archive = git(root, "archive", "--format=tar", commit, *CONTEXTS)
        with (staging / "image-build-recipe.tar.gz").open("wb") as stream:
            with gzip.GzipFile(filename="", mode="wb", fileobj=stream, mtime=0) as compressed:
                compressed.write(archive)
        (staging / "toolchain.json").write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n")
        for name in ("image-build-recipe.tar.gz", "toolchain.json"):
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
    parser.add_argument("--resolve-tools", action="store_true", help="download/hash required tools; never execute")
    parser.add_argument("--require-complete", action="store_true", help="reject unresolved required tool records")
    options = parser.parse_args()
    try:
        manifest = generate(options.repo, options.ref, options.output, options.resolve_tools, options.require_complete)
        print(f"Generated recipe and manifest for {manifest['source_commit']}")
    except (OSError, ValueError, KeyError, subprocess.CalledProcessError) as error:
        print(f"{type(error).__name__}: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
