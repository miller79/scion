# Release image recipe assets

`generate-image-bundle.py` requires Python 3.11+ and Git, with no additional
packages. It exports committed inputs, never untracked or dirty working files.
Offline generation needs no network:

```bash
python3 -I scripts/release/generate-image-bundle.py --ref "$TAG" --output release
```

Assets: `image-build-recipe.tar.gz`, `toolchain.json`, `core-pins.env`,
`harness-pins.env`, `sbom.cdx.json`, and `SHA256SUMS`. Checksums cover every
flat-directory release asset, including existing binaries, and exclude themselves.
Git paths/modes are preserved; gzip timestamps are normalized. Repeating the
same committed inputs and resolved metadata produces identical assets.

The archive includes image recipes/helpers, full harness contexts (including
hidden home files), repository-root source-stage COPY inputs, and go.mod/go.sum.
Release-only builds select `SCION_BINARIES=release`. Core-base's context is
`image-build/`, scion-base's is the extracted root, and a harness's is
`harnesses/<name>/`. Check SHA256SUMS before extracting into an empty directory.

## Manifest and pin consumption

Schema version 2 records ten Linux amd64/arm64 core download records, thirteen
harness records, and every recipe Dockerfile's FROM reference. npm/PyPI source
archives have `arch: any`; opaque binary archives have separate architecture
records. Concrete versions, URLs and SHA256 identify downloaded inputs, not tags
such as `latest`. npm also records and verifies `dist.integrity`.

An npm pin covers the package archive itself. Several harness CLIs (Claude Code,
Codex, Copilot, OpenCode) ship their binary in a per-platform optional
dependency, which npm fetches from the registry during the install. npm checks
it against the registry's integrity data, but the manifest does not record or
pin it.

`--resolve-tools` reads kubectl stable.txt, GitHub release metadata, npm package
version metadata, PyPI JSON metadata, and registry manifests; it downloads/hashes
artifacts but never executes them. Go/gh/helm and already-pinned Copilot/Pi
versions come from the committed Dockerfiles, rather than silently upgrading
them. Helm must match its committed hashes; PyPI distributions must match the
advertised SHA256. `--npm-registry` and `--pypi-api` select compatible mirrors;
URLs must be HTTPS without embedded credentials. Mirror API compatibility and
authenticated mirror access are **Unverified**; use an approved accessible
mirror/proxy, never put credentials in URLs or build arguments.

`core-pins.env` contains the manifest-derived Go/gh/helm build args, kubectl and
golangci-lint version/SHA256 args, and core-base FROM image overrides.
`harness-pins.env` contains version build args, npm package URL/SHA256/integrity
args (Copilot included), Hermes source URL/SHA256, and any explicitly locked
opaque archive URL/SHA256s.
They are **data, not shell scripts**: read key/value lines into `--build-arg`
arguments; do not source/eval them. Only resolved records are emitted.

The installer accepts `KUBECTL_VERSION`, `KUBECTL_SHA256_AMD64/ARM64`,
`GOLANGCI_LINT_VERSION`, and `GOLANGCI_LINT_SHA256_AMD64/ARM64`. Supply a concrete
version plus the hash for the target architecture. Pins bypass existing-tool
skips and verify before extraction/install. Partial/malformed pins or hash
mismatches fail. With no pin inputs, the original existing-tool skip,
kubectl-stable download, and golangci-lint installer paths are unchanged.
Go/gh retain their existing minimum-version behavior; helm remains an exact pin.

External FROM tags resolve to SHA256 of registry manifest/index bytes (verified
against Docker-Content-Digest when supplied). Index digests cover multi-platform
manifests. Local stage aliases and unbound `${BASE_IMAGE}` are marked `stage` or
`build-dependency`, not falsely resolved registry images. Downstream must supply
the built core/scion base image; publication below supplies a core-base digest.
Literal FROM references outside core-base are inventoried but may need an
operator recipe override; the core-base BUN_IMAGE/NODE_IMAGE/GIT_IMAGE args
consume its generated digest pins.

Offline unknowns are nulls. `--require-complete` rejects unresolved core tools,
harnesses and external FROM images **before writing output**. It excludes
intentional local stages/build dependencies, not unresolved external tags.

### Opaque harnesses: explicit limitation and operator lock

Antigravity executes `antigravity.google/cli/install.sh`, Grok executes
`x.ai/cli/install.sh` (its GROK_VERSION ARG is unused), and Muse executes
`dev.meta.ai/install.sh`. No documented versioned CLI artifact API was supplied.
They remain unresolved even with `--resolve-tools`; **do not claim all harnesses
pinned** or publish a complete manifest without operator-provided locks.

`--harness-lock /absolute/path/harness-lock.json` accepts this structure for
each of `antigravity`, `grok-build`, and `muse-code`:

