package retry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mavergreen/fleet/internal/attention"
	"github.com/Mavergreen/fleet/internal/ghapi"
	"github.com/Mavergreen/fleet/internal/model"
)

var now = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

const me = "fleetbot"

func ts(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }

type write struct {
	Method, Path string
	Body         any
}

// remFake is an in-memory GitHub. Route values are JSON-marshalled, except
// []byte (raw, for logs), error (returned), and func() any (evaluated per call).
type remFake struct {
	t      *testing.T
	mu     sync.Mutex
	routes map[string]any
	writes []write
	fail   map[string]error // write path -> error to answer with
	// afterWrite, if set, runs after each recorded write.
	afterWrite func(w write)
}

func newFake(t *testing.T) *remFake {
	return &remFake{t: t, fail: map[string]error{}, routes: map[string]any{
		"/user":                                  map[string]any{"login": me},
		"/orgs/o/repos?type=public&per_page=100": []any{map[string]any{"name": "r"}},
		"/repos/o/r/pulls?state=open":            []any{},
		"/repos/o/r/actions/workflows":           map[string]any{"workflows": []any{}},
		"/repos/o/r/contents/" + declaredPath:    &ghapi.HTTPError{Status: 404, Path: "contents"},
	}}
}

func (f *remFake) set(path string, v any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[path] = v
}

func (f *remFake) lookup(p string) (any, error) {
	f.mu.Lock()
	v, ok := f.routes[p]
	f.mu.Unlock()
	if !ok {
		return nil, &ghapi.HTTPError{Status: 404, Path: p, Body: "not found"}
	}
	if fn, ok := v.(func() any); ok {
		v = fn()
	}
	if err, ok := v.(error); ok {
		return nil, err
	}
	return v, nil
}

func (f *remFake) Get(_ context.Context, p string, out any) error {
	v, err := f.lookup(p)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(v)
	return json.Unmarshal(b, out)
}

func (f *remFake) GetPaged(_ context.Context, p string, each func(json.RawMessage) error) error {
	v, err := f.lookup(p)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(v)
	var arr []json.RawMessage
	if json.Unmarshal(b, &arr) != nil {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(b, &obj); err != nil {
			return err
		}
		for _, x := range obj {
			if json.Unmarshal(x, &arr) == nil {
				break
			}
		}
	}
	for _, r := range arr {
		if err := each(r); err != nil {
			return err
		}
	}
	return nil
}

func (f *remFake) GetRaw(_ context.Context, p string) ([]byte, error) {
	v, err := f.lookup(p)
	if err != nil {
		return nil, err
	}
	if b, ok := v.([]byte); ok {
		return b, nil
	}
	return json.Marshal(v)
}

func (f *remFake) record(method, p string, body any) error {
	if strings.Contains(p, "cancel") || method == "DELETE" {
		f.t.Errorf("forbidden write %s %s", method, p)
	}
	w := write{method, p, body}
	f.mu.Lock()
	f.writes = append(f.writes, w)
	err := f.fail[p]
	f.mu.Unlock()
	if f.afterWrite != nil {
		f.afterWrite(w)
	}
	return err
}

func (f *remFake) Patch(_ context.Context, p string, body, _ any) error {
	return f.record("PATCH", p, body)
}
func (f *remFake) Post(_ context.Context, p string, body, _ any) error {
	return f.record("POST", p, body)
}

func (f *remFake) allWrites() []write {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]write{}, f.writes...)
}

// --- fixtures ---

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func renovateBody(t *testing.T) string { return string(readFixture(t, "renovate-body.md")) }

type prSpec struct {
	n           int
	ref, title  string
	body        string
	sha         string
	fork        bool
	checksState string // "failure" (default) or "success"
}

func addPR(f *remFake, p prSpec) {
	if p.checksState == "" {
		p.checksState = "failure"
	}
	headRepo := map[string]any{"full_name": "o/r"}
	if p.fork {
		headRepo = map[string]any{"full_name": "someone/r"}
	}
	pull := map[string]any{
		"number": p.n, "html_url": fmt.Sprintf("https://github.com/o/r/pull/%d", p.n),
		"title": p.title, "body": p.body, "state": "open", "created_at": ts(-72 * time.Hour),
		"user": map[string]any{"login": "renovate[bot]", "type": "Bot"},
		"head": map[string]any{"ref": p.ref, "sha": p.sha, "repo": headRepo},
		"base": map[string]any{"repo": map[string]any{"full_name": "o/r"}},
	}
	list, _ := f.routes["/repos/o/r/pulls?state=open"].([]any)
	f.routes["/repos/o/r/pulls?state=open"] = append(list, pull)
	f.routes[fmt.Sprintf("/repos/o/r/pulls/%d", p.n)] = pull
	f.routes["/repos/o/r/commits/"+p.sha+"/check-runs?per_page=100"] = map[string]any{"check_runs": []any{
		map[string]any{"status": "completed", "conclusion": p.checksState, "started_at": ts(-3 * time.Hour), "completed_at": ts(-2 * time.Hour)},
	}}
	f.routes["/repos/o/r/commits/"+p.sha+"/status"] = map[string]any{"statuses": []any{}}
	if _, ok := f.routes[fmt.Sprintf("/repos/o/r/issues/%d/comments?per_page=100", p.n)]; !ok {
		f.routes[fmt.Sprintf("/repos/o/r/issues/%d/comments?per_page=100", p.n)] = []any{}
	}
}

type jobSpec struct {
	id         int64
	name       string
	conclusion string // default failure
	noRunner   bool
	created    time.Duration // relative to now; default -3h
	log        any           // []byte, error, or nil for "no route"
	annots     []any
}

type runSpec struct {
	id         int64
	wid        int64
	event      string
	branch     string
	sha        string
	status     string // default completed
	conclusion string // default failure
	attempt    int    // default 1
	updated    time.Duration
	jobs       []jobSpec
}

