#!/bin/sh
# platform: host-agnostic
#   usage: fetch-go.sh VERSION OS-ARCH DEST
#          Puts upstream Go VERSION (components/go-upstream/version, such as 1.27.4) for OS-ARCH
#          (darwin-arm64, linux-amd64) in DEST/go and prints DEST/go/bin/go. The tarball is checked
#          against the SHA-256 that go.dev publishes beside it, fetched first; no checksum, no Go.
#          Needs $SHIPYARD_SCRIPTS (install@v1 sets it).
# spec: mavergreen-conventions SKILL.md "Verifying the fetched upstream" -- upstream publishes a
#       checksum for each file, so the fetch is verified against that, not a hash kept here
set -eu
[ $# -eq 3 ] || { echo "usage: fetch-go.sh VERSION OS-ARCH DEST" >&2; exit 2; }
VER=$1; PLAT=$2; DEST=$3
case "$VER" in ''|*[!0-9.]*) echo "fetch-go: bad version '$VER'" >&2; exit 2 ;; esac
case "$PLAT" in darwin-arm64|darwin-amd64|linux-amd64) ;; *) echo "fetch-go: unsupported platform '$PLAT'" >&2; exit 2 ;; esac
SELF="$(cd "$(dirname "$0")" && pwd)"
. "$SELF/msc.sh"
. "$SHIPYARD/mavericks_fetch.sh"
file="go$VER.$PLAT.tar.gz"
base="https://dl.google.com/go"
mkdir -p "$DEST"
shipyard_curl "$base/$file.sha256" "$DEST/$file.sha256"
sum="$(tr -d ' \n\t' < "$DEST/$file.sha256")"
case "$sum" in ''|*[!0-9a-f]*) echo "fetch-go: $file.sha256 is not a SHA-256" >&2; exit 1 ;; esac
[ "${#sum}" -eq 64 ] || { echo "fetch-go: $file.sha256 is not a SHA-256" >&2; exit 1; }
shipyard_fetch_verified "$base/$file" "$sum" "$DEST" "$file" >&2
rm -rf "$DEST/go"
tar -xzf "$DEST/$file" -C "$DEST"
echo "$DEST/go/bin/go"