```json
{
  "antigravity": {
    "version": "1.2.3",
    "linux": {
      "amd64": {"url": "https://YOUR-MIRROR/agy-amd64.tar.gz", "sha256": "REPLACE_WITH_64_HEX_DIGITS"},
      "arm64": {"url": "https://YOUR-MIRROR/agy-arm64.tar.gz", "sha256": "REPLACE_WITH_64_HEX_DIGITS"}
    }
  }
}
```

These are operator-selected immutable artifacts, not inferred vendor pins.
Each tar.gz must contain the executable at its root: `agy`, `grok`, or `muse`.
The operator must verify that executable's version and runtime dependencies.
The generator verifies both architecture downloads against the supplied hashes
in resolved mode; offline declarations are not considered complete. Dockerfiles
consume ANTIGRAVITY_CLI/GROK_CLI/MUSE_CLI version, URL and SHA256 args.
Without those args their original upstream installers are unchanged.

## SBOM scope

`sbom.cdx.json` is a **CycloneDX 1.5 build-input SBOM**, with lifecycle `pre-build`,
not an installed-package scan of a built image. It includes go.mod requirements
with matching go.sum h1 values as properties, and resolved core/harness tools
with artifact SHA256 hashes. Go h1 is a directory-content checksum, **not** a
tarball SHA256, and is never relabeled as one. Offline unresolved tools are
omitted from SBOM components but remain visible in toolchain.json.