func runJSON(r runSpec) map[string]any {
	if r.status == "" {
		r.status = "completed"
	}
	if r.conclusion == "" && r.status == "completed" {
		r.conclusion = "failure"
	}
	if r.attempt == 0 {
		r.attempt = 1
	}
	if r.wid == 0 {
		r.wid = 7
	}
	if r.updated == 0 {
		r.updated = -2 * time.Hour
	}
	return map[string]any{
		"id": r.id, "workflow_id": r.wid, "name": "CI", "path": ".github/workflows/ci.yml",
		"event": r.event, "head_branch": r.branch, "head_sha": r.sha, "status": r.status,
		"conclusion": r.conclusion, "run_attempt": r.attempt, "created_at": ts(-4 * time.Hour),
		"updated_at": ts(r.updated), "html_url": fmt.Sprintf("https://github.com/o/r/actions/runs/%d", r.id),
		"check_suite_id": 10000 + r.id,
	}
}

// addRun registers the run, its jobs, logs and annotations, and makes it the
// newest run of its workflow for its head SHA.
func addRun(f *remFake, r runSpec) map[string]any {
	rj := runJSON(r)
	f.routes[fmt.Sprintf("/repos/o/r/actions/runs/%d", r.id)] = rj
	f.routes[fmt.Sprintf("/repos/o/r/actions/workflows/%d/runs?head_sha=%s&per_page=1", rj["workflow_id"], r.sha)] =
		map[string]any{"workflow_runs": []any{rj}}
	var jobs []any
	for _, j := range r.jobs {
		if j.conclusion == "" {
			j.conclusion = "failure"
		}
		if j.created == 0 {
			j.created = -3 * time.Hour
		}
		job := map[string]any{"id": j.id, "name": j.name, "conclusion": j.conclusion, "status": "completed",
			"created_at": ts(j.created)}
		if j.noRunner {
			job["runner_name"] = nil
			job["steps"] = []any{}
		} else {
			job["runner_name"] = "GitHub Actions 3"
			job["steps"] = []any{map[string]any{"name": "build", "status": "completed", "conclusion": j.conclusion}}
		}
		jobs = append(jobs, job)
		if j.log != nil {
			f.routes[fmt.Sprintf("/repos/o/r/actions/jobs/%d/logs", j.id)] = j.log
		}
		if j.annots == nil {
			j.annots = []any{}
		}
		f.routes[fmt.Sprintf("/repos/o/r/check-runs/%d/annotations", j.id)] = j.annots
	}
	f.routes[fmt.Sprintf("/repos/o/r/actions/runs/%d/jobs?per_page=100", r.id)] = map[string]any{"jobs": jobs}
	return rj
}

// addHeadRuns lists runs on a PR head, newest first.
func addHeadRuns(f *remFake, sha string, runs ...map[string]any) {
	var l []any
	for _, r := range runs {
		l = append(l, r)
	}
	f.routes["/repos/o/r/actions/runs?head_sha="+sha+"&per_page=100"] = map[string]any{"workflow_runs": l}
	// The head's check runs are the ones these runs reported.
	var crs []any
	for _, r := range runs {
		crs = append(crs, map[string]any{"name": fmt.Sprintf("job of run %v", r["id"]),
			"check_suite": map[string]any{"id": r["check_suite_id"]}, "status": r["status"],
			"conclusion": r["conclusion"], "started_at": ts(-3 * time.Hour), "completed_at": ts(-2 * time.Hour)})
	}
	f.routes["/repos/o/r/commits/"+sha+"/check-runs?per_page=100"] = map[string]any{"check_runs": crs}
}

// addMainRun makes r the newest run on main of workflow r.wid.
func addMainRun(f *remFake, r runSpec) map[string]any {
	if r.wid == 0 {
		r.wid = 7
	}
	rj := addRun(f, r)
	wf, _ := f.routes["/repos/o/r/actions/workflows"].(map[string]any)
	l, _ := wf["workflows"].([]any)
	f.routes["/repos/o/r/actions/workflows"] = map[string]any{"workflows": append(l,
		map[string]any{"id": r.wid, "name": "CI", "state": "active"})}
	f.routes[fmt.Sprintf("/repos/o/r/actions/workflows/%d/runs?branch=main&per_page=1", r.wid)] = map[string]any{"workflow_runs": []any{rj}}
	f.routes[fmt.Sprintf("/repos/o/r/actions/workflows/%d/runs?event=schedule&per_page=1", r.wid)] = map[string]any{"workflow_runs": []any{}}
	return rj
}

func comment(login, body string, at time.Duration) map[string]any {
	return map[string]any{"user": map[string]any{"login": login}, "body": body, "created_at": ts(at)}
}

const renovateTitle = "Update rust-lang/rust to v1.100.0"

// renovateWorld: PR #43 on renovate/rust, red from a runner-lost job.
func renovateWorld(t *testing.T) *remFake {
	f := newFake(t)
	addPR(f, prSpec{n: 43, ref: "renovate/rust", title: renovateTitle, body: renovateBody(t), sha: "abc"})
	rj := addRun(f, runSpec{id: 900, event: "pull_request", branch: "renovate/rust", sha: "abc",
		jobs: []jobSpec{{id: 5001, name: "build-native", log: readFixture(t, "logs/runner-lost.txt")}}})
	addHeadRuns(f, "abc", rj)
	return f
}

// mainWorld: newest main run 700 failed with a network-fetch give-up.
func mainWorld(t *testing.T, attempt int) *remFake {
	f := newFake(t)
	addMainRun(f, runSpec{id: 700, event: "push", branch: "main", sha: "m1", attempt: attempt,
		jobs: []jobSpec{{id: 6001, name: "build-native", log: readFixture(t, "logs/network-fetch.txt")}}})
	return f
}

func plan(t *testing.T, f *remFake) []Action {
	t.Helper()
	as, err := Plan(context.Background(), f, "o", t.TempDir(), now)
	if err != nil {
		t.Fatal(err)
	}
	return as
}

func only(t *testing.T, as []Action) Action {
	t.Helper()
	if len(as) != 1 {
		t.Fatalf("want 1 action, got %d: %+v", len(as), as)
	}
	return as[0]
}

