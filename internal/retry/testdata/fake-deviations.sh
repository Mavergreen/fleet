#!/bin/sh
# platform: host-agnostic
# spec: internal/retry/declared_test.go -- stands in for shipyard's flag-day deviations.sh, printing retryable lines per label dir but omit-*
for d in "$1"/.mavergreen/deviations/retryable/*/; do
	[ -d "$d" ] || continue
	case "$(basename "$d")" in omit-*) continue ;; esac
	printf 'retryable %s %s\n' "$(cat "$d/scope")" "$(cat "$d/reason")"
done
