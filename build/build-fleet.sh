#!/bin/sh
# platform: host-agnostic
#   usage: build-fleet.sh GO GOOS GOARCH OUT VERSION
#          Writes OUT/mavergreen-fleet, built by GO for GOOS/GOARCH with VERSION stamped into --version.
set -eu
[ $# -eq 5 ] || { echo "usage: build-fleet.sh GO GOOS GOARCH OUT VERSION" >&2; exit 2; }
GO=$1; GOOS=$2; GOARCH=$3; OUT=$4; VERSION=$5
case "$VERSION" in
  ""|*[[:space:]]*) echo "build-fleet.sh: VERSION must be non-empty and contain no whitespace" >&2; exit 2 ;;
esac
case "$OUT" in /*) ;; *) OUT="$(pwd)/$OUT" ;; esac
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

# spec: INGREDIENTS.md "Declared state" -- the pinned Go is the only Go; never download another
export CGO_ENABLED=0 GOTOOLCHAIN=local GOOS GOARCH GOENV=off GOFLAGS=
mkdir -p "$OUT"
cd "$ROOT"
"$GO" build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$OUT/mavergreen-fleet" ./cmd/mavergreen-fleet