func enableReruns(t *testing.T) { setReruns(t, true) }

func setReruns(t *testing.T, on bool) {
	old := rerunsEnabled
	rerunsEnabled = on
	t.Cleanup(func() { rerunsEnabled = old })
}

// --- tests ---

func TestRerunsWorkVerifiedByTask0(t *testing.T) {
	if !RerunsWork || !rerunsEnabled {
		t.Fatal("Task 0 proved a re-run passes install@v1 (2026-11-19); reruns are on")
	}
}

func TestRenovateRunGetsTickNotRerun(t *testing.T) {
	f := renovateWorld(t)
	a := only(t, plan(t, f))
	if a.Kind != KindTickBox || a.PR != 43 || a.PatternID != "runner-lost" || a.Attempt != 1 {
		t.Fatalf("action %+v", a)
	}
	r := Apply(context.Background(), f, a, false)
	if r.Err != nil || !r.Done {
		t.Fatalf("result %+v", r)
	}
	ws := f.allWrites()
	if len(ws) != 2 {
		t.Fatalf("writes %+v", ws)
	}
	if ws[0].Method != "POST" || ws[0].Path != "/repos/o/r/issues/43/comments" {
		t.Fatalf("first write must be the marker comment: %+v", ws[0])
	}
	cb := ws[0].Body.(map[string]string)["body"]
	if CountRetries([]string{cb}, UpdateKey(renovateTitle)) != 1 || !strings.Contains(cb, "runs/900") {
		t.Fatalf("comment %q", cb)
	}
	if ws[1].Method != "PATCH" || ws[1].Path != "/repos/o/r/pulls/43" {
		t.Fatalf("second write must tick the box: %+v", ws[1])
	}
	pb := ws[1].Body.(map[string]string)
	if len(pb) != 1 {
		t.Fatalf("PATCH must carry only the body: %+v", pb)
	}
	want, st := TickRebaseBox(renovateBody(t))
	if st != BoxTicked || pb["body"] != want {
		t.Fatalf("patched body is not the ticked body")
	}
	for _, w := range ws {
		if strings.Contains(w.Path, "rerun") {
			t.Fatalf("renovate run re-run: %+v", w)
		}
	}
}

func TestMainRunGetsRerun(t *testing.T) {
	enableReruns(t)
	f := mainWorld(t, 1)
	a := only(t, plan(t, f))
	if a.Kind != KindRerun || a.RunID != 700 || a.PatternID != "network-fetch" || a.Attempt != 1 {
		t.Fatalf("action %+v", a)
	}
	r := Apply(context.Background(), f, a, false)
	if r.Err != nil || !r.Done {
		t.Fatalf("result %+v", r)
	}
	ws := f.allWrites()
	if len(ws) != 1 || ws[0].Method != "POST" || ws[0].Path != "/repos/o/r/actions/runs/700/rerun-failed-jobs" {
		t.Fatalf("writes %+v", ws)
	}
}

func TestRerunDisabledReportsOnly(t *testing.T) {
	setReruns(t, false)
	f := mainWorld(t, 1)
	a := only(t, plan(t, f))
	if a.Kind != KindReport || a.Note != "reruns disabled until verified (Task 0)" || a.PatternID != "network-fetch" {
		t.Fatalf("action %+v", a)
	}
	// Apply refuses a rerun even if handed one while reruns are disabled.
	a.Kind = KindRerun
	r := Apply(context.Background(), f, a, false)
	if r.Done || len(f.allWrites()) != 0 {
		t.Fatalf("result %+v writes %+v", r, f.allWrites())
	}
}

func TestRerun403SuggestsCutRelease(t *testing.T) {
	enableReruns(t)
	f := mainWorld(t, 1)
	f.fail["/repos/o/r/actions/runs/700/rerun-failed-jobs"] = &ghapi.HTTPError{Status: 403, Path: "rerun", Body: "workflow file changed"}
	a := only(t, plan(t, f))
	r := Apply(context.Background(), f, a, false)
	if r.Done || r.Note != "re-dispatch with cut-release.sh" || r.Action.Kind != KindReport {
		t.Fatalf("result %+v", r)
	}
}

func TestFeatureBranchAndForkReportOnly(t *testing.T) {
	f := newFake(t)
	addPR(f, prSpec{n: 10, ref: "feature/x", title: "x", body: renovateBody(t), sha: "f1"})
	addPR(f, prSpec{n: 11, ref: "renovate/evil", title: renovateTitle, body: renovateBody(t), sha: "f2", fork: true})
	for _, sha := range []string{"f1", "f2"} {
		rj := addRun(f, runSpec{id: map[string]int64{"f1": 1, "f2": 2}[sha], event: "pull_request", sha: sha,
			jobs: []jobSpec{{id: 1, name: "b", log: readFixture(t, "logs/runner-lost.txt")}}})
		addHeadRuns(f, sha, rj)
	}
	as := plan(t, f)
	if len(as) != 2 {
		t.Fatalf("%+v", as)
	}
	for _, a := range as {
		if a.Kind != KindReport {
			t.Fatalf("not report-only: %+v", a)
		}
		Apply(context.Background(), f, a, false)
	}
	if !strings.Contains(as[0].Note, "not a Renovate branch") || !strings.Contains(as[1].Note, "fork") {
		t.Fatalf("notes %q / %q", as[0].Note, as[1].Note)
	}
	if ws := f.allWrites(); len(ws) != 0 {
		t.Fatalf("writes %+v", ws)
	}
}

func TestCapReachedReports(t *testing.T) {
	f := renovateWorld(t)
	upd := UpdateKey(renovateTitle)
	f.routes["/repos/o/r/issues/43/comments?per_page=100"] = []any{
		comment(me, Comment(1, Cap, "runner-lost", "u", upd), -48*time.Hour),
		comment(me, Comment(2, Cap, "runner-lost", "u", upd), -24*time.Hour),
	}
	a := only(t, plan(t, f))
	if a.Kind != KindReport || a.Note != "retries exhausted (2/2)" {
		t.Fatalf("PR: %+v", a)
	}

	enableReruns(t)
	m := mainWorld(t, 3)
	a = only(t, plan(t, m))
	if a.Kind != KindReport || a.Note != "retries exhausted (2/2)" {
		t.Fatalf("run: %+v", a)
	}
	m2 := mainWorld(t, 2)
	if a := only(t, plan(t, m2)); a.Kind != KindRerun || a.Attempt != 2 {
		t.Fatalf("attempt 2 should still rerun: %+v", a)
	}
}

