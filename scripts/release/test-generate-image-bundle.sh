#!/usr/bin/env bash
# Offline producer-to-consumer checks; no package installation or downloads.
set -euo pipefail
root="$(git rev-parse --show-toplevel)"
ref="${SCION_BUNDLE_TEST_REF:-HEAD}"
python3 -I - "$root" "$ref" <<'PY'
import gzip
import hashlib
import importlib.util
import json
import subprocess
import sys
import tarfile
import tempfile
from pathlib import Path

root = Path(sys.argv[1])
ref = sys.argv[2]
spec = importlib.util.spec_from_file_location("bundle", root / "scripts/release/generate-image-bundle.py")
bundle = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bundle)
with tempfile.TemporaryDirectory(prefix="scion-bundle-test-") as temporary:
    output = Path(temporary) / "release"
    output.mkdir()
    (output / "scion-linux-amd64.tar.gz").write_bytes(b"existing release asset")
    manifest = bundle.generate(root, ref, output)
    first = {p.name: p.read_bytes() for p in output.iterdir()}
    with tarfile.open(output / "image-build-recipe.tar.gz") as archive:
        names = archive.getnames()
        tracked = subprocess.check_output(
            ["git", "-C", str(root), "ls-tree", "-r", "--name-only", ref, *bundle.CONTEXTS]
        ).decode().splitlines()
        for name in tracked:
            member = archive.getmember(name)
            expected = subprocess.check_output(["git", "-C", str(root), "show", f"{ref}:{name}"])
            if member.isfile():
                assert archive.extractfile(member).read() == expected, name
            elif member.issym():
                assert member.linkname.encode() == expected, name
            else:
                raise AssertionError(name)
        assert "harnesses/copilot/home/.copilot/settings.json" in names
        assert "harnesses/claude/init-firewall.sh" in names
        assert "harnesses/pi/package-lock.json" in names
        assert "go.mod" in names and "go.sum" in names
        assert archive.getmember("image-build/lib/install-core-toolchain.sh").mode & 0o111
        assert not any(".git/" in name for name in names)
    print("PASS committed recipe/context bytes, hidden files, modes and module inputs")
    assert json.loads((output / "toolchain.json").read_text()) == manifest
    assert len(manifest["tools"]) == 10
    assert len(manifest["harness_tools"]) == 14
    assert json.loads((output / "sbom.cdx.json").read_text())["specVersion"] == "1.5"
    assert "harness-pins.env" in first and "core-pins.env" in first
    assert all(t["sha256"] for t in manifest["tools"] if t["name"] == "helm")
    assert any(t["status"] == "unresolved" for t in manifest["tools"])
    before = dict(first)
    try:
        bundle.generate(root, ref, output, require_complete=True)
        raise AssertionError("incomplete manifest accepted")
    except ValueError as error:
        assert "unresolved" in str(error)
    assert {p.name: p.read_bytes() for p in output.iterdir()} == before
    print("PASS manifest round trip and incomplete-publication refusal without output changes")
    bundle.generate(root, ref, output)
    assert {p.name: p.read_bytes() for p in output.iterdir()} == first
    subprocess.run(["sha256sum", "-c", "SHA256SUMS"], cwd=output, check=True)
    assert "SHA256SUMS" not in (output / "SHA256SUMS").read_text()
    print("PASS deterministic rerun and checksum coverage of existing/new assets")
PY
python3 -I "$root/scripts/release/test-image-bundle-resolvers.py" "$root"
bash "$root/image-build/lib/test-install-core-pins.sh"
