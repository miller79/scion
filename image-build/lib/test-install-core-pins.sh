#!/usr/bin/env bash
# Hermetic tests: only downloads and final system installation are substituted.
set -euo pipefail
root="$(cd "$(dirname "$0")/../.." && pwd)"
source "$root/image-build/lib/install-core-toolchain.sh"
temporary="$(mktemp -d)"
trap 'rm -r "$temporary"' EXIT
unset KUBECTL_VERSION KUBECTL_SHA256_AMD64 KUBECTL_SHA256_ARM64
unset GOLANGCI_LINT_VERSION GOLANGCI_LINT_SHA256_AMD64 GOLANGCI_LINT_SHA256_ARM64

if [ "$#" -eq 3 ]; then
  pins="$1"; kubectl_file="$2"; lint_file="$3"
  while IFS='=' read -r key value; do
    case "$key" in
    KUBECTL_VERSION) KUBECTL_VERSION="$value" ;;
    KUBECTL_SHA256_AMD64) KUBECTL_SHA256_AMD64="$value" ;;
    KUBECTL_SHA256_ARM64) KUBECTL_SHA256_ARM64="$value" ;;
    GOLANGCI_LINT_VERSION) GOLANGCI_LINT_VERSION="$value" ;;
    GOLANGCI_LINT_SHA256_AMD64) GOLANGCI_LINT_SHA256_AMD64="$value" ;;
    GOLANGCI_LINT_SHA256_ARM64) GOLANGCI_LINT_SHA256_ARM64="$value" ;;
    esac
  done < "$pins"
else
  kubectl_file="$temporary/kubectl"
  printf 'pinned kubectl\n' > "$kubectl_file"
  mkdir -p "$temporary/golangci-lint-1.2.3-linux-amd64" "$temporary/golangci-lint-1.2.3-linux-arm64"
  printf 'pinned lint\n' > "$temporary/golangci-lint-1.2.3-linux-amd64/golangci-lint"
  cp "$temporary/golangci-lint-1.2.3-linux-amd64/golangci-lint" \
    "$temporary/golangci-lint-1.2.3-linux-arm64/golangci-lint"
  lint_file="$temporary/lint.tar.gz"
  tar -czf "$lint_file" -C "$temporary" golangci-lint-1.2.3-linux-amd64 golangci-lint-1.2.3-linux-arm64
  KUBECTL_VERSION=1.2.3
  KUBECTL_SHA256_AMD64="$(sha256sum "$kubectl_file" | awk '{print $1}')"
  KUBECTL_SHA256_ARM64="$KUBECTL_SHA256_AMD64"
  GOLANGCI_LINT_VERSION=1.2.3
  GOLANGCI_LINT_SHA256_AMD64="$(sha256sum "$lint_file" | awk '{print $1}')"
  GOLANGCI_LINT_SHA256_ARM64="$GOLANGCI_LINT_SHA256_AMD64"
fi

for architecture in amd64 arm64; do
  (
    deb_arch() { printf '%s\n' "$architecture"; }
    curl() {
      local url="$2" destination="$4"
      printf '%s\n' "$url" >> "$temporary/urls"
      case "$url" in
      */kubectl) cp "$kubectl_file" "$destination" ;;
      */golangci-lint-*.tar.gz) cp "$lint_file" "$destination" ;;
      *) echo "unexpected URL" >&2; return 1 ;;
      esac
    }
    install() {
      test -s "$7"
      printf '%s\n' "$8" >> "$temporary/installed"
    }
    step_kubectl
    step_golangci_lint
    grep -q "/linux/$architecture/kubectl" "$temporary/urls"
    grep -q "golangci-lint-${GOLANGCI_LINT_VERSION}-linux-$architecture.tar.gz" "$temporary/urls"
  )
done
test "$(wc -l < "$temporary/installed")" -eq 4
test "$(grep -cx '/usr/local/bin/kubectl' "$temporary/installed")" -eq 2
test "$(grep -cx '/usr/local/bin/golangci-lint' "$temporary/installed")" -eq 2
echo "PASS exact pins both architectures, SHA256 before extraction and installation"

