package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Mavergreen/fleet/internal/ghapi"
)

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"2026-10-19": time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC),
		"7d":         now.Add(-7 * 24 * time.Hour),
		"48h":        now.Add(-48 * time.Hour),
	} {
		if got, err := parseSince(in, now); err != nil || !got.Equal(want) {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "soon", "-3d", "0d"} {
		if _, err := parseSince(bad, now); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// unreadableJobs serves one repo with one run whose jobs cannot be read.
type unreadableJobs struct{}

func (unreadableJobs) Get(context.Context, string, any) error { return errors.New("unused") }
func (unreadableJobs) GetPaged(_ context.Context, p string, each func(json.RawMessage) error) error {
	switch {
	case strings.HasPrefix(p, "/orgs/"):
		return each(json.RawMessage(`{"name":"r"}`))
	case strings.Contains(p, "/jobs"):
		return &ghapi.HTTPError{Status: 500, Path: p}
	}
	return each(json.RawMessage(`{"id":9,"path":".github/workflows/release.yml","status":"completed","run_attempt":1,"created_at":"2026-11-18T00:00:00Z"}`))
}
func (unreadableJobs) GetRaw(context.Context, string) ([]byte, error) {
	return nil, errors.New("unused")
}
func (unreadableJobs) Patch(context.Context, string, any, any) error { return nil }
func (unreadableJobs) Post(context.Context, string, any, any) error  { return nil }

func TestTimingReportsUnreadableRuns(t *testing.T) {
	old := newClient
	newClient = func(string, io.Writer) ghapi.Client { return unreadableJobs{} }
	t.Cleanup(func() { newClient = old })
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	e, out, _ := testEnv()
	e.Getenv = func(k string) string {
		if k == "GH_TOKEN" {
			return "tok"
		}
		return ""
	}
	if code := Run([]string{"timing"}, e); code != 0 {
		t.Fatalf("code %d: %s", code, out)
	}
	if !strings.Contains(out.String(), "cannot read jobs for 1 runs in r\n") || !strings.Contains(out.String(), "cannot read: ") {
		t.Fatalf("%q", out.String())
	}
	if strings.Contains(out.String(), "\x1b") {
		t.Fatal("ANSI off tty")
	}
	out.Reset()
	if code := Run([]string{"--json", "timing"}, e); code != 0 {
		t.Fatal(code)
	}
	var env struct {
		Repos      []any          `json:"repos"`
		Unreadable map[string]int `json:"unreadable_runs"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil || env.Unreadable["r"] != 1 {
		t.Fatalf("%v %s", err, out)
	}
}

func TestTimingRejectsBadArgs(t *testing.T) {
	e, _, errb := testEnv()
	for _, args := range [][]string{{"timing", "--since", "soon"}, {"timing", "r", "x"}, {"timing", "--bogus"}, {"timing", "a", "1", "2"}} {
		if code := Run(args, e); code != 2 || !strings.Contains(errb.String(), "usage:") {
			t.Errorf("%v: code %d", args, code)
		}
	}
}

// noRuns serves a repo with no runs in the window.
type noRuns struct{ unreadableJobs }

func (noRuns) Get(_ context.Context, p string, out any) error {
	return json.Unmarshal([]byte(`{"workflow_runs":[]}`), out)
}

func TestTimingRepoWithNoRunsExits0(t *testing.T) {
	useClient(t, noRuns{})
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	e, out, _ := testEnv()
	e.Getenv = func(k string) string {
		if k == "GH_TOKEN" {
			return "tok"
		}
		return ""
	}
	if code := Run([]string{"timing", "r"}, e); code != 0 || !strings.Contains(out.String(), "no runs in r since") {
		t.Fatalf("text: code %d %q", code, out)
	}
	out.Reset()
	if code := Run([]string{"--json", "timing", "r"}, e); code != 0 {
		t.Fatalf("json: code %d %q", code, out)
	}
	var env struct {
		Repo string `json:"repo"`
		Note string `json:"note"`
		Jobs []any  `json:"jobs"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil || env.Repo != "r" || env.Jobs == nil || len(env.Jobs) != 0 ||
		!strings.Contains(env.Note, "no runs in r since") {
		t.Fatalf("%v %s", err, out)
	}
}

// quota is oneRun with a known X-RateLimit-Remaining.
type quota struct {
	oneRun
	left int
}

func (q quota) Remaining() int { return q.left }

// A cold-cache fleet timing that would eat the shared quota stops first.
func TestTimingRefusesToExhaustQuota(t *testing.T) {
	for _, c := range []struct {
		left  int
		force bool
		code  int
	}{{500, false, 1}, {500, true, 0}, {501, false, 0}, {-1, false, 0}} {
		useClient(t, quota{left: c.left})
		t.Setenv("XDG_CACHE_HOME", t.TempDir())
		t.Setenv("HOME", t.TempDir())
		e, out, errb := testEnv()
		e.Getenv = func(k string) string {
			if k == "GH_TOKEN" {
				return "tok"
			}
			return ""
		}
		args := []string{"timing"}
		if c.force {
			args = append(args, "--force")
		}
		code := Run(args, e)
		if code != c.code {
			t.Fatalf("%+v: code %d stdout %q stderr %q", c, code, out, errb)
		}
		const warn = "timing would use ~1 requests; only 500 left this hour (shared quota). Use --since to narrow, or wait.\n"
		if refused := code == 1; refused != strings.Contains(errb.String(), warn) || refused != (out.Len() == 0) {
			t.Fatalf("%+v: stdout %q stderr %q", c, out, errb)
		}
	}
}
