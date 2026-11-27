# Release image recipe assets

`generate-image-bundle.py` requires Python 3.11+ and Git, with no additional
packages. It exports committed inputs, never untracked or dirty working files.

```bash
python3 -I scripts/release/generate-image-bundle.py \
  --ref "$TAG" --output release --resolve-tools --require-complete
```

This produces `image-build-recipe.tar.gz`, `toolchain.json`, and `SHA256SUMS`.
Checksums include existing flat-directory release assets and exclude themselves.
Archive paths/modes come from Git; gzip timestamps are normalized. The same
commit and unresolved offline inventory produce identical assets on rerun.

The archive includes image-build recipes/helpers, full harness contexts (including
hidden home files), and repository-root inputs needed by the scion-base source
stage. `go.mod` and `go.sum` are included; a source checkout is unnecessary.
Release-only builds should select `SCION_BINARIES=release`.

After checking SHA256SUMS, extract into an empty directory. Core-base uses
`image-build/` as its Docker build context, scion-base uses the extracted root,
and each harness uses `harnesses/<name>/`. For example:

```bash
sha256sum -c SHA256SUMS
tar -xzf image-build-recipe.tar.gz
docker build -f image-build/core-base/Dockerfile image-build/
docker build -f image-build/scion-base/Dockerfile \
  --build-arg BASE_IMAGE=my-core-base --build-arg SCION_BINARIES=release \
  --build-arg SCION_RELEASE_TAG="$TAG" .
```

## Manifest semantics and limitations

The five required tools cover Linux amd64/arm64, the image platforms supported
by the recipe. Go/gh versions come from committed Dockerfile arguments; helm's
version and hashes come from its committed pins. Resolution reads kubectl's
stable version and golangci-lint's latest release, then streams each artifact
through SHA256 without executing it. Helm's downloads must match committed hashes.
Network errors or missing declarations fail with an exception class and reason.

Without `--resolve-tools`, generation is offline and unresolved fields are JSON
nulls. **Do not publish that as a complete tool manifest**: `--require-complete`
refuses unresolved required-tool records before writing assets.

**Unverified:** live resolver API shapes, downloads and Docker builds have not
been exercised by the offline tests. Before wiring publication, run the command
above for an actual release tag in a release sandbox and independently verify
the ten downloaded artifact hashes/versions. This is the live positive control.

The manifest is a download/recipe inventory, not an installed-image SBOM.
Go/gh floors can preserve newer tools; apt packages and some base/harness sources
float. Auxiliary entries explicitly retain unknown versions/hashes, and harness
build arguments are recorded. Existing installers do not consume JSON: downstream
builders must apply manifest pins themselves, especially for kubectl/golangci-lint.
Hashing an artifact records its bytes; it does not independently authenticate
publisher provenance. No image is built/published, no base digest is discovered,
and no image SBOM is generated. Those need a separate image-build integration.

## Workflow follow-up (maintainer-owned, not applied here)

In `.github/workflows/build-release.yml`, insert immediately after **Prepare
release assets** and before **Generate checksums**:

```yaml
      - name: Generate image recipe assets
        env:
          TAG: ${{ steps.channel.outputs.tag }}
        run: |
          python3 -I scripts/release/generate-image-bundle.py \
            --ref "$TAG" --output release --resolve-tools --require-complete
```

The existing checksum step will regenerate SHA256SUMS over every asset, and
`files: release/*` will publish the additions. Keep the existing repository guard:
this patch does not enable fork releases or change any workflow.

Offline verification: `bash scripts/release/test-generate-image-bundle.sh`.
