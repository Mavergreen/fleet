package retry

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Cap is the number of fleet retries allowed per Renovate update.
const Cap = 2

// BoxState says what TickRebaseBox found in a PR body.
type BoxState int

const (
	BoxTicked        BoxState = iota // we ticked it
	BoxAlreadyTicked                 // a retry is already requested
	BoxAbsent                        // no rebase-check checkbox
)

// Renovate writes " - [ ] <!-- rebase-check -->..." (one leading space in
// practice); allow up to three, as Markdown does.
var boxRe = regexp.MustCompile(`(?m)^ {0,3}- \[([ xX])\] <!-- rebase-check -->`)

// TickRebaseBox ticks the first rebase-check checkbox, changing only the
// single character inside the brackets (line endings are untouched).
func TickRebaseBox(body string) (string, BoxState) {
	m := boxRe.FindStringSubmatchIndex(body)
	if m == nil {
		return body, BoxAbsent
	}
	if body[m[2]] != ' ' {
		return body, BoxAlreadyTicked
	}
	return body[:m[2]] + "x" + body[m[2]+1:], BoxTicked
}

// UpdateKey normalises a PR title: trimmed, whitespace runs collapsed to one
// space. Renovate retitles on a new target version, which resets the cap.
func UpdateKey(prTitle string) string {
	return strings.Join(strings.FieldsFunc(prTitle, unicode.IsSpace), " ")
}

// Comment builds fleet's retry comment: a visible line, then a hidden marker.
// update and pattern are query-escaped in the marker so neither can contain
// spaces or close the HTML comment.
func Comment(attempt, cap int, patternID, runURL, update string) string {
	return fmt.Sprintf("fleet: retry %d/%d, pattern `%s`, run %s\n<!-- fleet-retry v1 update=%s attempt=%d pattern=%s -->",
		attempt, cap, patternID, runURL, url.QueryEscape(update), attempt, url.QueryEscape(patternID))
}

var markerRe = regexp.MustCompile(`<!-- fleet-retry (\S+)([^>]*?)-->`)

// CountRetries counts well-formed v1 markers whose update equals update, across
// every occurrence in every body. Malformed markers are ignored; attempt must
// be a plain integer >= 1 (no sign).
//
// Markers are not authenticated: callers must pass only the bodies of comments
// authored by the login fleet is running as, or anyone could post a marker on a
// public PR and burn the cap.
func CountRetries(commentBodies []string, update string) int {
	n := 0
	for _, body := range commentBodies {
		for _, m := range markers(body) {
			if m.update == update {
				n++
			}
		}
	}
	return n
}

// marker is one well-formed v1 fleet-retry marker.
type marker struct {
	update, pattern string
	attempt         int
}

// markers returns the well-formed v1 markers in body, in order.
func markers(body string) []marker {
	var out []marker
	for _, m := range markerRe.FindAllStringSubmatch(body, -1) {
		if m[1] != "v1" {
			continue
		}
		var mk marker
		var haveUpd, haveAttempt bool
		for _, f := range strings.Fields(m[2]) {
			k, v, ok := strings.Cut(f, "=")
			if !ok {
				continue
			}
			switch k {
			case "update":
				u, err := url.QueryUnescape(v)
				if err == nil {
					mk.update, haveUpd = u, true
				}
			case "attempt":
				if a, err := strconv.Atoi(v); err == nil && a >= 1 && v[0] != '+' {
					mk.attempt, haveAttempt = a, true
				}
			case "pattern":
				if p, err := url.QueryUnescape(v); err == nil {
					mk.pattern = p
				}
			}
		}
		if haveUpd && haveAttempt {
			out = append(out, mk)
		}
	}
	return out
}