func TestSpoofedMarkersDoNotCount(t *testing.T) {
	f := renovateWorld(t)
	upd := UpdateKey(renovateTitle)
	f.routes["/repos/o/r/issues/43/comments?per_page=100"] = []any{
		comment("mallory", Comment(1, Cap, "runner-lost", "u", upd), -48*time.Hour),
		comment("mallory", Comment(2, Cap, "runner-lost", "u", upd), -24*time.Hour),
		comment(me, Comment(1, Cap, "runner-lost", "u", upd), -20*time.Hour),
	}
	a := only(t, plan(t, f))
	if a.Kind != KindTickBox || a.Attempt != 2 {
		t.Fatalf("%+v", a)
	}
}

func TestNewUpdateResetsCap(t *testing.T) {
	f := renovateWorld(t)
	old := UpdateKey("Update rust-lang/rust to v1.99.1")
	f.routes["/repos/o/r/issues/43/comments?per_page=100"] = []any{
		comment(me, Comment(1, Cap, "runner-lost", "u", old), -48*time.Hour),
		comment(me, Comment(2, Cap, "runner-lost", "u", old), -24*time.Hour),
	}
	if a := only(t, plan(t, f)); a.Kind != KindTickBox || a.Attempt != 1 {
		t.Fatalf("%+v", a)
	}
}

