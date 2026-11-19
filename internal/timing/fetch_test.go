package timing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mavergreen/fleet/internal/ghapi"
)

type fake struct {
	mu    sync.Mutex
	pages map[string][]string // path -> items (JSON)
	errs  map[string]error
	calls []string
}

func (f *fake) Get(context.Context, string, any) error { return nil }
func (f *fake) GetPaged(_ context.Context, p string, each func(json.RawMessage) error) error {
	f.mu.Lock()
	f.calls = append(f.calls, p)
	err := f.errs[p]
	items, ok := f.pages[p]
	f.mu.Unlock()
	if err != nil {
		return err
	}
	if !ok {
		return &ghapi.HTTPError{Status: 404, Path: p, Body: "nf"}
	}
	for _, it := range items {
		if err := each(json.RawMessage(it)); err != nil {
			return err
		}
	}
	return nil
}
func (f *fake) GetRaw(context.Context, string) ([]byte, error) { return nil, nil }
func (f *fake) Patch(context.Context, string, any, any) error  { return nil }
func (f *fake) Post(context.Context, string, any, any) error   { return nil }

func (f *fake) jobCalls() int {
	n := 0
	for _, c := range f.calls {
		if strings.Contains(c, "/jobs") {
			n++
		}
	}
	return n
}

var since = time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)

const runsPath = "/repos/Mavergreen/a/actions/runs?created=>=2026-11-12&per_page=100"

func runJSON(id int64, status string, attempt int, created string) string {
	return fmt.Sprintf(`{"id":%d,"path":".github/workflows/release.yml","name":"Release","head_branch":"main","event":"push","status":%q,"conclusion":"success","run_attempt":%d,"created_at":%q,"actor":{"login":"me"}}`, id, status, attempt, created)
}

const jobA1 = `{"name":"build","run_attempt":1,"labels":["macos"],"created_at":"2026-11-18T10:00:00Z","started_at":"2026-11-18T10:01:00Z","completed_at":"2026-11-18T10:05:00Z","runner_name":"r1","conclusion":"success","steps":[{"started_at":null},{"started_at":"2026-11-18T10:01:05Z"}]}`
const jobA2 = `{"name":"build","run_attempt":2,"labels":["macos"],"created_at":"2026-11-18T11:00:00Z","started_at":"2026-11-18T11:01:00Z","completed_at":"2026-11-18T11:05:00Z","runner_name":"r2","conclusion":"success","steps":[{"started_at":null}]}`

func jobsPath(id int64) string {
	return fmt.Sprintf("/repos/Mavergreen/a/actions/runs/%d/jobs?filter=all", id)
}

