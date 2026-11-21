#!/usr/bin/env bash
# Usage: check-release-dist.sh <dist-dir> <tag>
# Fails unless dist holds the seven release assets and nothing but the files GoReleaser writes
# besides them, checksums.txt matches the archives, and the linux/amd64 binary reports the version
# of <tag>.
set -euo pipefail

dist=$1
tag=$2

fail() {
  echo "check-release-dist: $*" >&2
  exit 1
}

archives=(
  addition_darwin_amd64.tar.gz addition_darwin_arm64.tar.gz
  addition_linux_amd64.tar.gz addition_linux_arm64.tar.gz
  addition_windows_amd64.zip addition_windows_arm64.zip
)

allowed=(
  "${archives[@]}" checksums.txt
  CHANGELOG.md artifacts.json config.yaml metadata.json
  addition_darwin_amd64_v1 addition_darwin_arm64_v8.0 addition_linux_amd64_v1
  addition_linux_arm64_v8.0 addition_windows_amd64_v1 addition_windows_arm64_v8.0
)

for asset in "${archives[@]}" checksums.txt; do
  [ -f "$dist/$asset" ] || fail "missing $asset"
done

shopt -s dotglob nullglob
for entry in "$dist"/*; do
  name=${entry##*/}
  known=false
  for ok in "${allowed[@]}"; do
    if [ "$name" = "$ok" ]; then known=true; fi
  done
  [ "$known" = true ] || fail "unexpected file in $dist: $name"
done
shopt -u dotglob nullglob

listed=$(awk '{ print $2 }' "$dist/checksums.txt" | sort)
expected=$(printf '%s\n' "${archives[@]}" | sort)
[ "$listed" = "$expected" ] || fail "checksums.txt must list exactly the six archives"
(cd "$dist" && sha256sum --strict -c checksums.txt >/dev/null) || fail "checksums.txt does not match the archives"

scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT
tar -xzf "$dist/addition_linux_amd64.tar.gz" -C "$scratch" addition
reported=$("$scratch/addition" version </dev/null | sed -n 's/.*"version":"\([^"]*\)".*/\1/p')
[ "$reported" = "${tag#v}" ] || fail "the binary reports version '$reported', the tag is $tag"