func TestBoxAbsentOrAlreadyTickedReports(t *testing.T) {
	f := renovateWorld(t)
	pull := f.routes["/repos/o/r/pulls/43"].(map[string]any)
	pull["body"] = "no box here"
	f.routes["/repos/o/r/pulls?state=open"] = []any{pull}
	if a := only(t, plan(t, f)); a.Kind != KindReport || a.Note != "no rebase/retry checkbox" {
		t.Fatalf("%+v", a)
	}
	ticked, _ := TickRebaseBox(renovateBody(t))
	pull["body"] = ticked
	if a := only(t, plan(t, f)); a.Kind != KindReport || a.Note != "already requested" {
		t.Fatalf("%+v", a)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	enableReruns(t)
	f := renovateWorld(t)
	addMainRun(f, runSpec{id: 700, event: "push", branch: "main", sha: "m1",
		jobs: []jobSpec{{id: 6001, name: "build-native", log: readFixture(t, "logs/network-fetch.txt")}}})
	as := plan(t, f)
	if len(as) != 2 {
		t.Fatalf("%+v", as)
	}
	for _, a := range as {
		r := Apply(context.Background(), f, a, true)
		if r.Done || r.Err != nil || r.Action.Kind != a.Kind {
			t.Fatalf("dry run result %+v", r)
		}
	}
	if ws := f.allWrites(); len(ws) != 0 {
		t.Fatalf("dry run wrote %+v", ws)
	}
}

func TestApplyRereadsStateBeforeActing(t *testing.T) {
	f := renovateWorld(t)
	a := only(t, plan(t, f))
	if a.Kind != KindTickBox {
		t.Fatalf("%+v", a)
	}
	// Another agent acts between Plan and Apply: ticks the box, posts a marker.
	pull := f.routes["/repos/o/r/pulls/43"].(map[string]any)
	ticked, _ := TickRebaseBox(renovateBody(t))
	f.set("/repos/o/r/pulls/43", map[string]any{"number": 43, "title": renovateTitle, "body": ticked,
		"state": "open", "head": pull["head"], "base": pull["base"]})
	f.set("/repos/o/r/issues/43/comments?per_page=100", []any{
		comment(me, Comment(1, Cap, "runner-lost", "u", UpdateKey(renovateTitle)), -time.Minute)})
	r := Apply(context.Background(), f, a, false)
	if r.Done || r.Note != "already requested" {
		t.Fatalf("result %+v", r)
	}
	if ws := f.allWrites(); len(ws) != 0 {
		t.Fatalf("wrote %+v", ws)
	}

	// Each change alone is enough.
	for name, mutate := range map[string]func(f *remFake){
		"marker only": func(f *remFake) {
			f.set("/repos/o/r/issues/43/comments?per_page=100", []any{
				comment(me, Comment(1, Cap, "runner-lost", "u", UpdateKey(renovateTitle)), -time.Minute)})
		},
		"box only": func(f *remFake) {
			p := f.routes["/repos/o/r/pulls/43"].(map[string]any)
			q := map[string]any{}
			for k, v := range p {
				q[k] = v
			}
			q["body"] = ticked
			f.set("/repos/o/r/pulls/43", q)
		},
		"head moved": func(f *remFake) {
			p := f.routes["/repos/o/r/pulls/43"].(map[string]any)
			q := map[string]any{}
			for k, v := range p {
				q[k] = v
			}
			q["head"] = map[string]any{"ref": "renovate/rust", "sha": "def", "repo": map[string]any{"full_name": "o/r"}}
			f.set("/repos/o/r/pulls/43", q)
		},
	} {
		f := renovateWorld(t)
		a := only(t, plan(t, f))
		mutate(f)
		if r := Apply(context.Background(), f, a, false); r.Done || r.Note != "already requested" || len(f.allWrites()) != 0 {
			t.Errorf("%s: result %+v writes %+v", name, r, f.allWrites())
		}
	}
}

func TestApplyRereadsRunState(t *testing.T) {
	enableReruns(t)
	for name, c := range map[string]struct {
		run  map[string]any
		note string
	}{
		"in progress":       {map[string]any{"id": 700, "status": "in_progress", "run_attempt": 2, "head_branch": "main"}, "run in progress"},
		"now green":         {map[string]any{"id": 700, "status": "completed", "conclusion": "success", "run_attempt": 2, "head_branch": "main"}, "run no longer failed"},
		"re-run by another": {map[string]any{"id": 700, "status": "completed", "conclusion": "failure", "run_attempt": 2, "head_branch": "main"}, "already requested"},
	} {
		f := mainWorld(t, 1)
		a := only(t, plan(t, f))
		f.set("/repos/o/r/actions/runs/700", c.run)
		if r := Apply(context.Background(), f, a, false); r.Done || r.Note != c.note || len(f.allWrites()) != 0 {
			t.Errorf("%s: result %+v writes %+v", name, r, f.allWrites())
		}
	}
	// A tick-box also does nothing while the run it was planned on is re-running.
	f := renovateWorld(t)
	a := only(t, plan(t, f))
	queued := runJSON(runSpec{id: 900, event: "pull_request", branch: "renovate/rust", sha: "abc", status: "queued"})
	f.set("/repos/o/r/actions/runs?head_sha=abc&per_page=100", map[string]any{"workflow_runs": []any{queued}})
	if r := Apply(context.Background(), f, a, false); r.Done || r.Note != "run in progress" || len(f.allWrites()) != 0 {
		t.Errorf("tick-box: result %+v", r)
	}
}

func TestTickFailureAfterCommentIsAnError(t *testing.T) {
	f := renovateWorld(t)
	f.fail["/repos/o/r/pulls/43"] = &ghapi.HTTPError{Status: 422, Path: "/repos/o/r/pulls/43"}
	a := only(t, plan(t, f))
	r := Apply(context.Background(), f, a, false)
	if r.Err == nil || !strings.Contains(r.Note, "comment posted") || !strings.Contains(r.Note, "tick failed") {
		t.Fatalf("result %+v", r)
	}
	// The posted comment counts: the next plan is attempt 2, not attempt 1 again.
	var bodies []any
	for _, w := range f.allWrites() {
		if w.Method == "POST" {
			bodies = append(bodies, comment(me, w.Body.(map[string]string)["body"], 0))
		}
	}
	f.routes["/repos/o/r/issues/43/comments?per_page=100"] = bodies
	if a := only(t, plan(t, f)); a.Attempt != 2 {
		t.Fatalf("%+v", a)
	}
}

func TestCommentFailureTicksNothing(t *testing.T) {
	f := renovateWorld(t)
	f.fail["/repos/o/r/issues/43/comments"] = &ghapi.HTTPError{Status: 500, Path: "comments"}
	r := Apply(context.Background(), f, only(t, plan(t, f)), false)
	if r.Err == nil || r.Done {
		t.Fatalf("result %+v", r)
	}
	for _, w := range f.allWrites() {
		if w.Method == "PATCH" {
			t.Fatalf("ticked after the comment failed")
		}
	}
}

func TestUnmatchedJobReportsAndNamesIt(t *testing.T) {
	f := newFake(t)
	addPR(f, prSpec{n: 43, ref: "renovate/rust", title: renovateTitle, body: renovateBody(t), sha: "abc"})
	rj := addRun(f, runSpec{id: 900, event: "pull_request", branch: "renovate/rust", sha: "abc", jobs: []jobSpec{
		{id: 1, name: "on-mavericks", log: readFixture(t, "logs/runner-lost.txt")},
		{id: 2, name: "on-linux", log: readFixture(t, "logs/build-error.txt")},
	}})
	addHeadRuns(f, "abc", rj)
	a := only(t, plan(t, f))
	if a.Kind != KindReport || !strings.Contains(a.Note, `"on-linux"`) {
		t.Fatalf("%+v", a)
	}
}

func TestExpiredOrForbiddenLogsReport(t *testing.T) {
	for status, want := range map[int]string{404: "logs expired", 410: "logs expired", 403: "cannot read logs"} {
		f := newFake(t)
		addPR(f, prSpec{n: 43, ref: "renovate/rust", title: renovateTitle, body: renovateBody(t), sha: "abc"})
		rj := addRun(f, runSpec{id: 900, event: "pull_request", branch: "renovate/rust", sha: "abc", jobs: []jobSpec{
			{id: 1, name: "b", log: &ghapi.HTTPError{Status: status, Path: "logs"},
				annots: []any{map[string]any{"annotation_level": "failure", "message": "The hosted runner lost communication with the server."}}},
		}})
		addHeadRuns(f, "abc", rj)
		if a := only(t, plan(t, f)); a.Kind != KindReport || !strings.Contains(a.Note, want) {
			t.Errorf("%d: %+v", status, a)
		}
	}
}

func TestAnnotationsFeedTheClassifier(t *testing.T) {
	quiet := []byte("build-native\tUNKNOWN STEP\t2026-11-13T01:12:03Z    Compiling syn v2.0.110\n")
	lost := map[string]any{"annotation_level": "failure", "message": "The hosted runner lost communication with the server. Anything in your workflow that terminates the runner process can cause this error."}
	warn := map[string]any{"annotation_level": "warning", "message": "Node.js 16 actions are deprecated."}
	real := map[string]any{"annotation_level": "failure", "message": "test foo failed"}
	for name, c := range map[string]struct {
		annots []any
		kind   ActionKind
	}{
		"runner-lost annotation":          {[]any{lost}, KindTickBox},
		"warnings are ignored":            {[]any{lost, warn}, KindTickBox},
		"a later real error blocks retry": {[]any{lost, real}, KindReport},
		"no annotation, no pattern":       {[]any{}, KindReport},
	} {
		f := newFake(t)
		addPR(f, prSpec{n: 43, ref: "renovate/rust", title: renovateTitle, body: renovateBody(t), sha: "abc"})
		rj := addRun(f, runSpec{id: 900, event: "pull_request", branch: "renovate/rust", sha: "abc",
			jobs: []jobSpec{{id: 1, name: "b", log: quiet, annots: c.annots}}})
		addHeadRuns(f, "abc", rj)
		if a := only(t, plan(t, f)); a.Kind != c.kind {
			t.Errorf("%s: %+v", name, a)
		}
	}
}

func TestNeverStartedGate(t *testing.T) {
	const deliberate = "cancelled — not retrying a possibly deliberate cancel"
	enableReruns(t)
	world := func(created time.Duration) *remFake {
		f := newFake(t)
		addMainRun(f, runSpec{id: 700, event: "push", branch: "main", sha: "m1", conclusion: "cancelled",
			jobs: []jobSpec{{id: 1, name: "on-mavericks", conclusion: "cancelled", noRunner: true, created: created}}})
		return f
	}
	// Starved for a day: retry. No log is fetched for a job that never started.
	if a := only(t, plan(t, world(-26*time.Hour))); a.Kind != KindRerun || a.PatternID != "never-started" {
		t.Fatalf("starved: %+v", a)
	}
	// Cancelled within the hour: maybe deliberate.
	if a := only(t, plan(t, world(-2*time.Hour-30*time.Minute))); a.Kind != KindReport || a.Note != deliberate {
		t.Fatalf("quick cancel: %+v", a)
	}
	// Superseded by a newer run of the same workflow on the same SHA.
	f := world(-26 * time.Hour)
	f.routes["/repos/o/r/actions/workflows/7/runs?head_sha=m1&per_page=1"] = map[string]any{"workflow_runs": []any{
		runJSON(runSpec{id: 701, sha: "m1", status: "in_progress"})}}
	if a := only(t, plan(t, f)); a.Kind != KindReport || a.Note != deliberate {
		t.Fatalf("superseded: %+v", a)
	}
}

func TestDeclaredLoadFailureReports(t *testing.T) {
	f := renovateWorld(t)
	f.routes["/repos/o/r/contents/"+declaredPath] = &ghapi.HTTPError{Status: 500, Path: "contents", Body: "boom"}
	a := only(t, plan(t, f))
	if a.Kind != KindReport || !strings.HasPrefix(a.Note, "cannot read declared retryable patterns: ") {
		t.Fatalf("%+v", a)
	}
}

func TestUnreadablePRsAreReported(t *testing.T) {
	f := newFake(t)
	f.routes["/repos/o/r/pulls?state=open"] = &ghapi.HTTPError{Status: 500, Path: "/repos/o/r/pulls?state=open"}
	a := only(t, plan(t, f))
	if a.Kind != KindReport || !strings.Contains(a.Note, "cannot read") {
		t.Fatalf("%+v", a)
	}
}

func TestNeverCancelsOrDeletes(t *testing.T) {
	enableReruns(t)
	f := mixedWorld(t)
	for _, a := range plan(t, f) {
		Apply(context.Background(), f, a, false)
	}
	// remFake.record fails the test on any DELETE or cancel path.
}

// mixedWorld has one of everything: a tickable Renovate PR, a rerunnable main
// run, a feature-branch PR, a fork PR, an exhausted PR and a deliberate cancel.
func mixedWorld(t *testing.T) *remFake {
	f := renovateWorld(t)
	addPR(f, prSpec{n: 10, ref: "feature/x", title: "x", body: renovateBody(t), sha: "f1"})
	addPR(f, prSpec{n: 11, ref: "renovate/evil", title: renovateTitle, body: renovateBody(t), sha: "f2", fork: true})
	addPR(f, prSpec{n: 12, ref: "renovate/go", title: "Update go", body: renovateBody(t), sha: "f3"})
	f.routes["/repos/o/r/issues/12/comments?per_page=100"] = []any{
		comment(me, Comment(1, Cap, "runner-lost", "u", "Update go"), -48*time.Hour),
		comment(me, Comment(2, Cap, "runner-lost", "u", "Update go"), -24*time.Hour)}
	for i, sha := range []string{"f1", "f2", "f3"} {
		rj := addRun(f, runSpec{id: int64(800 + i), event: "pull_request", sha: sha,
			jobs: []jobSpec{{id: int64(80 + i), name: "b", log: readFixture(t, "logs/runner-lost.txt")}}})
		addHeadRuns(f, sha, rj)
	}
	addMainRun(f, runSpec{id: 700, event: "push", branch: "main", sha: "m1",
		jobs: []jobSpec{{id: 6001, name: "build-native", log: readFixture(t, "logs/network-fetch.txt")}}})
	addMainRun(f, runSpec{id: 710, wid: 8, event: "push", branch: "main", sha: "m2", conclusion: "cancelled",
		jobs: []jobSpec{{id: 6101, name: "x", conclusion: "cancelled", noRunner: true, created: -2*time.Hour - time.Minute}}})
	return f
}

func TestOnlyAllowedWritePaths(t *testing.T) {
	enableReruns(t)
	f := mixedWorld(t)
	as := plan(t, f)
	kinds := map[ActionKind]int{}
	for _, a := range as {
		kinds[a.Kind]++
		Apply(context.Background(), f, a, false)
	}
	if kinds[KindTickBox] != 1 || kinds[KindRerun] != 1 || kinds[KindReport] != 4 {
		t.Fatalf("kinds %v: %+v", kinds, as)
	}
	allowed := []struct {
		method string
		re     *regexp.Regexp
	}{
		{"POST", regexp.MustCompile(`^/repos/o/r/issues/\d+/comments$`)},
		{"PATCH", regexp.MustCompile(`^/repos/o/r/pulls/\d+$`)},
		{"POST", regexp.MustCompile(`^/repos/o/r/actions/runs/\d+/rerun-failed-jobs$`)},
	}
	ws := f.allWrites()
	if len(ws) != 3 {
		t.Fatalf("writes %+v", ws)
	}
	for _, w := range ws {
		ok := false
		for _, a := range allowed {
			ok = ok || (w.Method == a.method && a.re.MatchString(w.Path))
		}
		if !ok {
			t.Errorf("write outside the allowed set: %s %s", w.Method, w.Path)
		}
		if w.Method == "PATCH" {
			if m, _ := w.Body.(map[string]string); len(m) != 1 || m["body"] == "" {
				t.Errorf("PATCH must change only the body: %+v", w.Body)
			}
		}
	}
}

func TestPlanNeedsIdentity(t *testing.T) {
	f := newFake(t)
	f.routes["/user"] = errors.New("bad credentials")
	if _, err := Plan(context.Background(), f, "o", t.TempDir(), now); err == nil {
		t.Fatal("want error")
	}
}

// --- attention.Retry ---

func TestAttentionRetryable(t *testing.T) {
	var _ attention.Retry = (*Attention)(nil)
	f := renovateWorld(t)
	addPR(f, prSpec{n: 10, ref: "feature/x", title: "x", body: renovateBody(t), sha: "f1"})
	rj := addRun(f, runSpec{id: 801, event: "pull_request", sha: "f1",
		jobs: []jobSpec{{id: 81, name: "b", log: readFixture(t, "logs/runner-lost.txt")}}})
	addHeadRuns(f, "f1", rj)
	r := NewAttention(f, "o", t.TempDir(), now)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if pat, ok, err := r.Retryable(context.Background(), attention.PR{Repo: "r", Number: 43}); !ok || err != nil || pat != "runner-lost" {
				t.Errorf("43: %q %v %v", pat, ok, err)
			}
			if _, ok, err := r.Retryable(context.Background(), attention.PR{Repo: "r", Number: 10}); ok || err != nil {
				t.Errorf("10: %v %v", ok, err)
			}
		}()
	}
	wg.Wait()
}

