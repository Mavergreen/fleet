package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const longJob = "a-very-long-job-name-that-keeps-going-and-going-past-any-sane-column"
const longRepo = "a-repo-with-a-name-long-enough-to-overflow-a-narrow-terminal"

// oneRun serves one repo (longRepo) with one completed run (id 9) whose one
// job has a long name.
type oneRun struct{}

func (oneRun) Get(_ context.Context, p string, out any) error {
	if strings.HasSuffix(p, "/actions/runs/9") {
		return json.Unmarshal([]byte(`{"id":9,"path":".github/workflows/release.yml","status":"completed","conclusion":"success","run_attempt":1,"created_at":"2026-11-18T00:00:00Z"}`), out)
	}
	return errors.New("unexpected Get " + p)
}
func (oneRun) GetPaged(_ context.Context, p string, each func(json.RawMessage) error) error {
	switch {
	case strings.HasPrefix(p, "/orgs/"):
		return each(json.RawMessage(`{"name":"` + longRepo + `"}`))
	case strings.Contains(p, "/jobs"):
		return each(json.RawMessage(`{"name":"` + longJob + `","run_attempt":1,"labels":["macos-26"],"created_at":"2026-11-18T00:00:00Z","started_at":"2026-11-18T00:10:00Z","completed_at":"2026-11-18T00:40:00Z","runner_name":"r","conclusion":"success","steps":[{"started_at":"2026-11-18T00:10:00Z"}]}`))
	}
	return each(json.RawMessage(`{"id":9,"path":".github/workflows/release.yml","status":"completed","conclusion":"success","run_attempt":1,"created_at":"2026-11-18T00:00:00Z"}`))
}
func (oneRun) GetRaw(context.Context, string) ([]byte, error) { return nil, errors.New("unused") }
func (oneRun) Patch(context.Context, string, any, any) error  { return errors.New("no writes") }
func (oneRun) Post(context.Context, string, any, any) error   { return errors.New("no writes") }

func widthEnv(t *testing.T, tty bool, columns string, termWidth int) (Env, *strings.Builder) {
	t.Helper()
	useClient(t, oneRun{})
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	e, _, _ := testEnv()
	var out strings.Builder
	e.Stdout = &out
	e.IsTTY = tty
	e.Width = func() int { return termWidth }
	e.Getenv = func(k string) string {
		switch k {
		case "GH_TOKEN":
			return "tok"
		case "COLUMNS":
			return columns
		}
		return ""
	}
	return e, &out
}

func maxLine(s string) int {
	n := 0
	for _, l := range strings.Split(s, "\n") {
		if m := len([]rune(strings.NewReplacer("\x1b[1m", "", "\x1b[0m", "").Replace(l))); m > n {
			n = m
		}
	}
	return n
}

// Off a terminal nothing is truncated, whatever COLUMNS says.
func TestTimingOffTTYNeverTruncates(t *testing.T) {
	for _, args := range [][]string{{"timing"}, {"timing", longRepo, "9"}} {
		e, out := widthEnv(t, false, "40", 0)
		if code := Run(args, e); code != 0 {
			t.Fatalf("%v: code %d: %s", args, code, out)
		}
		want := longRepo
		if len(args) == 3 {
			want = longJob
		}
		if !strings.Contains(out.String(), want) || strings.Contains(out.String(), "…") {
			t.Errorf("%v: truncated off a tty:\n%s", args, out)
		}
	}
}

// On a terminal the width comes from the terminal; COLUMNS overrides it.
func TestTimingWidthOnTTY(t *testing.T) {
	for _, c := range []struct {
		columns string
		term    int
		want    int
	}{{"", 60, 60}, {"70", 60, 70}} {
		for _, args := range [][]string{{"timing"}, {"timing", longRepo, "9"}} {
			e, out := widthEnv(t, true, c.columns, c.term)
			if code := Run(args, e); code != 0 {
				t.Fatalf("%v: code %d", args, code)
			}
			if n := maxLine(out.String()); n > c.want || !strings.Contains(out.String(), "…") {
				t.Errorf("%v COLUMNS=%q term=%d: widest line %d, want <= %d and truncated:\n%s", args, c.columns, c.term, n, c.want, out)
			}
		}
	}
}
