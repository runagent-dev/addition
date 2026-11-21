#!/bin/sh
# Installs the addition CLI from its GitHub release: no prompts, no sudo, no shell rc edits.
#   curl -fsSL https://raw.githubusercontent.com/runagent-dev/addition/main/install.sh | sh
# Environment:
#   ADDITION_VERSION      release to install, v0.1.0 or 0.1.0 (default: latest)
#   ADDITION_INSTALL_DIR  where to put the binary (default: $HOME/.local/bin)
#   ADDITION_BASE_URL     releases URL, for mirrors and tests: https://, or http:// on 127.0.0.1 or localhost
# stdout gets one JSON line on success; everything else goes to stderr, ending with a JSON error
# envelope on failure.
set -eu
LC_ALL=C
export LC_ALL

BINARY=addition
RELEASES_URL=https://github.com/runagent-dev/addition/releases
MAX_ARCHIVE_BYTES=104857600
MAX_CHECKSUMS_BYTES=1048576
MAX_BINARY_BYTES=209715200

workdir=""
staged=""
downloader=""
checksum_tool=""
os=""
arch=""

log() {
  printf '%s\n' "$*" >&2
}

# Escapes quotes, backslashes and every control character so the result sits inside a JSON string.
json_escape() {
  printf '%s\n' "$1" | awk '
    BEGIN {
      for (i = 1; i < 32; i++) escaped[sprintf("%c", i)] = sprintf("\\u%04x", i)
      escaped["\t"] = "\\t"
      escaped["\r"] = "\\r"
      escaped["\\"] = "\\\\"
      escaped["\""] = "\\\""
    }
    {
      if (NR > 1) printf "\\n"
      for (i = 1; i <= length($0); i++) {
        c = substr($0, i, 1)
        printf "%s", (c in escaped) ? escaped[c] : c
      }
    }'
}

# The trailing "." keeps command substitution from eating a final newline of the value.
shell_quote() {
  quoted=$(printf '%s.' "$1" | sed "s/'/'\\\\''/g")
  printf "'%s'" "${quoted%.}"
}

# fail <kebab-code> <exit-status> <message>
fail() {
  printf '{"error":{"code":"%s","message":"%s"}}\n' "$1" "$(json_escape "$3")" >&2
  exit "$2"
}

cleanup() {
  cd / 2>/dev/null || :
  if [ -n "$workdir" ]; then rm -rf "$workdir"; fi
  if [ -n "$staged" ]; then rm -f "$staged"; fi
}

# Prints the absolute path of an existing directory, newlines in the name included.
canonical_dir() {
  resolved=$(cd "$1" && pwd && echo x) || return 1
  resolved=${resolved%x}
  printf '%s' "${resolved%?}"
}

file_size() {
  size=$(wc -c <"$1")
  echo $((size))
}

# Runs a command whose output files may not exceed about <bytes>; shells count ulimit blocks as 512 or
# 1024 bytes, so callers still compare file_size exactly.
capped() {
  limit=$1
  shift
  (
    ulimit -f $((limit / 512 + 1)) 2>/dev/null || :
    "$@"
  )
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

# Matches ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$
valid_version() {
  case $1 in
    *-*)
      core=${1%%-*}
      prerelease=${1#*-}
      [ -n "$prerelease" ] || return 1
      case $prerelease in *[!0-9A-Za-z.]*) return 1 ;; esac
      ;;
    *) core=$1 ;;
  esac
  case $core in
    *.*.*.*) return 1 ;;
    *.*.*) ;;
    *) return 1 ;;
  esac
  major=${core%%.*}
  rest=${core#*.}
  minor=${rest%%.*}
  patch=${rest#*.}
  for part in "$major" "$minor" "$patch"; do
    case $part in "" | *[!0-9]*) return 1 ;; esac
  done
}

# Sets release_path to the URL path below the releases base: latest/download or download/v<version>.
resolve_release_path() {
  requested=${ADDITION_VERSION:-latest}
  if [ "$requested" = latest ]; then
    release_path=latest/download
    return
  fi
  version=${requested#v}
  valid_version "$version" || fail invalid-version 2 "invalid ADDITION_VERSION '$requested'; use latest, v0.1.0 or 0.1.0"
  requested=$version
  release_path=download/v$version
}

resolve_base_url() {
  base_url=${ADDITION_BASE_URL:-$RELEASES_URL}
  base_url=${base_url%/}
  case $base_url in
    *[[:space:]]* | *[[:cntrl:]]* | *@* | *\\* | *\?* | *\#*)
      fail invalid-base-url 2 "ADDITION_BASE_URL must not contain whitespace, control characters, @, backslashes, ? or #"
      ;;
  esac
  case $base_url in
    https://?* | http://127.0.0.1 | http://127.0.0.1:* | http://127.0.0.1/* | http://localhost | http://localhost:* | http://localhost/*) ;;
    *) fail invalid-base-url 2 "ADDITION_BASE_URL must start with https:// (http:// is accepted only for 127.0.0.1 and localhost)" ;;
  esac
}

