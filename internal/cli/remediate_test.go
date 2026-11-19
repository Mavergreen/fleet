package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Mavergreen/fleet/internal/ghapi"
	"github.com/Mavergreen/fleet/internal/retry"
)

// readOnly serves /user and one repo whose PRs and workflows cannot be read,
// and fails the test on any write.
type readOnly struct{ t *testing.T }

func (f readOnly) Get(_ context.Context, p string, out any) error {
	if p == "/user" {
		return json.Unmarshal([]byte(`{"login":"fleetbot"}`), out)
	}
	return &ghapi.HTTPError{Status: 500, Path: p}
}
func (f readOnly) GetPaged(_ context.Context, p string, each func(json.RawMessage) error) error {
	if strings.HasPrefix(p, "/orgs/") {
		return each(json.RawMessage(`{"name":"r"}`))
	}
	return &ghapi.HTTPError{Status: 500, Path: p}
}
func (f readOnly) GetRaw(context.Context, string) ([]byte, error) { return nil, errors.New("unused") }
func (f readOnly) Patch(_ context.Context, p string, _, _ any) error {
	f.t.Errorf("PATCH %s", p)
	return nil
}
func (f readOnly) Post(_ context.Context, p string, _, _ any) error {
	f.t.Errorf("POST %s", p)
	return nil
}

func TestRemediateDryRunReportsUnreadable(t *testing.T) {
	old := newClient
	newClient = func(string) ghapi.Client { return readOnly{t} }
	t.Cleanup(func() { newClient = old })
	e, out, _ := testEnv()
	e.Getenv = func(k string) string {
		if k == "GH_TOKEN" {
			return "tok"
		}
		return ""
	}
	if code := Run([]string{"remediate", "--dry-run"}, e); code != 0 {
		t.Fatalf("code %d", code)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "r -  report  cannot read") || !strings.HasSuffix(lines[0], "(dry run)") {
		t.Fatalf("%q", out.String())
	}
}

func TestRemediateRejectsUnknownArgs(t *testing.T) {
	e, _, errb := testEnv()
	if code := Run([]string{"remediate", "--force"}, e); code != 2 || !strings.Contains(errb.String(), "usage:") {
		t.Fatalf("code %d stderr %q", code, errb.String())
	}
}

func TestPrintRemediate(t *testing.T) {
	rs := []retry.Result{
		{Action: retry.Action{Kind: retry.KindTickBox, Repo: "rust", PR: 43, PatternID: "runner-lost", Attempt: 1}, Note: "retry 1/2, pattern runner-lost"},
		{Action: retry.Action{Kind: retry.KindReport, Repo: "cmake", RunID: 700}, Note: "reruns disabled until verified (Task 0)"},
		{Action: retry.Action{Kind: retry.KindTickBox, Repo: "go", PR: 5}, Note: "comment posted but tick failed", Err: errors.New("HTTP 422")},
	}
	var b bytes.Buffer
	if err := printRemediate(&b, rs, true, false, time.Time{}); err != nil {
		t.Fatal(err)
	}
	want := "rust PR #43  tick-box  retry 1/2, pattern runner-lost (dry run)\n" +
		"cmake run 700  report  reruns disabled until verified (Task 0) (dry run)\n" +
		"go PR #5  tick-box  comment posted but tick failed: HTTP 422 (dry run)\n"
	if b.String() != want {
		t.Fatalf("got\n%s", b.String())
	}
	b.Reset()
	if err := printRemediate(&b, rs, false, true, time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	var env struct {
		Command string `json:"command"`
		Actions []struct {
			Kind  string `json:"kind"`
			PR    int    `json:"pr"`
			Error string `json:"error"`
		} `json:"actions"`
	}
	if err := json.Unmarshal(b.Bytes(), &env); err != nil || env.Command != "remediate" || len(env.Actions) != 3 ||
		env.Actions[0].Kind != "tick-box" || env.Actions[0].PR != 43 || env.Actions[2].Error != "HTTP 422" {
		t.Fatalf("%v %s", err, b.String())
	}
	b.Reset()
	printRemediate(&b, nil, false, true, time.Time{})
	if !strings.Contains(b.String(), `"actions": []`) {
		t.Fatalf("%s", b.String())
	}
}

func TestRemediateJSONCarriesReadErrors(t *testing.T) {
	old := newClient
	newClient = func(string) ghapi.Client { return readOnly{t} }
	t.Cleanup(func() { newClient = old })
	e, out, _ := testEnv()
	e.Getenv = func(k string) string {
		if k == "GH_TOKEN" {
			return "tok"
		}
		return ""
	}
	if code := Run([]string{"--json", "remediate", "--dry-run"}, e); code != 0 {
		t.Fatalf("a read error is a report, not a failure: code %d", code)
	}
	var env struct {
		Actions []struct {
			Kind  string `json:"kind"`
			Error string `json:"error"`
		} `json:"actions"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil || len(env.Actions) != 2 {
		t.Fatalf("%v %s", err, out.String())
	}
	for _, a := range env.Actions {
		if a.Kind != "report" || !strings.Contains(a.Error, "500") {
			t.Fatalf("%s", out.String())
		}
	}
	// A plain report has no error field.
	var b bytes.Buffer
	printRemediate(&b, []retry.Result{{Action: retry.Action{Kind: retry.KindReport, Repo: "r", PR: 1}, Note: "fork PR: report-only"}}, false, true, time.Time{})
	if strings.Contains(b.String(), `"error"`) {
		t.Fatalf("%s", b.String())
	}
}
