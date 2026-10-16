#!/usr/bin/env bash
# Generate runtime dependency notices for exactly the CGO-disabled release
# targets. The tool is installed for the host, then inspects each target graph.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

targets=("$@")
if [[ ${#targets[@]} -eq 0 ]]; then
  targets=(linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64)
fi
for target in "${targets[@]}"; do
  case "$target" in
    linux/amd64|linux/arm64|darwin/amd64|darwin/arm64|windows/amd64|windows/arm64) ;;
    *) echo "unsupported license target: $target" >&2; exit 1 ;;
  esac
done

# Only replace a bundle this script owns, never a user-provided directory or
# symlink. Stage everything first so failed generation preserves the old one.
marker='roast runtime notices: v1'
if [[ -e THIRD_PARTY_LICENSES || -L THIRD_PARTY_LICENSES ]]; then
  if [[ -L THIRD_PARTY_LICENSES || ! -f THIRD_PARTY_LICENSES/.roast-generated ]]; then
    echo 'refusing to replace an unowned THIRD_PARTY_LICENSES directory' >&2
    exit 1
  fi
  case "$(<THIRD_PARTY_LICENSES/.roast-generated)" in
    "$marker"|'roast runtime notices: go-licenses/v2 v2.0.1') ;;
    *) echo 'refusing to replace an unowned THIRD_PARTY_LICENSES directory' >&2; exit 1 ;;
  esac
fi

license_work=$(mktemp -d "${TMPDIR:-/tmp}/roast-licenses.XXXXXXXX")
trap 'rm -r -- "$license_work"' EXIT
mkdir -p "$license_work/bin" "$license_work/bundle"
env GOOS="$(go env GOHOSTOS)" GOARCH="$(go env GOHOSTARCH)" \
  go build -modfile=.github/tools/licenses/go.mod -o "$license_work/bin/go-licenses" \
  github.com/google/go-licenses/v2
license_tool="$license_work/bin/go-licenses"
allowed='Apache-2.0,BSD-2-Clause,BSD-3-Clause,ISC,MIT,MPL-2.0,Unlicense'

for target in "${targets[@]}"; do
  goos=${target%/*}
  goarch=${target#*/}
  printf 'Generating runtime notices for %s\n' "$target"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" "$license_tool" check \
    ./cmd/roast ./cmd/roast-broker --ignore github.com/bluesky-social/roast \
    --allowed_licenses "$allowed"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" "$license_tool" save \
    ./cmd/roast ./cmd/roast-broker --ignore github.com/bluesky-social/roast \
    --save_path "$license_work/$goos-$goarch"
  # Module-cache licenses are read-only. Normalize only the generated copies
  # so repeated notices from the next target can be merged on macOS too.
  chmod -R u+rwX "$license_work/$goos-$goarch"
  cp -R "$license_work/$goos-$goarch/." "$license_work/bundle/"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" "$license_tool" report \
    ./cmd/roast ./cmd/roast-broker --ignore github.com/bluesky-social/roast \
    > "$license_work/bundle/$goos-$goarch.csv"
done

# The Go runtime and standard library are also part of static binaries.
go_root=$(go env GOROOT)
go_license="$go_root/LICENSE"
if [[ ! -f "$go_license" && -f "$go_root/../LICENSE" ]]; then
  # Homebrew keeps Go's LICENSE above its libexec GOROOT.
  go_license="$go_root/../LICENSE"
fi
if [[ ! -f "$go_license" ]]; then
  echo "cannot find the installed Go toolchain's LICENSE" >&2
  exit 1
fi
cp "$go_license" "$license_work/bundle/Go-LICENSE"
if [[ -f "$go_root/PATENTS" ]]; then
  cp "$go_root/PATENTS" "$license_work/bundle/Go-PATENTS"
fi
chmod -R u+rwX "$license_work/bundle"
printf '%s\n' "$marker" > "$license_work/bundle/.roast-generated"
if [[ -d THIRD_PARTY_LICENSES ]]; then
  rm -r -- THIRD_PARTY_LICENSES
fi
mv "$license_work/bundle" THIRD_PARTY_LICENSES
