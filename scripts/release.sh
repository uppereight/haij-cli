#!/usr/bin/env bash
# Builds one release into <outdir>:
#
#   haij-<version>-<os>-<arch>.tar.gz   darwin/linux × arm64/amd64, `haij` at the root
#   SHA256SUMS                          one line per archive
#   haij.rb                             the Homebrew formula for exactly these archives
#
# .github/workflows/release.yml runs it on a v* tag; it runs the same way
# locally (the archives' checksums differ from CI's, so never commit a
# locally rendered formula):
#
#   scripts/release.sh 0.1.0 "$(git rev-parse --short HEAD)" dist
set -euo pipefail

usage="usage: scripts/release.sh <version, no leading v> <commit> <outdir>"
version=${1:?$usage}
commit=${2:?$usage}
out=${3:?$usage}
if [[ ! $version =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$ ]]; then
  echo "release.sh: version '$version' is not semver without a leading v" >&2
  exit 2
fi

root=$(cd "$(dirname "$0")/.." && pwd)
mkdir -p "$out"
out=$(cd "$out" && pwd)
work="$out/.build"
rm -rf "$work"
trap 'rm -rf "$work"' EXIT
export COPYFILE_DISABLE=1 # no macOS ._ resource files in the tarballs

for target in darwin/arm64 darwin/amd64 linux/arm64 linux/amd64; do
  os=${target%/*}
  arch=${target#*/}
  name="haij-$version-$os-$arch"
  mkdir -p "$work/$name"
  (cd "$root" && CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
    -ldflags "-s -w -X main.version=$version -X main.commit=$commit" \
    -o "$work/$name/haij" ./cmd/haij)
  tar -C "$work/$name" -czf "$out/$name.tar.gz" haij
  echo "built $name.tar.gz"
done

(cd "$out" && shasum -a 256 haij-"$version"-*.tar.gz > SHA256SUMS)
"$root/scripts/render-formula.sh" "$version" "$out/SHA256SUMS" > "$out/haij.rb"

# Smoke-test the archive this machine can run: it must report the version.
host="haij-$version-$(go env GOOS)-$(go env GOARCH)"
if [[ -f "$out/$host.tar.gz" ]]; then
  mkdir -p "$work/smoke"
  tar -C "$work/smoke" -xzf "$out/$host.tar.gz"
  got=$("$work/smoke/haij" --version)
  if [[ $got != "haij $version ($commit) "* ]]; then
    echo "release.sh: $host.tar.gz reports '$got', want 'haij $version ($commit) …'" >&2
    exit 1
  fi
  echo "smoke: $got"
fi
