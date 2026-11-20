#!/bin/sh
# platform: host-agnostic
#   usage: build-universal.sh GO_AMD64 GO_ARM64 OUT VERSION
#          Writes OUT/mavergreen-fleet, a universal Mach-O: darwin/amd64 built by GO_AMD64 (the
#          golang-127 toolchain, 10.9 floor) and darwin/arm64 built by GO_ARM64 (upstream Go), merged
#          by shipyard's lipo-merge-tree.sh with $LIPO (clang22's llvm-lipo, from fetch_clang22.sh; required). Also leaves the thin amd64 build
#          in OUT/amd64/ for the compat guard. OUT must not exist.
set -eu
[ $# -eq 4 ] || { echo "usage: build-universal.sh GO_AMD64 GO_ARM64 OUT VERSION" >&2; exit 2; }
GO_AMD64=$1; GO_ARM64=$2; OUT=$3; VERSION=$4
SELF="$(cd "$(dirname "$0")" && pwd)"
. "$SELF/msc.sh"
case "$OUT" in /*) ;; *) OUT="$(pwd)/$OUT" ;; esac
[ ! -e "$OUT" ] || { echo "build-universal: $OUT already exists" >&2; exit 2; }
[ -x "${LIPO:-}" ] || { echo "build-universal: LIPO must name clang22's llvm-lipo (sh \$SHIPYARD_SCRIPTS/fetch_clang22.sh components/clang22 prints its prefix)" >&2; exit 2; }
export LIPO
mkdir -p "$OUT/amd64" "$OUT/arm64"
sh "$SELF/build-fleet.sh" "$GO_AMD64" darwin amd64 "$OUT/amd64" "$VERSION"
sh "$SELF/build-fleet.sh" "$GO_ARM64" darwin arm64 "$OUT/arm64" "$VERSION"
sh "$SHIPYARD/lipo-merge-tree.sh" --a "$OUT/amd64" --b "$OUT/arm64" --out "$OUT/universal" --require-archs "x86_64 arm64"
mv "$OUT/universal/mavergreen-fleet" "$OUT/mavergreen-fleet"
rm -rf "$OUT/universal" "$OUT/arm64"
echo "$OUT/mavergreen-fleet"