func TestAttentionRetryableErrorsWhenUnreadable(t *testing.T) {
	f := renovateWorld(t)
	f.routes["/repos/o/r/contents/"+declaredPath] = &ghapi.HTTPError{Status: 500, Path: "contents"}
	r := NewAttention(f, "o", t.TempDir(), now)
	if _, _, err := r.Retryable(context.Background(), attention.PR{Repo: "r", Number: 43}); err == nil {
		t.Fatal("an unreadable verdict must be an error, not 'not retryable'")
	}
}

func TestAttentionInProgress(t *testing.T) {
	upd := UpdateKey(renovateTitle)
	marker := func(at time.Duration) []any {
		return []any{comment(me, Comment(1, Cap, "runner-lost", "u", upd), at)}
	}
	// Marker 10m ago, newer than the head's last completed run (2h ago).
	f := renovateWorld(t)
	f.routes["/repos/o/r/issues/43/comments?per_page=100"] = marker(-10 * time.Minute)
	fs, err := NewAttention(f, "o", t.TempDir(), now).InProgress(context.Background(), "r")
	if err != nil || len(fs) != 1 || fs[0].Kind != model.KindRetryInProgress || fs[0].Item != "r#43" ||
		!strings.Contains(fs[0].Why, "1/2") || !strings.Contains(fs[0].Why, "runner-lost") {
		t.Fatalf("%+v %v", fs, err)
	}
	// Marker 3h ago, older than the last completed run: finished, nothing in progress.
	f = renovateWorld(t)
	f.routes["/repos/o/r/issues/43/comments?per_page=100"] = marker(-3 * time.Hour)
	if fs, _ := NewAttention(f, "o", t.TempDir(), now).InProgress(context.Background(), "r"); len(fs) != 0 {
		t.Fatalf("%+v", fs)
	}
	// A spoofed marker is not ours.
	f = renovateWorld(t)
	f.routes["/repos/o/r/issues/43/comments?per_page=100"] = []any{comment("mallory", Comment(1, Cap, "x", "u", upd), -time.Minute)}
	if fs, _ := NewAttention(f, "o", t.TempDir(), now).InProgress(context.Background(), "r"); len(fs) != 0 {
		t.Fatalf("%+v", fs)
	}
	// Marker 90m ago and no run started since: stuck.
	f = renovateWorld(t)
	f.routes["/repos/o/r/issues/43/comments?per_page=100"] = marker(-90 * time.Minute)
	fs, _ = NewAttention(f, "o", t.TempDir(), now).InProgress(context.Background(), "r")
	if len(fs) != 1 || fs[0].Kind != model.KindStuckPR || !strings.Contains(fs[0].Why, "no run started") {
		t.Fatalf("%+v", fs)
	}
	// A main run on attempt 2, still running.
	f = newFake(t)
	addMainRun(f, runSpec{id: 700, event: "push", branch: "main", sha: "m1", status: "in_progress", attempt: 2})
	fs, _ = NewAttention(f, "o", t.TempDir(), now).InProgress(context.Background(), "r")
	if len(fs) != 1 || fs[0].Kind != model.KindRetryInProgress || !strings.Contains(fs[0].Why, "1/2") {
		t.Fatalf("%+v", fs)
	}
}

