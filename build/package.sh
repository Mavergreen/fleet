#!/bin/sh
# platform: host-agnostic
#   usage: package.sh VERSION DARWIN_BIN OUT [LINUX_BIN]
#          Writes OUT/fleet-VERSION.pkg, OUT/fleet-VERSION-macos-universal.tar.gz (the pkg's tree, standalone)
#          and OUT/build-info.txt from DARWIN_BIN (the universal mavergreen-fleet), and with LINUX_BIN also OUT/fleet-VERSION-linux-x86_64.tar.gz, which holds
#          opt/mavergreen/fleet/bin/mavergreen-fleet and opt/mavergreen/fleet/share/man/man1/mavergreen-fleet.1.
#          Both install man/mavergreen-fleet.1, which the family link farm exports from share/man/man1. Prints each path it wrote. The pkg's stage
#          stays in OUT/stage for the smoke test (FLEET_ROOT=OUT/stage/root). Needs $SHIPYARD_SCRIPTS
#          and a git checkout (the pkg's mtime is HEAD's commit time unless SOURCE_DATE_EPOCH is set).
# spec: shipyard SKILL.md "Install layout and identity" -- one product tree, usr/local/mavergreen/fleet/,
#       from stage_product.sh, wrapped by set_install_floor.sh at the 10.9.5 floor
set -eu
[ $# -eq 3 ] || [ $# -eq 4 ] || { echo "usage: package.sh VERSION DARWIN_BIN OUT [LINUX_BIN]" >&2; exit 2; }
VER=$1; DARWIN_BIN=$2; OUT=$3; LINUX_BIN=${4:-}
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
. "$ROOT/build/msc.sh"
case "$VER" in ''|*[!0-9.]*) echo "package: VERSION '$VER' is not YYYYMMDD.N" >&2; exit 2 ;; esac
[ -f "$DARWIN_BIN" ] && [ -x "$DARWIN_BIN" ] || { echo "package: no executable $DARWIN_BIN" >&2; exit 1; }
[ -z "$LINUX_BIN" ] || { [ -f "$LINUX_BIN" ] && [ -x "$LINUX_BIN" ]; } || { echo "package: no executable $LINUX_BIN" >&2; exit 1; }
mkdir -p "$OUT"
OUT="$(cd "$OUT" && pwd)"
ID="$(sh "$SHIPYARD/product-name.sh" identifier fleet)"

STAGE="$OUT/stage"
rm -rf "$STAGE"
mkdir -p "$STAGE/root/usr/local/mavergreen/fleet/bin" "$STAGE/root/usr/local/mavergreen/fleet/share/man/man1" "$STAGE/scripts"
cp "$DARWIN_BIN" "$STAGE/root/usr/local/mavergreen/fleet/bin/mavergreen-fleet"
chmod 755 "$STAGE/root/usr/local/mavergreen/fleet/bin/mavergreen-fleet"
cp "$ROOT/man/mavergreen-fleet.1" "$STAGE/root/usr/local/mavergreen/fleet/share/man/man1/"
chmod 644 "$STAGE/root/usr/local/mavergreen/fleet/share/man/man1/mavergreen-fleet.1"

sh "$SHIPYARD/stage_product.sh" --stage "$STAGE/root" --product fleet \
  --name "Fleet for Mavericks" --version "$VER" --scripts-out "$STAGE/scripts" >&2
if [ -z "${SOURCE_DATE_EPOCH:-}" ]; then
  SOURCE_DATE_EPOCH="$(cd "$ROOT" && git log -1 --format=%ct)"
  export SOURCE_DATE_EPOCH
fi
sh "$SHIPYARD/build_component_pkg.sh" --root "$STAGE/root" --identifier "$ID" \
  --version "$VER" --install-location / --scripts "$STAGE/scripts" --out "$STAGE/component.pkg" >&2
rm -f "$OUT/fleet-$VER.pkg"
sh "$SHIPYARD/set_install_floor.sh" --identifier "$ID" --title "Fleet for Mavericks $VER" \
  --component "$STAGE/component.pkg" --out "$OUT/fleet-$VER.pkg" --min-os 10.9.5 --require-scripts >&2
echo "$OUT/fleet-$VER.pkg"

# spec: shipyard scripts/stage_standalone.sh -- the standalone macOS archive, derived from the pkg's staged tree
rm -f "$OUT/fleet-$VER-macos-universal.tar.gz"
sh "$SHIPYARD/stage_standalone.sh" "$STAGE/root" fleet "$VER" universal "$OUT/fleet-$VER-macos-universal.tar.gz" >&2
echo "$OUT/fleet-$VER-macos-universal.tar.gz"

set -- "pkg=fleet-$VER.pkg" "identifier=$ID" "golang=$(tr -d '[:space:]' < "$ROOT/components/golang/version")" \
  "go-upstream=$(tr -d '[:space:]' < "$ROOT/components/go-upstream/version")"
sh "$SHIPYARD/build-info.sh" "$OUT/build-info.txt" "$@"
echo "$OUT/build-info.txt"

if [ -n "$LINUX_BIN" ]; then
  T="$OUT/tarball"
  rm -rf "$T"
  mkdir -p "$T/opt/mavergreen/fleet/bin" "$T/opt/mavergreen/fleet/share/man/man1"
  cp "$LINUX_BIN" "$T/opt/mavergreen/fleet/bin/mavergreen-fleet"
  chmod 755 "$T/opt/mavergreen/fleet/bin/mavergreen-fleet"
  cp "$ROOT/man/mavergreen-fleet.1" "$T/opt/mavergreen/fleet/share/man/man1/"
  chmod 644 "$T/opt/mavergreen/fleet/share/man/man1/mavergreen-fleet.1"
  find "$T" -type d -exec chmod 755 {} +
  # platform: BSD date reads the epoch with -r and GNU date with -d @; touch -t takes [[CC]YY]MMDDhhmm[.SS]
  stamp="$(date -u -r "$SOURCE_DATE_EPOCH" +%Y%m%d%H%M.%S 2>/dev/null || date -u -d "@$SOURCE_DATE_EPOCH" +%Y%m%d%H%M.%S)"
  find "$T" -exec touch -t "$stamp" {} +
  # platform: GNU tar sets ownership with --owner/--group, bsdtar 3 (modern macOS) with --uid/--gid, and
  #           bsdtar 2.8.3 (10.9) has neither, so it keeps the packer's own
  owner=""
  if tar --owner=0 --group=0 --numeric-owner -cf /dev/null -T /dev/null 2>/dev/null; then
    owner="--owner=0 --group=0 --numeric-owner"
  elif tar --uid 0 --gid 0 -cf /dev/null -T /dev/null 2>/dev/null; then
    owner="--uid 0 --gid 0"
  fi
  out="$OUT/fleet-$VER-linux-x86_64.tar.gz"
  rm -f "$out"
  # shellcheck disable=SC2086
  tar $owner -cf "$T.tar" -C "$T" opt
  gzip -n -c "$T.tar" > "$out"
  rm -rf "$T" "$T.tar"
  echo "$out"
fi
