package attention

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mavergreen/fleet/internal/ghapi"
	"github.com/Mavergreen/fleet/internal/model"
)

type fake struct {
	routes map[string]string   // path -> testdata file, or "ERR500"
	pages  map[string][]string // path -> files, one per page (GetPaged only)
	mu     sync.Mutex
	bad    []string // runs requests that were not bounded single Gets
}

func (f *fake) flag(s string) {
	f.mu.Lock()
	f.bad = append(f.bad, s)
	f.mu.Unlock()
}

func (f *fake) body(path string) ([]byte, error) {
	name, ok := f.routes[path]
	if !ok {
		return nil, &ghapi.HTTPError{Status: 404, Path: path, Body: "not found"}
	}
	if name == "ERR500" {
		return nil, &ghapi.HTTPError{Status: 500, Path: path, Body: "boom"}
	}
	return os.ReadFile("testdata/" + name)
}
func (f *fake) Get(_ context.Context, p string, out any) error {
	if strings.Contains(p, "/runs") && !strings.Contains(p, "per_page=") {
		f.flag("Get without per_page: " + p)
	}
	b, err := f.body(p)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// items returns a bare array's elements, or the first array field of an envelope.
func items(b []byte) ([]json.RawMessage, error) {
	var arr []json.RawMessage
	if json.Unmarshal(b, &arr) == nil {
		return arr, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(b, &obj); err != nil {
		return nil, err
	}
	for _, v := range obj {
		if json.Unmarshal(v, &arr) == nil {
			return arr, nil
		}
	}
	return nil, nil
}

func (f *fake) GetPaged(_ context.Context, p string, each func(json.RawMessage) error) error {
	if strings.Contains(p, "/runs") {
		f.flag("GetPaged on runs: " + p)
	}
	var bodies [][]byte
	if files, ok := f.pages[p]; ok {
		for _, n := range files {
			b, err := os.ReadFile("testdata/" + n)
			if err != nil {
				return err
			}
			bodies = append(bodies, b)
		}
	} else {
		b, err := f.body(p)
		if err != nil {
			return err
		}
		bodies = append(bodies, b)
	}
	for _, b := range bodies {
		arr, err := items(b)
		if err != nil {
			return err
		}
		for _, it := range arr {
			if err := each(it); err != nil {
				return err
			}
		}
	}
	return nil
}
func (f *fake) GetRaw(context.Context, string) ([]byte, error) { return nil, nil }
func (f *fake) Patch(context.Context, string, any, any) error  { return nil }
func (f *fake) Post(context.Context, string, any, any) error   { return nil }

func routes() map[string]string {
	m := map[string]string{
		"/user": "user.json",
		"/orgs/Mavergreen/repos?type=public&per_page=100": "repos.json",

		"/repos/Mavergreen/rust/actions/workflows":                     "empty-workflows.json",
		"/repos/Mavergreen/rust/pulls?state=open":                      "rust-pulls.json",
		"/repos/Mavergreen/rust/commits/sha43/check-runs?per_page=100": "rust-checkruns-43.json",
		"/repos/Mavergreen/rust/commits/sha44/check-runs?per_page=100": "rust-checkruns-44.json",
		"/repos/Mavergreen/rust/commits/sha43/status":                  "rust-status-43.json",
		"/repos/Mavergreen/rust/commits/sha44/status":                  "rust-status-44.json",
		"/repos/Mavergreen/rust/issues?state=open":                     "rust-issues.json",
		"/repos/Mavergreen/rust/issues/50/comments?per_page=100":       "rust-comments-50.json",
		"/repos/Mavergreen/rust/issues/51/comments?per_page=100":       "rust-comments-51.json",

		"/repos/Mavergreen/mavergreen-site/actions/workflows": "site-workflows.json",
		"/repos/Mavergreen/mavergreen-site/pulls?state=open":  "empty-list.json",
		"/repos/Mavergreen/mavergreen-site/issues?state=open": "empty-list.json",

		"/repos/Mavergreen/flaky/actions/workflows": "empty-workflows.json",
		"/repos/Mavergreen/flaky/pulls?state=open":  "ERR500",
		"/repos/Mavergreen/flaky/issues?state=open": "empty-list.json",
	}
	site := "/repos/Mavergreen/mavergreen-site/actions/workflows/"
	for _, id := range []string{"1", "2", "3"} {
		m[site+id+"/runs?branch=main&per_page=30"] = "empty-runs.json"
		m[site+id+"/runs?event=schedule&per_page=30"] = "empty-runs.json"
	}
	m[site+"1/runs?event=schedule&per_page=30"] = "wf1-schedule-runs.json"
	m[site+"2/runs?event=schedule&per_page=30"] = "wf2-schedule-runs.json"
	m[site+"3/runs?branch=main&per_page=30"] = "wf3-main-runs.json"
	m[run11Jobs] = "run11-jobs.json"
	m[ann501] = "ann501.json"
	return m
}

var cnow = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

func collect(t *testing.T, r map[string]string) ([]model.Finding, int) {
	t.Helper()
	fs, checked, err := Collect(context.Background(), &fake{routes: r}, "Mavergreen", "", NoRetry{}, cnow)
	if err != nil {
		t.Fatal(err)
	}
	_ = checked
	return fs, 0
}

func byKind(fs []model.Finding, k model.Kind) []model.Finding {
	var out []model.Finding
	for _, f := range fs {
		if f.Kind == k {
			out = append(out, f)
		}
	}
	return out
}

func TestAttentionFindsStuckRenovatePR(t *testing.T) {
	fs, _ := collect(t, routes())
	got := byKind(fs, model.KindStuckPR)
	if len(got) != 1 || got[0].Item != "rust#43" || got[0].URL != "https://github.com/Mavergreen/rust/pull/43" {
		t.Fatalf("stuck: %+v", got)
	}
	if want := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC); !got[0].Since.Equal(want) {
		t.Fatalf("since %v", got[0].Since)
	}
	nr := byKind(fs, model.KindNeedsReply)
	if len(nr) != 1 || nr[0].Item != "rust#50" || !strings.Contains(nr[0].Why, "outsider") {
		t.Fatalf("needs-reply (bot last commenter must be ignored): %+v", nr)
	}
}

func TestAttentionFindsRedScheduledWorkflow(t *testing.T) {
	fs, _ := collect(t, routes())
	got := byKind(fs, model.KindRedWorkflow)
	if len(got) != 1 || !strings.Contains(got[0].Item, "reconcile.yml") || got[0].Repo != "mavergreen-site" {
		t.Fatalf("red-workflow: %+v", got)
	}
	if got[0].URL == "" {
		t.Fatal("no url")
	}
}

func TestAttentionSkipsArchivedRepos(t *testing.T) {
	r := routes()
	r["/repos/Mavergreen/old-thing/pulls?state=open"] = "ERR500"
	fs, _ := collect(t, r)
	for _, f := range fs {
		if f.Repo == "old-thing" {
			t.Fatalf("archived repo touched: %+v", f)
		}
	}
}

func TestAttentionReportsUnreadRepos(t *testing.T) {
	fs, _ := collect(t, routes())
	got := byKind(fs, model.KindCannotRead)
	if len(got) != 1 || got[0].Repo != "flaky" || got[0].Item != "/repos/Mavergreen/flaky/pulls?state=open" || !strings.Contains(got[0].Why, "500") {
		t.Fatalf("cannot-read: %+v", got)
	}
	if len(byKind(fs, model.KindStuckPR)) != 1 {
		t.Fatal("other repos must still be reported")
	}
}

func TestAttentionCountsChecked(t *testing.T) {
	_, checked, err := Collect(context.Background(), &fake{routes: routes()}, "Mavergreen", "", NoRetry{}, cnow)
	if err != nil {
		t.Fatal(err)
	}
	if checked.Repos != 3 || checked.PRs != 2 || checked.Issues != 2 || checked.Runs != 3 {
		t.Fatalf("checked %+v", checked)
	}
}

func TestAttentionDeterministicOrder(t *testing.T) {
	a, _ := collect(t, routes())
	for i := 0; i < 5; i++ {
		b, _ := collect(t, routes())
		if len(a) != len(b) {
			t.Fatal("length differs")
		}
		for j := range a {
			if a[j] != b[j] {
				t.Fatalf("order differs at %d", j)
			}
		}
	}
}

func TestDeletedOrDisabledWorkflowNotRed(t *testing.T) {
	r := routes()
	r["/repos/Mavergreen/mavergreen-site/actions/workflows"] = "site-workflows-disabled.json"
	fs, _ := collect(t, r)
	if got := byKind(fs, model.KindRedWorkflow); len(got) != 0 {
		t.Fatalf("disabled workflow reported: %+v", got)
	}
	// workflow 1 absent entirely (deleted)
	r["/repos/Mavergreen/mavergreen-site/actions/workflows"] = "empty-workflows.json"
	fs, _ = collect(t, r)
	if got := byKind(fs, model.KindRedWorkflow); len(got) != 0 {
		t.Fatalf("deleted workflow reported: %+v", got)
	}
}

func TestRunsQueryIsBounded(t *testing.T) {
	f := &fake{routes: routes()}
	if _, _, err := Collect(context.Background(), f, "Mavergreen", "", NoRetry{}, cnow); err != nil {
		t.Fatal(err)
	}
	if len(f.bad) != 0 {
		t.Fatalf("unbounded runs requests: %v", f.bad)
	}
}

func TestCheckRunsBeyondFirstPage(t *testing.T) {
	f := &fake{routes: routes(), pages: map[string][]string{
		"/repos/Mavergreen/rust/commits/sha44/check-runs?per_page=100": {"checkruns-44-page1.json", "checkruns-44-page2.json"},
	}}
	fs, _, err := Collect(context.Background(), f, "Mavergreen", "", NoRetry{}, cnow)
	if err != nil {
		t.Fatal(err)
	}
	// #44 went red at 07:00 today: red but under 24h, so not stuck; prove red via a later clock.
	fs2, _, _ := Collect(context.Background(), f, "Mavergreen", "", NoRetry{}, cnow.Add(48*time.Hour))
	found := false
	for _, x := range fs2 {
		if x.Item == "rust#44" && x.Kind == model.KindStuckPR && strings.HasPrefix(x.Why, "red") {
			found = true
		}
	}
	if !found {
		t.Fatalf("page-2 failure ignored; now: %+v later: %+v", fs, fs2)
	}
}

// stubRetry answers for rust#43 only.
type stubRetry struct {
	pattern  string
	ok       bool
	err      error
	progress []model.Finding
	asked    int
}

func (s *stubRetry) Retryable(_ context.Context, pr PR) (string, bool, error) {
	s.asked++
	if pr.Number != 43 {
		return "", false, nil
	}
	return s.pattern, s.ok, s.err
}
func (s *stubRetry) InProgress(_ context.Context, repo string) ([]model.Finding, error) {
	if repo != "rust" {
		return nil, nil
	}
	return s.progress, nil
}

func findingsFor(fs []model.Finding, item string) []model.Finding {
	var out []model.Finding
	for _, f := range fs {
		if f.Item == item {
			out = append(out, f)
		}
	}
	return out
}

func collectWith(t *testing.T, r Retry, routes map[string]string) []model.Finding {
	t.Helper()
	fs, _, err := Collect(context.Background(), &fake{routes: routes}, "Mavergreen", "", r, cnow)
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

func TestRetryableButUnretriedIsStuck(t *testing.T) {
	got := findingsFor(collectWith(t, &stubRetry{pattern: "runner-lost", ok: true}, routes()), "rust#43")
	if len(got) != 1 || got[0].Kind != model.KindStuckPR ||
		!strings.HasPrefix(got[0].Why, "red ") || !strings.HasSuffix(got[0].Why, ", retryable (runner-lost) — not yet retried") ||
		got[0].Fix != "run mavergreen-fleet remediate" {
		t.Fatalf("%+v", got)
	}
}

// A retry already requested speaks for the PR: no second, red-age stuck-pr.
// (A stale retry, "requested, no run started", likewise wins: the rule the
// deleted PR.RetryRequestedAt once encoded now lives in retry's prProgress.)
func TestRetryInProgressSuppressesRedAge(t *testing.T) {
	for _, k := range []model.Kind{model.KindRetryInProgress, model.KindStuckPR} {
		prog := model.Finding{Repo: "rust", Item: "rust#43", Kind: k, Since: cnow, Why: "retry 1/2"}
		s := &stubRetry{progress: []model.Finding{prog}}
		got := findingsFor(collectWith(t, s, routes()), "rust#43")
		if len(got) != 1 || got[0] != prog || s.asked != 0 {
			t.Fatalf("%s: %+v asked %d", k, got, s.asked)
		}
	}
}

// A verdict that could not be read is a cannot-read, and only that.
func TestRetryableErrorIsOnlyCannotRead(t *testing.T) {
	got := findingsFor(collectWith(t, &stubRetry{err: errors.New("boom")}, routes()), "retryable check for rust#43")
	all := collectWith(t, &stubRetry{err: errors.New("boom")}, routes())
	if len(got) != 1 || got[0].Kind != model.KindCannotRead || len(findingsFor(all, "rust#43")) != 0 {
		t.Fatalf("%+v / %+v", got, findingsFor(all, "rust#43"))
	}
}

// A head red only because a run awaits approval says so.
func TestActionRequiredSaysApprove(t *testing.T) {
	r := routes()
	r["/repos/Mavergreen/rust/commits/sha43/check-runs?per_page=100"] = "rust-checkruns-43-approval.json"
	s := &stubRetry{}
	got := findingsFor(collectWith(t, s, r), "rust#43")
	if len(got) != 1 || got[0].Kind != model.KindStuckPR || got[0].Fix != "approve the workflow run" {
		t.Fatalf("%+v", got)
	}
}

// Retryable is only asked when its answer can change a finding: red > 24h.
func TestRetryableAskedOnlyWhenItMatters(t *testing.T) {
	s := &stubRetry{}
	collectWith(t, s, routes())
	if s.asked != 1 { // rust#43 is red 16d; rust#44 is green
		t.Fatalf("asked %d times", s.asked)
	}
}

func redOne(t *testing.T, r map[string]string) model.Finding {
	t.Helper()
	fs, _ := collect(t, r)
	got := byKind(fs, model.KindRedWorkflow)
	if len(got) != 1 {
		t.Fatalf("red-workflow: %+v", got)
	}
	return got[0]
}

const wf1Sched = "/repos/Mavergreen/mavergreen-site/actions/workflows/1/runs?event=schedule&per_page=30"

func TestRedWorkflowStreak(t *testing.T) {
	r := routes()
	r[wf1Sched] = "wf1-schedule-streak.json"
	f := redOne(t, r)
	if !strings.HasPrefix(f.Why, "red 3 runs in a row, since Nov 15") {
		t.Fatalf("why %q", f.Why)
	}
	if want := time.Date(2026, 11, 15, 3, 10, 0, 0, time.UTC); !f.Since.Equal(want) {
		t.Fatalf("since %v", f.Since)
	}
}

func TestRedWorkflowStreakStopsAtCancelled(t *testing.T) {
	r := routes()
	r[wf1Sched] = "wf1-schedule-cancelled.json"
	if f := redOne(t, r); !strings.HasPrefix(f.Why, "red 1 run, since Nov 18") {
		t.Fatalf("why %q", f.Why)
	}
}

func TestRedWorkflowStreakCapped(t *testing.T) {
	r := routes()
	r[wf1Sched] = "wf1-schedule-30.json"
	if f := redOne(t, r); !strings.HasPrefix(f.Why, "red 30+ runs in a row, since Nov 3") {
		t.Fatalf("why %q", f.Why)
	}
}

const (
	run11Jobs = "/repos/Mavergreen/mavergreen-site/actions/runs/11/jobs?per_page=100"
	ann501    = "/repos/Mavergreen/mavergreen-site/check-runs/501/annotations"
)

func unreadNaming(fs []model.Finding, path string) bool {
	for _, f := range byKind(fs, model.KindCannotRead) {
		if strings.Contains(f.Item, path) || strings.Contains(f.Why, path) {
			return true
		}
	}
	return false
}

func TestRedWorkflowNamesFailingStep(t *testing.T) {
	if f := redOne(t, routes()); f.Fix != "reconcile › Derive upstream version: no src dir" {
		t.Fatalf("fix %q", f.Fix)
	}
}

func TestRedWorkflowStepWithoutAnnotation(t *testing.T) {
	r := routes()
	r[ann501] = "ann501-generic.json"
	if f := redOne(t, r); f.Fix != "reconcile › Derive upstream version" {
		t.Fatalf("fix %q", f.Fix)
	}
}

func TestRedWorkflowAnnotationFirstLine(t *testing.T) {
	r := routes()
	r[ann501] = "ann501-multiline.json"
	if f := redOne(t, r); f.Fix != "reconcile › Derive upstream version: a" {
		t.Fatalf("fix %q", f.Fix)
	}
}

func TestRedWorkflowNoFailedStep(t *testing.T) {
	r := routes()
	r[run11Jobs] = "run11-jobs-empty.json"
	fs, _ := collect(t, r)
	if f := byKind(fs, model.KindRedWorkflow); len(f) != 1 || f[0].Fix != "open the run, read the failing step" {
		t.Fatalf("red: %+v", f)
	}
	if unreadNaming(fs, "jobs") {
		t.Fatal("no failed step is not an unread")
	}
}

func TestRedWorkflowJobsUnreadable(t *testing.T) {
	r := routes()
	r[run11Jobs] = "ERR500"
	fs, _ := collect(t, r)
	if f := byKind(fs, model.KindRedWorkflow); len(f) != 1 || f[0].Fix != "open the run, read the failing step" {
		t.Fatalf("red: %+v", f)
	}
	if !unreadNaming(fs, "/actions/runs/11/jobs") {
		t.Fatalf("no cannot-read for jobs: %+v", byKind(fs, model.KindCannotRead))
	}
}

func TestRedWorkflowAnnotationsUnreadable(t *testing.T) {
	r := routes()
	r[ann501] = "ERR500"
	fs, _ := collect(t, r)
	if f := byKind(fs, model.KindRedWorkflow); len(f) != 1 || f[0].Fix != "reconcile › Derive upstream version" {
		t.Fatalf("red: %+v", f)
	}
	if !unreadNaming(fs, "/check-runs/501/annotations") {
		t.Fatalf("no cannot-read for annotations: %+v", byKind(fs, model.KindCannotRead))
	}
}
