package render

import (
	"bytes"
	"flag"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Mavergreen/fleet/internal/model"
)

var update = flag.Bool("update", false, "rewrite golden files")

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func sample() []model.Finding {
	return []model.Finding{
		{Repo: "a", Item: "#1", Kind: model.KindRetryInProgress, Since: t0, Why: "w1", Fix: "f1", URL: "u1"},
		{Repo: "b", Item: "#2", Kind: model.KindNeedsReply, Since: t0, Why: "w2", Fix: "f2", URL: "u2"},
		{Repo: "c", Item: "#3", Kind: model.KindStuckPR, Since: t0.Add(time.Hour), Why: "w3", Fix: "f3", URL: "u3"},
		{Repo: "d", Item: "#4", Kind: model.KindStuckPR, Since: t0, Why: "w4", Fix: "f4", URL: "u4"},
		{Repo: "e", Item: "#5", Kind: model.KindRedWorkflow, Since: t0, Why: "w5", Fix: "f5", URL: "u5"},
		{Repo: "f", Item: "#6", Kind: model.KindCannotRead, Since: t0, Why: "w6", Fix: "f6", URL: "u6"},
	}
}

func TestRenderSortsMostUrgentFirst(t *testing.T) {
	var b bytes.Buffer
	if err := Findings(&b, sample(), Checked{Repos: 6}, Options{}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	want := []string{"f #6", "d #4", "c #3", "e #5", "b #2", "a #1"}
	if len(lines) != 7 {
		t.Fatalf("lines = %d: %q", len(lines), lines)
	}
	for i, w := range want {
		if !strings.HasPrefix(lines[i], w) {
			t.Errorf("line %d = %q, want prefix %q", i, lines[i], w)
		}
	}
	if lines[6] != "6 findings across 6 repos" {
		t.Errorf("summary = %q", lines[6])
	}
}

func TestRenderAllClearNamesWhatWasChecked(t *testing.T) {
	var b bytes.Buffer
	if err := Findings(&b, nil, Checked{Repos: 39, PRs: 33, Issues: 41, Runs: 212}, Options{}); err != nil {
		t.Fatal(err)
	}
	want := "39 repos, 33 PRs, 41 issues, 212 workflow runs: nothing needs attention\n"
	if b.String() != want {
		t.Errorf("got %q", b.String())
	}
}

func TestRenderPlainWhenNotTTY(t *testing.T) {
	var b bytes.Buffer
	if err := Findings(&b, sample(), Checked{Repos: 6}, Options{TTY: false, Width: 10}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "\x1b[") {
		t.Error("ANSI in non-TTY output")
	}
	if strings.Contains(b.String(), "…") {
		t.Error("truncated in non-TTY output")
	}
}

func TestRenderTruncatesToWidthOnTTY(t *testing.T) {
	var b bytes.Buffer
	if err := Findings(&b, sample(), Checked{Repos: 6}, Options{TTY: true, Width: 30}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "\x1b[") {
		t.Error("expected colour on TTY")
	}
	for _, l := range strings.Split(strings.TrimRight(b.String(), "\n"), "\n") {
		if n := utf8.RuneCountInString(stripANSI(l)); n > 30 {
			t.Errorf("line %q has %d runes", l, n)
		}
	}
	if !strings.Contains(b.String(), "…") {
		t.Error("expected ellipsis")
	}
}

func TestJSONEnvelopeShape(t *testing.T) {
	var b bytes.Buffer
	payload := struct {
		Checked  Checked         `json:"checked"`
		Findings []model.Finding `json:"findings"`
	}{Checked{Repos: 1, PRs: 2, Issues: 3, Runs: 4}, sample()[:2]}
	if err := JSON(&b, "attention", payload, time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	const golden = "testdata/envelope.golden.json"
	if *update {
		if err := os.WriteFile(golden, b.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b.Bytes(), want) {
		t.Errorf("golden mismatch:\n%s", b.String())
	}
}

func TestFindingsJSONEmptyIsArrayAndSorted(t *testing.T) {
	var b bytes.Buffer
	if err := Findings(&b, nil, Checked{}, Options{JSON: true, TTY: true, Width: 5}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), `"findings": []`) || strings.Contains(b.String(), "\x1b") {
		t.Errorf("got %s", b.String())
	}
	b.Reset()
	if err := Findings(&b, sample(), Checked{}, Options{JSON: true}); err != nil {
		t.Fatal(err)
	}
	if strings.Index(b.String(), `"#6"`) > strings.Index(b.String(), `"#1"`) {
		t.Error("JSON not sorted")
	}
}

func TestUnknownKindSortsLast(t *testing.T) {
	fs := []model.Finding{
		{Repo: "x", Item: "#1", Kind: "weird", Since: t0},
		{Repo: "y", Item: "#2", Kind: model.KindRetryInProgress, Since: t0},
	}
	var b bytes.Buffer
	if err := Findings(&b, fs, Checked{Repos: 2}, Options{}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(b.String(), "y #2") {
		t.Errorf("got %q", b.String())
	}
}

func TestTieBreakByRepoThenItem(t *testing.T) {
	fs := []model.Finding{
		{Repo: "b", Item: "#1", Kind: model.KindStuckPR, Since: t0},
		{Repo: "a", Item: "#2", Kind: model.KindStuckPR, Since: t0},
		{Repo: "a", Item: "#1", Kind: model.KindStuckPR, Since: t0},
	}
	var b bytes.Buffer
	if err := Findings(&b, fs, Checked{Repos: 2}, Options{}); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, l := range strings.Split(b.String(), "\n")[:3] {
		got = append(got, l[:4])
	}
	if strings.Join(got, ",") != "a #1,a #2,b #1" {
		t.Errorf("got %v", got)
	}
}

func TestSingularSummaryAndNowAndNoHTMLEscape(t *testing.T) {
	fs := []model.Finding{{Repo: "a", Item: "#1", Kind: model.KindStuckPR, Since: t0, Why: "a<b & c>d"}}
	var b bytes.Buffer
	if err := Findings(&b, fs, Checked{Repos: 1}, Options{}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(b.String(), "1 finding across 1 repos\n") {
		t.Errorf("got %q", b.String())
	}
	b.Reset()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := Findings(&b, fs, Checked{}, Options{JSON: true, Now: now}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), `"2026-01-02T03:04:05Z"`) || !strings.Contains(b.String(), "a<b & c>d") {
		t.Errorf("got %s", b.String())
	}
}
