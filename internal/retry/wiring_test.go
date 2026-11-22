package retry

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/Mavergreen/fleet/internal/attention"
	"github.com/Mavergreen/fleet/internal/ghapi"
)

// counted counts every request that reaches the "network" (the fake).
type counted struct {
	*remFake
	mu sync.Mutex
	n  map[string]int
}

func count(f *remFake) *counted { return &counted{remFake: f, n: map[string]int{}} }

func (c *counted) hit(k string) {
	c.mu.Lock()
	c.n[k]++
	c.mu.Unlock()
}
func (c *counted) Get(ctx context.Context, p string, out any) error {
	c.hit("GET " + p)
	return c.remFake.Get(ctx, p, out)
}
func (c *counted) GetPaged(ctx context.Context, p string, each func(json.RawMessage) error) error {
	c.hit("GET " + p)
	return c.remFake.GetPaged(ctx, p, each)
}
func (c *counted) GetRaw(ctx context.Context, p string) ([]byte, error) {
	c.hit("RAW " + p)
	return c.remFake.GetRaw(ctx, p)
}

func (c *counted) reads() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for k, n := range c.n {
		out = append(out, k+" x"+strings.Repeat("I", n))
	}
	sort.Strings(out)
	return out
}

// attentionOrg: a Renovate PR red for 2h with a retryable failure, a red main
// run, and an open issue.
func attentionOrg(t *testing.T) *remFake {
	f := renovateWorld(t)
	addMainRun(f, runSpec{id: 700, wid: 8, event: "push", branch: "main", sha: "m1",
		jobs: []jobSpec{{id: 6001, name: "build-native", log: readFixture(t, "logs/network-fetch.txt")}}})
	f.routes["/repos/o/r/issues?state=open"] = []any{map[string]any{"number": 3, "title": "help",
		"user": map[string]any{"login": "someone", "type": "User"}, "created_at": ts(-1e9)}}
	f.routes["/repos/o/r/issues/3/comments?per_page=100"] = []any{}
	return f
}

// attention reads each endpoint once, sharing reads with remediate's
// planning (a red run's jobs included), and fetches no logs: nothing it
// reports needs one here.
func TestAttentionReadsOnceAndFetchesNoLogs(t *testing.T) {
	net := count(attentionOrg(t))
	m := ghapi.NewMemo(net)
	fs, _, err := attention.Collect(context.Background(), m, "o", "", NewAttention(m, "o", t.TempDir(), now), now)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range net.reads() {
		if strings.HasPrefix(r, "RAW ") || strings.Contains(r, "/logs") || !strings.HasSuffix(r, " xI") {
			t.Errorf("%s", r)
		}
	}
	red := false
	for _, f := range fs {
		red = red || (f.Kind == "red-workflow" && f.Repo == "r")
	}
	if !red {
		t.Fatalf("red main run not reported: %+v", fs)
	}
}

// Plan may share reads, but Apply's re-reads must reach the network: its
// correctness against a concurrent remediate rests on them. The token's
// user is asked once, by Plan.
func TestRemediateAppliesWithFreshReads(t *testing.T) {
	net := count(renovateWorld(t))
	results, repos, err := Remediate(context.Background(), net, "o", t.TempDir(), now, false)
	if err != nil || repos != 1 || len(results) != 1 || !results[0].Done {
		t.Fatalf("%v %d %+v", err, repos, results)
	}
	for path, want := range map[string]int{
		"/user": 1,
		"/repos/o/r/actions/runs?head_sha=abc&per_page=100": 2, // Plan, then Apply's recheck
		"/repos/o/r/issues/43/comments?per_page=100":        2, // Plan, then Apply's marker count
		"/repos/o/r/pulls/43":                               2, // Apply, before and after the comment
	} {
		if got := net.n["GET "+path]; got != want {
			t.Errorf("%s read %d times, want %d", path, got, want)
		}
	}
}
