#!/bin/sh
# spec: stands in for shipyard's flag-day deviations.sh; prints
# "retryable <scope> <reason>" for every .mavergreen retryable label dir,
# except labels named omit-*, which it pretends not to accept.
for d in "$1"/.mavergreen/deviations/retryable/*/; do
	[ -d "$d" ] || continue
	case "$(basename "$d")" in omit-*) continue ;; esac
	printf 'retryable %s %s\n' "$(cat "$d/scope")" "$(cat "$d/reason")"
done