func TestTickUsesBodyReadAfterComment(t *testing.T) {
	// Renovate rewrites the body while the comment is being posted: the tick
	// goes on the new body, never back onto the old one.
	f := renovateWorld(t)
	a := only(t, plan(t, f))
	rewritten := renovateBody(t) + "\n(edited by Renovate)\n"
	f.afterWrite = func(w write) {
		if w.Method == "POST" {
			p := map[string]any{}
			for k, v := range f.routes["/repos/o/r/pulls/43"].(map[string]any) {
				p[k] = v
			}
			p["body"] = rewritten
			f.set("/repos/o/r/pulls/43", p)
		}
	}
	r := Apply(context.Background(), f, a, false)
	ws := f.allWrites()
	want, _ := TickRebaseBox(rewritten)
	if !r.Done || len(ws) != 2 || ws[1].Body.(map[string]string)["body"] != want {
		t.Fatalf("result %+v writes %+v", r, ws)
	}

	// If the box got ticked (or the head moved) meanwhile, do not PATCH.
	f = renovateWorld(t)
	a = only(t, plan(t, f))
	ticked, _ := TickRebaseBox(renovateBody(t))
	f.afterWrite = func(w write) {
		if w.Method == "POST" {
			p := map[string]any{}
			for k, v := range f.routes["/repos/o/r/pulls/43"].(map[string]any) {
				p[k] = v
			}
			p["body"] = ticked
			f.set("/repos/o/r/pulls/43", p)
		}
	}
	r = Apply(context.Background(), f, a, false)
	if r.Done || r.Err != nil || r.Note != "already requested (comment posted, attempt 1 used)" {
		t.Fatalf("result %+v", r)
	}
	for _, w := range f.allWrites() {
		if w.Method == "PATCH" {
			t.Fatalf("patched after the box was ticked by someone else")
		}
	}
}

