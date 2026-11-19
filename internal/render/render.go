// Package render writes findings as text or as the JSON envelope.
package render

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/Mavergreen/fleet/internal/model"
)

// Checked says how much was looked at, so an empty report is not silent.
type Checked struct {
	Repos  int `json:"repos"`
	PRs    int `json:"prs"`
	Issues int `json:"issues"`
	Runs   int `json:"runs"`
}

type Options struct {
	TTY   bool
	Width int // <= 0 means no truncation
	JSON  bool
	Now   time.Time // zero means time.Now(); used for the JSON "generated" field
}

var urgency = map[model.Kind]int{
	model.KindCannotRead:      0,
	model.KindStuckPR:         1,
	model.KindRedWorkflow:     2,
	model.KindNeedsReply:      3,
	model.KindRetryInProgress: 4,
}

// rank puts unknown kinds after every known one.
func rank(k model.Kind) int {
	if r, ok := urgency[k]; ok {
		return r
	}
	return len(urgency)
}

func sorted(fs []model.Finding) []model.Finding {
	out := append([]model.Finding{}, fs...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if ua, ub := rank(a.Kind), rank(b.Kind); ua != ub {
			return ua < ub
		}
		if !a.Since.Equal(b.Since) {
			return a.Since.Before(b.Since)
		}
		if a.Repo != b.Repo {
			return a.Repo < b.Repo
		}
		return a.Item < b.Item
	})
	return out
}

// Findings writes the report in the format opt asks for.
func Findings(w io.Writer, fs []model.Finding, checked Checked, opt Options) error {
	fs = sorted(fs)
	if opt.JSON {
		now := opt.Now
		if now.IsZero() {
			now = time.Now()
		}
		return JSON(w, "attention", struct {
			Checked  Checked         `json:"checked"`
			Findings []model.Finding `json:"findings"`
		}{checked, nonNil(fs)}, now)
	}
	if len(fs) == 0 {
		_, err := fmt.Fprintf(w, "%d repos, %d PRs, %d issues, %d workflow runs: nothing needs attention\n",
			checked.Repos, checked.PRs, checked.Issues, checked.Runs)
		return err
	}
	for _, f := range fs {
		if _, err := fmt.Fprintln(w, line(f, opt)); err != nil {
			return err
		}
	}
	noun := "findings"
	if len(fs) == 1 {
		noun = "finding"
	}
	_, err := fmt.Fprintf(w, "%d %s across %d repos\n", len(fs), noun, checked.Repos)
	return err
}

func nonNil(fs []model.Finding) []model.Finding {
	if fs == nil {
		return []model.Finding{}
	}
	return fs
}

func colour(k model.Kind) string {
	switch k {
	case model.KindCannotRead, model.KindStuckPR:
		return "31"
	case model.KindRedWorkflow, model.KindNeedsReply:
		return "33"
	}
	return "2"
}

func line(f model.Finding, opt Options) string {
	head := fmt.Sprintf("%s %s  ", f.Repo, f.Item)
	kind := string(f.Kind)
	tail := fmt.Sprintf("  %s  — %s", f.Why, f.Fix)
	if !opt.TTY {
		return head + kind + tail
	}
	if opt.Width > 0 {
		full := head + kind + tail
		if utf8.RuneCountInString(full) > opt.Width {
			r := []rune(full)[:opt.Width-1]
			full = string(r) + "…"
			// Re-split the truncated text so the kind keeps its colour.
			hr := utf8.RuneCountInString(head)
			kr := utf8.RuneCountInString(kind)
			rr := []rune(full)
			switch {
			case len(rr) <= hr:
				return full
			case len(rr) <= hr+kr:
				return string(rr[:hr]) + paint(f.Kind, string(rr[hr:]))
			}
			return string(rr[:hr]) + paint(f.Kind, kind) + string(rr[hr+kr:])
		}
	}
	return head + paint(f.Kind, kind) + tail
}

func paint(k model.Kind, s string) string { return "\x1b[" + colour(k) + "m" + s + "\x1b[0m" }

var ansi = regexp.MustCompile("\x1b\\[[0-9;]*m")

func stripANSI(s string) string { return ansi.ReplaceAllString(s, "") }

// JSON writes the envelope. payload must marshal to a JSON object, whose
// fields (checked, findings, ...) are placed after the header fields.
func JSON(w io.Writer, command string, payload any, now time.Time) error {
	body, err := marshal(payload)
	if err != nil {
		return err
	}
	if len(body) < 2 || body[0] != '{' {
		return fmt.Errorf("render: payload must be a JSON object")
	}
	cmd, _ := marshal(command)
	gen, _ := marshal(now.UTC().Format(time.RFC3339))
	var raw bytes.Buffer
	fmt.Fprintf(&raw, `{"schema":"fleet/v1","command":%s,"generated":%s`, cmd, gen)
	if len(body) > 2 {
		raw.WriteByte(',')
	}
	raw.Write(body[1:])
	var out bytes.Buffer
	if err := json.Indent(&out, raw.Bytes(), "", "  "); err != nil {
		return err
	}
	out.WriteByte('\n')
	_, err = w.Write(out.Bytes())
	return err
}

// marshal encodes v without HTML escaping, so & < > stay literal.
func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}