func TestFetchAllAttempts(t *testing.T) {
	f := &fake{pages: map[string][]string{
		runsPath:    {runJSON(7, "completed", 2, "2026-11-18T09:00:00Z")},
		jobsPath(7): {jobA1, jobA2},
	}}
	runs, jobs, err := Fetch(context.Background(), f, "Mavergreen", []string{"a"}, since, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Workflow != "release.yml" || runs[0].Attempt != 2 || runs[0].Actor != "me" {
		t.Fatalf("runs = %+v", runs)
	}
	if len(jobs) != 2 || jobs[0].Attempt != 1 || jobs[1].Attempt != 2 {
		t.Fatalf("jobs = %+v", jobs)
	}
	if !jobs[0].AnyStepStarted || jobs[1].AnyStepStarted || jobs[0].Workflow != "release.yml" || jobs[0].Repo != "a" {
		t.Fatalf("job fields: %+v", jobs)
	}
}

func TestCacheServesCompletedRuns(t *testing.T) {
	f := &fake{pages: map[string][]string{
		runsPath:    {runJSON(7, "completed", 1, "2026-11-18T09:00:00Z")},
		jobsPath(7): {jobA1},
	}}
	cache := &Cache{Dir: t.TempDir()}
	for i := 0; i < 2; i++ {
		_, jobs, err := Fetch(context.Background(), f, "Mavergreen", []string{"a"}, since, cache)
		if err != nil || len(jobs) != 1 {
			t.Fatalf("pass %d: %v %v", i, jobs, err)
		}
	}
	if n := f.jobCalls(); n != 1 {
		t.Fatalf("jobs requests = %d, want 1", n)
	}
}

func TestRerunInvalidatesCache(t *testing.T) {
	f := &fake{pages: map[string][]string{
		runsPath:    {runJSON(7, "completed", 1, "2026-11-18T09:00:00Z")},
		jobsPath(7): {jobA1},
	}}
	cache := &Cache{Dir: t.TempDir()}
	Fetch(context.Background(), f, "Mavergreen", []string{"a"}, since, cache)
	f.pages[runsPath] = []string{runJSON(7, "completed", 2, "2026-11-18T09:00:00Z")}
	f.pages[jobsPath(7)] = []string{jobA1, jobA2}
	_, jobs, _ := Fetch(context.Background(), f, "Mavergreen", []string{"a"}, since, cache)
	if f.jobCalls() != 2 || len(jobs) != 2 {
		t.Fatalf("jobCalls=%d jobs=%d", f.jobCalls(), len(jobs))
	}
}

func TestInProgressRunsNotCached(t *testing.T) {
	f := &fake{pages: map[string][]string{
		runsPath:    {runJSON(7, "in_progress", 1, "2026-11-18T09:00:00Z")},
		jobsPath(7): {jobA1},
	}}
	cache := &Cache{Dir: t.TempDir()}
	for i := 0; i < 2; i++ {
		Fetch(context.Background(), f, "Mavergreen", []string{"a"}, since, cache)
	}
	if f.jobCalls() != 2 {
		t.Fatalf("jobs requests = %d, want 2", f.jobCalls())
	}
	if _, err := os.Stat(filepath.Join(cache.Dir, "a", "7.json")); err == nil {
		t.Fatal("in-progress run was cached")
	}
}

func TestCacheDirUnwritableStillFetches(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	os.WriteFile(blocker, []byte("x"), 0o644)
	var warn bytes.Buffer
	cache := &Cache{Dir: filepath.Join(blocker, "sub"), Warn: &warn}
	f := &fake{pages: map[string][]string{
		runsPath:    {runJSON(7, "completed", 1, "2026-11-18T09:00:00Z"), runJSON(8, "completed", 1, "2026-11-18T09:00:00Z")},
		jobsPath(7): {jobA1}, jobsPath(8): {jobA1},
	}}
	_, jobs, err := Fetch(context.Background(), f, "Mavergreen", []string{"a"}, since, cache)
	if err != nil || len(jobs) != 2 {
		t.Fatalf("jobs=%d err=%v", len(jobs), err)
	}
	if strings.Count(warn.String(), "\n") != 1 || !strings.Contains(warn.String(), "warning") {
		t.Fatalf("warn = %q", warn.String())
	}
}

func TestCorruptCacheFileIsMiss(t *testing.T) {
	cache := &Cache{Dir: t.TempDir()}
	os.MkdirAll(filepath.Join(cache.Dir, "a"), 0o755)
	os.WriteFile(filepath.Join(cache.Dir, "a", "7.json"), []byte("{torn"), 0o644)
	f := &fake{pages: map[string][]string{
		runsPath:    {runJSON(7, "completed", 1, "2026-11-18T09:00:00Z")},
		jobsPath(7): {jobA1},
	}}
	_, jobs, err := Fetch(context.Background(), f, "Mavergreen", []string{"a"}, since, cache)
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	if _, ok := cache.Get("a", 7, 1); !ok {
		t.Fatal("corrupt file not overwritten")
	}
}

func TestCachePrunesOldRuns(t *testing.T) {
	now := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	cache := &Cache{Dir: t.TempDir()}
	old := Run{Repo: "a", ID: 1, Attempt: 1, Created: now.AddDate(0, 0, -121)}
	recent := Run{Repo: "a", ID: 2, Attempt: 1, Created: now.AddDate(0, 0, -119)}
	cache.Put(old, nil)
	cache.Put(recent, nil)
	// Put alone does not prune.
	if _, ok := cache.Get("a", 1, 1); !ok {
		t.Fatal("Put pruned")
	}
	stale := filepath.Join(cache.Dir, "a", ".tmp-stale")
	fresh := filepath.Join(cache.Dir, "a", ".tmp-fresh")
	os.WriteFile(stale, []byte("x"), 0o644)
	os.WriteFile(fresh, []byte("x"), 0o644)
	os.Chtimes(stale, now.Add(-2*time.Hour), now.Add(-2*time.Hour))
	os.Chtimes(fresh, now.Add(-time.Minute), now.Add(-time.Minute))
	if err := cache.Prune("a", now); err != nil {
		t.Fatal(err)
	}
	if _, ok := cache.Get("a", 1, 1); ok {
		t.Fatal("old entry survived")
	}
	if _, ok := cache.Get("a", 2, 1); !ok {
		t.Fatal("recent entry pruned")
	}
	if _, err := os.Stat(stale); err == nil {
		t.Fatal("stale tmp file survived")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("fresh tmp file removed")
	}
}

func TestCacheRejectsUnsafeRepoNames(t *testing.T) {
	cache := &Cache{Dir: t.TempDir()}
	for _, repo := range []string{"", ".", "..", "../x", "a/b"} {
		if _, ok := cache.Get(repo, 1, 1); ok {
			t.Errorf("Get(%q) hit", repo)
		}
		if err := cache.Put(Run{Repo: repo, ID: 1, Attempt: 1}, nil); err == nil {
			t.Errorf("Put(%q) succeeded", repo)
		}
		if err := cache.Prune(repo, time.Now()); err == nil {
			t.Errorf("Prune(%q) succeeded", repo)
		}
	}
}

func TestCacheRoundTripKeepsAllAttempts(t *testing.T) {
	cache := &Cache{Dir: t.TempDir()}
	jobs := []Job{
		{Repo: "a", Name: "build", RunID: 7, Attempt: 1, Labels: []string{"macos"}, Started: time.Date(2026, 11, 18, 10, 1, 0, 0, time.UTC), AnyStepStarted: true},
		{Repo: "a", Name: "build", RunID: 7, Attempt: 2, Labels: []string{"macos"}, Started: time.Date(2026, 11, 18, 11, 1, 0, 0, time.UTC)},
	}
	if err := cache.Put(Run{Repo: "a", ID: 7, Attempt: 2, Created: time.Now()}, jobs); err != nil {
		t.Fatal(err)
	}
	got, ok := cache.Get("a", 7, 2)
	if !ok || !reflect.DeepEqual(got, jobs) {
		t.Fatalf("got %+v ok=%v", got, ok)
	}
}

func TestJobsFetchFailureFlagsRun(t *testing.T) {
	f := &fake{
		pages: map[string][]string{
			runsPath:    {runJSON(7, "completed", 1, "2026-11-18T09:00:00Z"), runJSON(8, "completed", 1, "2026-11-18T09:00:00Z")},
			jobsPath(8): {jobA1},
		},
		errs: map[string]error{jobsPath(7): &ghapi.HTTPError{Status: 500, Path: jobsPath(7), Body: "boom"}},
	}
	runs, jobs, err := Fetch(context.Background(), f, "Mavergreen", []string{"a"}, since, nil)
	if err == nil || !strings.Contains(err.Error(), "run 7") {
		t.Fatalf("err = %v", err)
	}
	if len(runs) != 2 || !runs[0].JobsUnknown || runs[1].JobsUnknown || len(jobs) != 1 {
		t.Fatalf("runs=%+v jobs=%d", runs, len(jobs))
	}
}

func TestFetchPrunesOncePerRepo(t *testing.T) {
	cache := &Cache{Dir: t.TempDir()}
	os.MkdirAll(filepath.Join(cache.Dir, "a"), 0o755)
	p := filepath.Join(cache.Dir, "a", "99.json")
	os.WriteFile(p, []byte(`{}`), 0o644)
	old := time.Now().AddDate(0, 0, -200)
	os.Chtimes(p, old, old)
	f := &fake{pages: map[string][]string{runsPath: nil}}
	Fetch(context.Background(), f, "Mavergreen", []string{"a"}, since, cache)
	if _, err := os.Stat(p); err == nil {
		t.Fatal("Fetch did not prune")
	}
}

func TestFetchPartialOnRepoFailure(t *testing.T) {
	f := &fake{
		pages: map[string][]string{
			"/repos/Mavergreen/b/actions/runs?created=>=2026-11-12&per_page=100": {runJSON(3, "completed", 1, "2026-11-18T09:00:00Z")},
			"/repos/Mavergreen/b/actions/runs/3/jobs?filter=all":                 {jobA1},
		},
		errs: map[string]error{runsPath: &ghapi.HTTPError{Status: 500, Path: runsPath, Body: "boom"}},
	}
	runs, jobs, err := Fetch(context.Background(), f, "Mavergreen", []string{"a", "b"}, since, nil)
	if err == nil || !strings.Contains(err.Error(), "a:") {
		t.Fatalf("err = %v", err)
	}
	if len(runs) != 1 || len(jobs) != 1 || runs[0].Repo != "b" {
		t.Fatalf("runs=%v jobs=%v", runs, jobs)
	}
}

func TestFetchWarnsAtListingCap(t *testing.T) {
	items := make([]string, 1000)
	for i := range items {
		items[i] = runJSON(int64(i+1), "queued", 1, "2026-11-18T09:00:00Z")
	}
	f := &fake{pages: map[string][]string{runsPath: items}}
	for i := 1; i <= 1000; i++ {
		f.pages[jobsPath(int64(i))] = nil
	}
	var warn bytes.Buffer
	Fetch(context.Background(), f, "Mavergreen", []string{"a"}, since, &Cache{Dir: t.TempDir(), Warn: &warn})
	if !strings.Contains(warn.String(), "a: runs listing reached") {
		t.Fatalf("warn = %q", warn.String())
	}
}