func TestRedCheckNotFromWorkflowRunReports(t *testing.T) {
	// An external status is red.
	f := renovateWorld(t)
	f.routes["/repos/o/r/commits/abc/status"] = map[string]any{"statuses": []any{
		map[string]any{"context": "ingredients-settled", "state": "pending"},
		map[string]any{"context": "ci/external", "state": "failure", "updated_at": ts(-time.Hour)}}}
	if a := only(t, plan(t, f)); a.Kind != KindReport || a.Note != "red check not from a classified workflow run: ci/external" {
		t.Fatalf("status: %+v", a)
	}
	// A check run from another app is red.
	f = renovateWorld(t)
	crs := f.routes["/repos/o/r/commits/abc/check-runs?per_page=100"].(map[string]any)["check_runs"].([]any)
	crs = append(crs, map[string]any{"name": "codecov", "check_suite": map[string]any{"id": 999},
		"status": "completed", "conclusion": "failure", "completed_at": ts(-time.Hour)})
	f.routes["/repos/o/r/commits/abc/check-runs?per_page=100"] = map[string]any{"check_runs": crs}
	if a := only(t, plan(t, f)); a.Kind != KindReport || a.Note != "red check not from a classified workflow run: codecov" {
		t.Fatalf("check run: %+v", a)
	}
	// The ingredients-settled status alone never blocks.
	f = renovateWorld(t)
	f.routes["/repos/o/r/commits/abc/status"] = map[string]any{"statuses": []any{
		map[string]any{"context": "ingredients-settled", "state": "failure"}}}
	if a := only(t, plan(t, f)); a.Kind != KindTickBox {
		t.Fatalf("settled: %+v", a)
	}
}

func TestRunsOnHeadKeyedByWorkflowAndEvent(t *testing.T) {
	// The same workflow ran for push and pull_request on the head; both are
	// newest for their event, and the failed push run must be classified too.
	f := renovateWorld(t)
	pr := addRun(f, runSpec{id: 900, event: "pull_request", branch: "renovate/rust", sha: "abc", conclusion: "success",
		jobs: []jobSpec{}})
	push := addRun(f, runSpec{id: 901, event: "push", branch: "renovate/rust", sha: "abc",
		jobs: []jobSpec{{id: 5002, name: "on-linux", log: readFixture(t, "logs/build-error.txt")}}})
	addHeadRuns(f, "abc", pr, push)
	if a := only(t, plan(t, f)); a.Kind != KindReport || !strings.Contains(a.Note, `"on-linux"`) {
		t.Fatalf("%+v", a)
	}
}

func TestApplyRechecksEveryRunOnHead(t *testing.T) {
	twoRuns := func() *remFake {
		f := renovateWorld(t)
		r1 := runJSON(runSpec{id: 900, event: "pull_request", branch: "renovate/rust", sha: "abc"})
		r2 := addRun(f, runSpec{id: 902, wid: 8, event: "pull_request", branch: "renovate/rust", sha: "abc",
			jobs: []jobSpec{{id: 5003, name: "lint", log: readFixture(t, "logs/runner-lost.txt")}}})
		addHeadRuns(f, "abc", r2, r1)
		return f
	}
	f := twoRuns()
	a := only(t, plan(t, f))
	if a.Kind != KindTickBox || len(a.RunIDs) != 2 {
		t.Fatalf("%+v", a)
	}
	// The second failed run went green (someone re-ran it).
	green := runJSON(runSpec{id: 902, wid: 8, event: "pull_request", sha: "abc", conclusion: "success"})
	f.set("/repos/o/r/actions/runs?head_sha=abc&per_page=100", map[string]any{"workflow_runs": []any{green,
		runJSON(runSpec{id: 900, event: "pull_request", sha: "abc"})}})
	if r := Apply(context.Background(), f, a, false); r.Done || r.Note != "run no longer failed" || len(f.allWrites()) != 0 {
		t.Fatalf("green: %+v", r)
	}
	// Another workflow started on the head.
	f = twoRuns()
	a = only(t, plan(t, f))
	l := f.routes["/repos/o/r/actions/runs?head_sha=abc&per_page=100"].(map[string]any)["workflow_runs"].([]any)
	l = append([]any{runJSON(runSpec{id: 950, wid: 9, event: "pull_request", sha: "abc", status: "in_progress"})}, l...)
	f.set("/repos/o/r/actions/runs?head_sha=abc&per_page=100", map[string]any{"workflow_runs": l})
	if r := Apply(context.Background(), f, a, false); r.Done || r.Note != "run in progress" || len(f.allWrites()) != 0 {
		t.Fatalf("in progress: %+v", r)
	}
}

func TestCannotReadReportsCarryTheError(t *testing.T) {
	f := renovateWorld(t)
	f.routes["/repos/o/r/contents/"+declaredPath] = &ghapi.HTTPError{Status: 500, Path: "contents", Body: "boom"}
	a := only(t, plan(t, f))
	if a.Kind != KindReport || a.Error == "" || !strings.Contains(a.Error, "500") {
		t.Fatalf("%+v", a)
	}
	b, _ := json.Marshal(a)
	if !strings.Contains(string(b), `"error":`) {
		t.Fatalf("%s", b)
	}
	r := Apply(context.Background(), f, a, false)
	if r.Err != nil || r.Action.Error != a.Error {
		t.Fatalf("a read error is reported, not a failure: %+v", r)
	}
	// Other reports carry no error field.
	b, _ = json.Marshal(only(t, plan(t, mainWorld(t, 1))))
	if strings.Contains(string(b), `"error"`) {
		t.Fatalf("%s", b)
	}
}
