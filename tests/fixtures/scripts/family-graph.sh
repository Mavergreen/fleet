#!/bin/sh
# platform: host-agnostic
#   usage: family-graph.sh [--from-dir DIR]
#          A fixture standing in for shipyard's family-graph.sh: prints a fixed small graph, for
#          tests/smoke-mavericks.sh to point SHIPYARD_SCRIPTS at, so the installed fleet's graph --json
#          runs offline. Exits 0.
cat <<'GRAPH'
cmake -> clang-22
rust -> clang-22
rust -> make
wezterm -> rust
DIAMOND wezterm: clang-22 via rust
GRAPH