It does not inventory apt packages, npm/PyPI transitive dependencies (except
Pi's separately bundled lockfile), dynamically downloaded runtime content, or
the actual installed packages. npm CLIs pin the top-level archive URL, SHA256
and SHA2 SRI through `<CLI_PREFIX>_URL`, `_SHA256`, `_INTEGRITY` build args
alongside the version. Supplying any archive input requires all inputs and a
concrete version; the shared base-image helper downloads and checks both hashes
before installing the local verified tarball. No archive inputs retain legacy
package/version installs (OpenCode without a version stays unversioned).
Pi's default lockfile/npm ci path is unchanged; explicit archive pins instead
select verified local installation with its existing ignore-scripts flags.
Rebuild core-base or thick-prep to include the helper before using archive pins.
Hermes pins
the source distribution, while its build/transitive dependencies still resolve.
Hashing bytes is not independent publisher authentication. Fully reproducible
images require those remaining inputs to be locked too.

Copilot is now installed from npm. In bundle pin mode, its top-level archive
is pinned by version, URL, SHA256 and SRI integrity through
`image-build/lib/install-pinned-npm.sh`. Its platform-specific packages and
transitive npm dependencies are fetched by npm and are **not hash-locked by
the bundle**. The former per-architecture Copilot archive hashes were removed;
this is a known limitation.

Resolver HTTP redirects must remain HTTPS on the same host and port; mirrors
that redirect to a different artifact host must supply that host's final URL
rather than an API redirect. Authorization headers never cross this boundary.

For an image-level SBOM, Anthony may add this optional command **after** image
publication and **before** the existing checksum step, using a separately
approved/pinned Syft installation:

```bash
syft "$(cat release/core-base-image.txt)" \
  -o cyclonedx-json=release/sbom.image.cdx.json
```

## Exact release sandbox positive control (not run here)

**Unverified:** live APIs, downloaded artifacts, mirror compatibility, opaque
archive layouts, and Docker builds. Offline protocol-shaped fixtures are not
live response captures. Before publication, set TAG to a release containing
these changes and HARNESS_LOCK to reviewed locks for all three opaque harnesses.
Use approved package mirrors (Artifactory example below); the PyPI root must
provide the documented `/hermes-agent[/version]/json` API.

```bash
python3 -I scripts/release/generate-image-bundle.py \
  --ref "$TAG" --output release --resolve-tools --require-complete \
  --npm-registry https://artifactory-prd.jbhunt.com/artifactory/api/npm/npm-repos \
  --pypi-api https://artifactory-prd.jbhunt.com/artifactory/api/pypi/pypi-repos/pypi \
  --harness-lock "$HARNESS_LOCK"
(cd release && sha256sum -c SHA256SUMS)
python3 -I - release/toolchain.json release/sbom.cdx.json <<'PY'
import json, sys
m, b = [json.load(open(p)) for p in sys.argv[1:]]
assert len(m["tools"]) == 10 and len(m["harness_tools"]) == 13
assert all(t["status"] == "resolved" for t in m["tools"] + m["harness_tools"])
assert all(x["digest"] for x in m["base_images"] if x["kind"] == "external")
assert b["bomFormat"] == "CycloneDX" and b["specVersion"] == "1.5"
print("PASS complete core/harness/base inputs and pre-build SBOM")
PY
recipe="$(mktemp -d)"
tar -xzf release/image-build-recipe.tar.gz -C "$recipe"
core_args=(); harness_args=()
while IFS='=' read -r key value; do core_args+=(--build-arg "$key=$value"); done < release/core-pins.env
while IFS='=' read -r key value; do harness_args+=(--build-arg "$key=$value"); done < release/harness-pins.env
for arch in amd64 arm64; do
  docker build --platform "linux/$arch" "${core_args[@]}" \
    -t "scion-core-sandbox:$arch" -f "$recipe/image-build/core-base/Dockerfile" "$recipe/image-build"
  docker build --platform "linux/$arch" --build-arg "BASE_IMAGE=scion-core-sandbox:$arch" \
    --build-arg SCION_BINARIES=release --build-arg "SCION_RELEASE_TAG=$TAG" \
    -t "scion-base-sandbox:$arch" -f "$recipe/image-build/scion-base/Dockerfile" "$recipe"
  for harness in claude codex gemini-cli opencode pi copilot hermes antigravity grok-build muse-code; do
    docker build --platform "linux/$arch" "${harness_args[@]}" \
      --build-arg "BASE_IMAGE=scion-base-sandbox:$arch" \
      -t "scion-$harness-sandbox:$arch" -f "$recipe/harnesses/$harness/Dockerfile" "$recipe/harnesses/$harness"
  done
done
```

The runner needs native workers or approved QEMU support for both architectures,
approved mirror configuration/build secrets, and release binaries at TAG.
Verify installed CLI versions in each built image against the manifest,
independently check artifact hashes/provenance, capture sanitized real API
responses for regression fixtures, and validate SBOM against the 1.5 schema.
No automatic image/container cleanup is prescribed on shared Docker hosts.

## Workflow follow-up (maintainer-owned; no CI files changed)

Insert immediately after **Prepare release assets** and before **Generate
checksums**. Configure the job's nonsecret HARNESS_LOCK path and approved package
mirror URLs; the lock must exist on the runner. Keep the existing repository
guard and `files: release/*` upload. This documents wiring, not an applied job.

```yaml
      - name: Generate image recipe assets
        env:
          TAG: ${{ steps.channel.outputs.tag }}
        run: |
          python3 -I scripts/release/generate-image-bundle.py \
            --ref "$TAG" --output release --resolve-tools --require-complete \
            --npm-registry "$NPM_REGISTRY" --pypi-api "$PYPI_JSON_API" \
            --harness-lock "$HARNESS_LOCK"
```

To satisfy the published-core-base nice-to-have, Anthony must also apply the
following steps **before Generate checksums**, after approved registry permission
and image naming. Set job permissions to `contents: read` and `packages: write`;
CORE_IMAGE below is an example upstream target, not authorization to publish.
Pin action revisions according to the release job's action policy.

```yaml
      - name: Prepare exported core-base context and args
        id: core-inputs
        env:
          CORE_IMAGE: ghcr.io/googlecloudplatform/scion-core-base
        run: |
          (cd release && sha256sum -c SHA256SUMS)
          mkdir -p "$RUNNER_TEMP/scion-image-recipe"
          tar -xzf release/image-build-recipe.tar.gz -C "$RUNNER_TEMP/scion-image-recipe"
          {
            echo "image=$CORE_IMAGE"
            echo 'args<<SCION_CORE_PINS'
            cat release/core-pins.env
            echo 'SCION_CORE_PINS'
          } >> "$GITHUB_OUTPUT"
      - uses: docker/setup-qemu-action@v4
      - uses: docker/setup-buildx-action@v4
      - uses: docker/login-action@v4
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}
      - name: Publish core-base
        id: core-base
        uses: docker/build-push-action@v7
        with:
          context: ${{ runner.temp }}/scion-image-recipe/image-build
          file: ${{ runner.temp }}/scion-image-recipe/image-build/core-base/Dockerfile
          platforms: linux/amd64,linux/arm64
          build-args: ${{ steps.core-inputs.outputs.args }}
          push: true
          tags: ${{ steps.core-inputs.outputs.image }}:${{ steps.channel.outputs.tag }}
      - name: Record immutable core-base reference
        env:
          CORE_IMAGE: ${{ steps.core-inputs.outputs.image }}
          CORE_DIGEST: ${{ steps.core-base.outputs.digest }}
        run: |
          [[ "$CORE_DIGEST" =~ ^sha256:[a-f0-9]{64}$ ]]
          printf '%s@%s\n' "$CORE_IMAGE" "$CORE_DIGEST" > release/core-base-image.txt
```

The existing checksum step must run **after** publication/optional image SBOM
to cover core-base-image.txt and any image SBOM in addition to the five generated
assets and existing binaries. This patch builds/publishes no image itself.

Offline verification: `bash scripts/release/test-generate-image-bundle.sh`
(archive/checksum checks, stdlib protocol tests, and both installer paths).

Primary contracts (accessed 2026-11-27): [npm metadata](https://github.com/npm/registry/blob/main/docs/responses/package-metadata.md),
[PyPI JSON](https://github.com/pypi/warehouse/blob/main/docs/user/api/json.md),
[OCI Distribution](https://github.com/opencontainers/distribution-spec/blob/main/spec.md),
[registry auth](https://distribution.github.io/distribution/spec/auth/token/),
[CycloneDX 1.5](https://cyclonedx.org/docs/1.5/json/), and
[build-push-action path context/digest output](https://github.com/docker/build-push-action/blob/main/README.md).
