#!/bin/sh
# platform: macOS-only -- reads the installed manifest with PlistBuddy and the receipt with pkgutil
#   usage: smoke-mavericks.sh
#          Runs against the INSTALLED fleet (FLEET_ROOT, default /, is the volume it is installed on):
#          the manifest's version is FLEET_VERSION_UNDER_TEST (required on a real install, FLEET_ROOT unset), --version reports it,
#          the manpage is installed, the family link farm exports the binary and the manpage (checked only on a real install, FLEET_ROOT unset:
#          the farm's links come from the postinstall), and graph --json, with SHIPYARD_SCRIPTS
#          at tests/fixtures/scripts, matches tests/fixtures/graph.json once its time-varying
#          "generated" field is replaced by NORMALISED. Exits 77, a skip, only when fleet is not
#          installed; the acceptance job fails on 77.
set -eu
R="$(cd "$(dirname "$0")/.." && pwd)"
ROOT="${FLEET_ROOT:-}"
ROOT="${ROOT%/}"
T="$ROOT/usr/local/mavergreen/fleet"
[ -f "$T/mavergreen.plist" ] || { echo "fleet is not installed under ${ROOT:-/} -- skipping" >&2; exit 77; }
BIN="$T/bin/mavergreen-fleet"

[ -n "$ROOT" ] || [ -n "${FLEET_VERSION_UNDER_TEST:-}" ] \
  || { echo "FLEET_VERSION_UNDER_TEST must name the version this run installed" >&2; exit 1; }
installed="$(/usr/libexec/PlistBuddy -c 'Print :version' "$T/mavergreen.plist")"
if [ -n "${FLEET_VERSION_UNDER_TEST:-}" ]; then
  [ "$installed" = "$FLEET_VERSION_UNDER_TEST" ] \
    || { echo "the installed manifest says $installed, not $FLEET_VERSION_UNDER_TEST" >&2; exit 1; }
fi
if [ -z "$ROOT" ] && [ -n "${FLEET_VERSION_UNDER_TEST:-}" ]; then
  got="$(pkgutil --pkg-info dev.mavergreen.fleet | sed -n 's/^version: //p')"
  [ "$got" = "$FLEET_VERSION_UNDER_TEST" ] || { echo "the receipt says $got, not $FLEET_VERSION_UNDER_TEST" >&2; exit 1; }
fi

[ "$("$BIN" --version)" = "mavergreen-fleet $installed" ] \
  || { echo "--version printed '$("$BIN" --version)', want 'mavergreen-fleet $installed'" >&2; exit 1; }
MAN="$T/share/man/man1/mavergreen-fleet.1"
[ -f "$MAN" ] || { echo "no manpage at $MAN" >&2; exit 1; }
if [ -z "$ROOT" ]; then
  [ /usr/local/mavergreen/bin/mavergreen-fleet -ef "$BIN" ] \
    || { echo "/usr/local/mavergreen/bin/mavergreen-fleet is not the installed mavergreen-fleet" >&2; exit 1; }
  [ /usr/local/mavergreen/share/man/man1/mavergreen-fleet.1 -ef "$MAN" ] \
    || { echo "the link farm does not export the installed manpage" >&2; exit 1; }
fi

W="$(mktemp -d "${TMPDIR:-/tmp}/fleet-smoke.XXXXXX")"; trap 'rm -rf "$W"' EXIT
SHIPYARD_SCRIPTS="$R/tests/fixtures/scripts" "$BIN" --json graph > "$W/graph.json"
sed 's/"generated": "[^"]*"/"generated": "NORMALISED"/' "$W/graph.json" > "$W/got.json"
cmp -s "$W/got.json" "$R/tests/fixtures/graph.json" || {
  echo "graph --json differs from tests/fixtures/graph.json:" >&2
  diff "$R/tests/fixtures/graph.json" "$W/got.json" >&2 || true
  exit 1
}
echo "smoke-mavericks OK ($installed)"
