#!/bin/sh
# Installs the addition CLI from its GitHub release: no prompts, no sudo, no shell rc edits.
#   curl -fsSL https://raw.githubusercontent.com/runagent-dev/addition/main/install.sh | sh
# Environment:
#   ADDITION_VERSION      release to install, v0.1.0 or 0.1.0 (default: latest)
#   ADDITION_INSTALL_DIR  where to put the binary (default: $HOME/.local/bin)
#   ADDITION_BASE_URL     releases URL, for mirrors and tests (default: the GitHub releases page)
# stdout gets one JSON line on success; everything else goes to stderr, ending with a JSON error
# envelope on failure.
set -eu

BINARY=addition
RELEASES_URL=https://github.com/runagent-dev/addition/releases

workdir=""
staged=""
downloader=""
checksum_tool=""
os=""
arch=""

log() {
  printf '%s\n' "$*" >&2
}

json_escape() {
  printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' | tr -d '\n\r'
}

# fail <kebab-code> <exit-status> <message>
fail() {
  printf '{"error":{"code":"%s","message":"%s"}}\n' "$1" "$(json_escape "$3")" >&2
  exit "$2"
}

cleanup() {
  if [ -n "$workdir" ]; then rm -rf "$workdir"; fi
  if [ -n "$staged" ]; then rm -f "$staged"; fi
}

detect_platform() {
  case "$(uname -s)" in
    Linux) os=linux ;;
    Darwin) os=darwin ;;
    *) fail unsupported-os 2 "unsupported operating system $(uname -s); on Windows download the .zip from $RELEASES_URL" ;;
  esac
  case "$(uname -m)" in
    x86_64 | amd64) arch=amd64 ;;
    arm64 | aarch64) arch=arm64 ;;
    *) fail unsupported-arch 2 "unsupported CPU architecture $(uname -m); see $RELEASES_URL for available builds" ;;
  esac
}

pick_tools() {
  if command -v curl >/dev/null 2>&1; then
    downloader=curl
  elif command -v wget >/dev/null 2>&1; then
    downloader=wget
  else
    fail no-downloader 1 "neither curl nor wget is installed"
  fi
  if command -v sha256sum >/dev/null 2>&1; then
    checksum_tool=sha256sum
  elif command -v shasum >/dev/null 2>&1; then
    checksum_tool=shasum
  else
    fail no-checksum-tool 1 "neither sha256sum nor shasum is installed, so the download cannot be verified"
  fi
}

# Sets release_path to the URL path below the releases base: latest/download or download/v<version>.
resolve_release_path() {
  requested=${ADDITION_VERSION:-latest}
  if [ "$requested" = latest ]; then
    release_path=latest/download
    return
  fi
  version=${requested#v}
  case "$version" in
    *[!0-9A-Za-z.+-]* | "") fail invalid-version 2 "invalid ADDITION_VERSION '$requested'; use latest, v0.1.0 or 0.1.0" ;;
    [0-9]*.[0-9]*.[0-9]*) ;;
    *) fail invalid-version 2 "invalid ADDITION_VERSION '$requested'; use latest, v0.1.0 or 0.1.0" ;;
  esac
  requested=$version
  release_path=download/v$version
}

fetch() {
  url=$1
  dest=$2
  log "Downloading $url"
  case "$downloader" in
    curl) curl -fsSL -o "$dest" "$url" </dev/null || fail download-failed 5 "could not download $url" ;;
    wget) wget -q -O "$dest" "$url" </dev/null || fail download-failed 5 "could not download $url" ;;
  esac
}

sha256_of() {
  case "$checksum_tool" in
    sha256sum) sha256sum "$1" | awk '{ print $1 }' ;;
    shasum) shasum -a 256 "$1" | awk '{ print $1 }' ;;
  esac
}

verify_archive() {
  expected=$(awk -v name="$1" '$2 == name || $2 == "*" name { print $1; exit }' "$workdir/checksums.txt")
  [ -n "$expected" ] || fail checksum-missing 1 "checksums.txt has no entry for $1"
  actual=$(sha256_of "$workdir/$1")
  [ "$actual" = "$expected" ] || fail checksum-mismatch 1 "sha256 of $1 is $actual, checksums.txt says $expected; nothing was installed"
  log "Verified sha256 of $1"
}

install_binary() {
  mkdir -p "$install_dir" || fail install-failed 1 "cannot create $install_dir"
  install_dir=$(cd "$install_dir" && pwd) || fail install-failed 1 "cannot enter $install_dir"
  target=$install_dir/$BINARY
  staged=$(mktemp "$install_dir/.$BINARY.XXXXXX") || fail install-failed 1 "cannot write to $install_dir"
  cp "$workdir/extract/$BINARY" "$staged" || fail install-failed 1 "cannot write to $install_dir"
  chmod 0755 "$staged" || fail install-failed 1 "cannot make $staged executable"
  mv -f "$staged" "$target" || fail install-failed 1 "cannot replace $target"
  staged=""
}

installed_version() {
  reported=$("$target" version </dev/null 2>/dev/null | sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p') || reported=""
  printf '%s' "${reported:-$requested}"
}

# Prints true when running `addition` would start the file just installed.
installed_on_path() {
  found=$(command -v "$BINARY" 2>/dev/null) || return 1
  found_dir=$(cd "$(dirname "$found")" 2>/dev/null && pwd -P) || return 1
  installed_dir=$(cd "$install_dir" && pwd -P)
  [ "$found_dir/$(basename "$found")" = "$installed_dir/$BINARY" ]
}

main() {
  detect_platform
  pick_tools
  resolve_release_path

  install_dir=${ADDITION_INSTALL_DIR:-}
  if [ -z "$install_dir" ]; then
    [ -n "${HOME:-}" ] || fail no-install-dir 2 "HOME is not set; set ADDITION_INSTALL_DIR"
    install_dir=$HOME/.local/bin
  fi
  base_url=${ADDITION_BASE_URL:-$RELEASES_URL}
  base_url=${base_url%/}

  workdir=$(mktemp -d "${TMPDIR:-/tmp}/addition-install.XXXXXX") || fail install-failed 1 "cannot create a temporary directory"
  archive=${BINARY}_${os}_${arch}.tar.gz
  fetch "$base_url/$release_path/$archive" "$workdir/$archive"
  fetch "$base_url/$release_path/checksums.txt" "$workdir/checksums.txt"
  verify_archive "$archive"

  mkdir "$workdir/extract"
  tar -xzf "$workdir/$archive" -C "$workdir/extract" "$BINARY" </dev/null ||
    fail install-failed 1 "$archive does not contain $BINARY"
  install_binary

  version_reported=$(installed_version)
  if installed_on_path; then
    on_path=true
  else
    on_path=false
    log "$install_dir is not first on your PATH for $BINARY. Add it with:"
    log "  export PATH=\"$install_dir:\$PATH\""
  fi
  log "Installed $BINARY $version_reported to $target"
  printf '{"installed":true,"version":"%s","path":"%s","on_path":%s,"next":"addition init"}\n' \
    "$(json_escape "$version_reported")" "$(json_escape "$target")" "$on_path"
}

trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM HUP
main "$@"
