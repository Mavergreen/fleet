package attention

import (
	"context"
	"encoding/json"
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
	if strings.Contains(p, "/runs") && !strings.Contains(p, "per_page=1") {
		f.flag("Get without per_page=1: " + p)
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
		m[site+id+"/runs?branch=main&per_page=1"] = "empty-runs.json"
		m[site+id+"/runs?event=schedule&per_page=1"] = "empty-runs.json"
	}
	m[site+"1/runs?event=schedule&per_page=1"] = "wf1-schedule-runs.json"
	m[site+"2/runs?event=schedule&per_page=1"] = "wf2-schedule-runs.json"
	m[site+"3/runs?branch=main&per_page=1"] = "wf3-main-runs.json"
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