resolve_install_dir() {
  install_dir=${ADDITION_INSTALL_DIR:-}
  if [ -z "$install_dir" ]; then
    [ -n "${HOME:-}" ] || fail no-install-dir 2 "HOME is not set; set ADDITION_INSTALL_DIR"
    install_dir=$HOME/.local/bin
  fi
  case $install_dir in
    /*) ;;
    *) install_dir=$PWD/$install_dir ;;
  esac
}

# Everything below runs inside a private directory, addressed by relative names once we are in it.
enter_workdir() {
  tmp_base=${TMPDIR:-/tmp}
  case $tmp_base in
    /*) ;;
    *) tmp_base=$PWD/$tmp_base ;;
  esac
  workdir=$(mktemp -d "$tmp_base/addition-install.XXXXXX") || fail install-failed 1 "cannot create a temporary directory"
  cd "$workdir" || fail install-failed 1 "cannot enter the temporary directory"
}

# shellcheck disable=SC2317
run_downloader() {
  case "$downloader" in
    curl) curl -fsSL -o "$2" -- "$1" </dev/null ;;
    wget) wget -q -O "$2" -- "$1" </dev/null ;;
  esac
}

# fetch <url> <dest> <max-bytes>
fetch() {
  log "Downloading $1"
  if ! capped "$3" run_downloader "$1" "$2"; then
    if [ -f "$2" ] && [ "$(file_size "$2")" -gt "$3" ]; then
      fail download-too-large 5 "$1 is larger than $3 bytes"
    fi
    fail download-failed 5 "could not download $1"
  fi
  [ "$(file_size "$2")" -le "$3" ] || fail download-too-large 5 "$1 is larger than $3 bytes"
}

# Prints the lower-case sha256 of a file, read from stdin so no file name can alter the digest.
sha256_of() {
  case "$checksum_tool" in
    sha256sum) digest=$(sha256sum <"$1") || return 1 ;;
    shasum) digest=$(shasum -a 256 <"$1") || return 1 ;;
  esac
  digest=${digest%% *}
  [ "${#digest}" -eq 64 ] || return 1
  case $digest in *[!0-9a-f]*) return 1 ;; esac
  printf '%s' "$digest"
}

# Prints missing, malformed, or "ok <lower-case digest>" for the one "<64 hex>  <name>" (or " *<name>") line.
checksum_record() {
  awk -v name="$1" '
    $2 == name || $2 == "*" name {
      records++
      digest = substr($0, 1, 64)
      wellformed = length(digest) == 64 && digest !~ /[^0-9a-fA-F]/ && ($0 == digest "  " name || $0 == digest " *" name)
    }
    END {
      if (records == 0) print "missing"
      else if (records > 1 || !wellformed) print "malformed"
      else print "ok " tolower(digest)
    }' checksums.txt
}

verify_archive() {
  record=$(checksum_record "$1") || fail checksum-failed 1 "cannot read checksums.txt"
  case $record in
    missing) fail checksum-missing 1 "checksums.txt has no entry for $1" ;;
    malformed) fail checksum-malformed 1 "checksums.txt must hold exactly one '<sha256>  $1' line" ;;
  esac
  expected=${record#ok }
  actual=$(sha256_of "$1") || fail checksum-failed 1 "$checksum_tool could not hash $1"
  [ "$actual" = "$expected" ] || fail checksum-mismatch 1 "sha256 of $1 is $actual, checksums.txt says $expected; nothing was installed"
  log "Verified sha256 of $1"
}

# Accepts an archive with one regular file named addition, no other kind of member, and no member
# that is absolute or climbs out with "..".
validate_archive() {
  if ! tar -tzf "$1" </dev/null >names.txt 2>/dev/null || ! tar -tvzf "$1" </dev/null >listing.txt 2>/dev/null; then
    fail archive-invalid 1 "$1 is not a readable tar.gz archive"
  fi
  verdict=$(awk -v binary="$BINARY" '
    NR == FNR { kind[FNR] = substr($0, 1, 1); listed = FNR; next }
    {
      if (kind[FNR] != "-") bad = 1
      if ($0 ~ /^\// || $0 ~ /(^|\/)\.\.(\/|$)/) bad = 1
      if ($0 == binary) binaries++
    }
    END { print (!bad && binaries == 1 && listed == FNR) ? "ok" : "invalid" }' listing.txt names.txt) ||
    fail archive-invalid 1 "cannot inspect $1"
  [ "$verdict" = ok ] || fail archive-invalid 1 "$1 must hold exactly one regular file named $BINARY and nothing that is not a regular file, absolute or outside its own folder"
}

extract_binary() {
  validate_archive "$1"
  mkdir extract
  if ! capped "$MAX_BINARY_BYTES" tar -xzf "$1" -C extract "$BINARY" </dev/null 2>/dev/null; then
    if [ -f "extract/$BINARY" ] && [ "$(file_size "extract/$BINARY")" -gt "$MAX_BINARY_BYTES" ]; then
      fail binary-too-large 1 "$BINARY in $1 is larger than $MAX_BINARY_BYTES bytes"
    fi
    fail archive-invalid 1 "could not extract $BINARY from $1"
  fi
  if [ ! -f "extract/$BINARY" ] || [ -L "extract/$BINARY" ]; then
    fail archive-invalid 1 "$BINARY in $1 is not a regular file"
  fi
  [ "$(file_size "extract/$BINARY")" -le "$MAX_BINARY_BYTES" ] || fail binary-too-large 1 "$BINARY in $1 is larger than $MAX_BINARY_BYTES bytes"
}

install_binary() {
  mkdir -p "$install_dir" || fail install-failed 1 "cannot create $install_dir"
  install_dir=$(canonical_dir "$install_dir") || fail install-failed 1 "cannot enter $install_dir"
  target=$install_dir/$BINARY
  if [ -d "$target" ]; then
    fail install-failed 1 "$target is a directory, not a file; remove it or choose another ADDITION_INSTALL_DIR"
  fi
  staged=$(mktemp "$install_dir/.$BINARY.XXXXXX") || fail install-failed 1 "cannot write to $install_dir"
  cp "extract/$BINARY" "$staged" || fail install-failed 1 "cannot write to $install_dir"
  chmod 0755 "$staged" || fail install-failed 1 "cannot make $staged executable"
  mv -f "$staged" "$target" || fail install-failed 1 "cannot replace $target"
  staged=""
}

installed_version() {
  reported=$("$target" version </dev/null 2>/dev/null | sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p') || reported=""
  printf '%s' "${reported:-$requested}"
}

# Succeeds when running `addition` would start the file just installed.
installed_on_path() {
  found=$(command -v "$BINARY" 2>/dev/null) || return 1
  case $found in
    /*) ;;
    *) found=./$found ;;
  esac
  found_dir=$(cd "$(dirname "$found")" 2>/dev/null && pwd -P) || return 1
  installed_dir=$(cd "$install_dir" && pwd -P)
  [ "$found_dir/$(basename "$found")" = "$installed_dir/$BINARY" ]
}

main() {
  detect_platform
  pick_tools
  resolve_release_path
  resolve_base_url
  resolve_install_dir

  enter_workdir
  archive=${BINARY}_${os}_${arch}.tar.gz
  fetch "$base_url/$release_path/$archive" "$archive" "$MAX_ARCHIVE_BYTES"
  fetch "$base_url/$release_path/checksums.txt" checksums.txt "$MAX_CHECKSUMS_BYTES"
  verify_archive "$archive"
  extract_binary "$archive"
  install_binary

  version_reported=$(installed_version)
  if installed_on_path; then
    on_path=true
  else
    on_path=false
    log "$install_dir is not first on your PATH for $BINARY. Add it with:"
    log "  export PATH=$(shell_quote "$install_dir"):\"\$PATH\""
  fi
  log "Installed $BINARY $version_reported to $target"
  printf '{"installed":true,"version":"%s","path":"%s","on_path":%s,"next":"addition init"}\n' \
    "$(json_escape "$version_reported")" "$(json_escape "$target")" "$on_path"
}

trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM HUP

# One compound command, so a script cut short anywhere inside it does not parse and runs nothing.
{
  main "$@"
}