for tool in kubectl golangci-lint; do
for failure in partial malformed mismatch download extraction; do
  if [ "$tool" = kubectl ] && [ "$failure" = extraction ]; then continue; fi
  (
    deb_arch() { echo amd64; }
    curl() {
      if [ "$failure" = download ]; then return 9; fi
      if [ "$tool" = kubectl ] || [ "$failure" = extraction ]; then
        cp "$kubectl_file" "$4"
      else
        cp "$lint_file" "$4"
      fi
    }
    install() { echo "unexpected install" >> "$temporary/refused"; }
    case "$tool/$failure" in
    kubectl/partial) KUBECTL_VERSION="" ;;
    kubectl/malformed) KUBECTL_VERSION=latest ;;
    kubectl/mismatch) KUBECTL_SHA256_AMD64="$(printf '%064d' 0)" ;;
    golangci-lint/partial) GOLANGCI_LINT_VERSION="" ;;
    golangci-lint/malformed) GOLANGCI_LINT_VERSION=latest ;;
    golangci-lint/mismatch) GOLANGCI_LINT_SHA256_AMD64="$(printf '%064d' 0)" ;;
    golangci-lint/extraction) GOLANGCI_LINT_SHA256_AMD64="$KUBECTL_SHA256_AMD64" ;;
    esac
    if [ "$tool" = kubectl ]; then
      if step_kubectl; then echo "failure accepted: $failure" >&2; exit 1; fi
    else
      if step_golangci_lint; then echo "failure accepted: $failure" >&2; exit 1; fi
    fi
  )
done
done
test ! -e "$temporary/refused"
echo "PASS both tools reject partial/floating pins, hash/download/extraction failures before installation"

(
  unset KUBECTL_VERSION KUBECTL_SHA256_AMD64 KUBECTL_SHA256_ARM64
  unset GOLANGCI_LINT_VERSION GOLANGCI_LINT_SHA256_AMD64 GOLANGCI_LINT_SHA256_ARM64
  command() {
    if [ "${1:-}" = -v ] && { [ "${2:-}" = kubectl ] || [ "${2:-}" = golangci-lint ]; }; then
      printf '/existing/%s\n' "$2"
    else builtin command "$@"; fi
  }
  curl() { echo "legacy skip downloaded" >&2; exit 1; }
  step_kubectl
  step_golangci_lint
)
(
  unset KUBECTL_VERSION KUBECTL_SHA256_AMD64 KUBECTL_SHA256_ARM64
  unset GOLANGCI_LINT_VERSION GOLANGCI_LINT_SHA256_AMD64 GOLANGCI_LINT_SHA256_ARM64
  command() {
    if [ "${1:-}" = -v ] && { [ "${2:-}" = kubectl ] || [ "${2:-}" = golangci-lint ]; }; then
      return 1
    else builtin command "$@"; fi
  }
  deb_arch() { echo arm64; }
  curl() {
    case "$*" in
    *stable.txt*) echo v1.2.3 ;;
    *HEAD/install.sh*) echo fixture-installer ;;
    *) printf '%s\n' "$*" >> "$temporary/legacy-urls" ;;
    esac
  }
  install() { printf '%s\n' "$*" >> "$temporary/legacy-install"; }
  rm() { :; }
  sh() { cat > "$temporary/legacy-script"; printf '%s\n' "$*" > "$temporary/legacy-sh"; }
  step_kubectl
  step_golangci_lint
  grep -q 'v1.2.3/bin/linux/arm64/kubectl' "$temporary/legacy-urls"
  grep -q '/tmp/kubectl /usr/local/bin/kubectl' "$temporary/legacy-install"
  grep -q 'fixture-installer' "$temporary/legacy-script"
  grep -q -- '-s -- -b /usr/local/bin' "$temporary/legacy-sh"
)
echo "PASS no-pin existing skips and original stable/latest installation paths unchanged"
