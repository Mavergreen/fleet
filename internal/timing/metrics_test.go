package timing

import (
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func at(m float64) time.Time { return t0.Add(time.Duration(m * float64(time.Minute))) }

func mac(name string, created, started, completed time.Time) Job {
	return Job{Repo: "r", Workflow: "release.yml", Name: name, RunID: 1, Attempt: 1, Labels: []string{"macos-26"},
		Created: created, Started: started, Completed: completed, RunnerName: "runner", AnyStepStarted: true, Conclusion: "success"}
}

func TestDefinitionsMatchRunnerCostStudy(t *testing.T) {
	b, err := os.ReadFile("testdata/study-sample.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Study struct {
			Repo, Kind string
			Jobs       int     `json:"jobs"`
			WaitH      float64 `json:"wait_h"`
			MedianM    float64 `json:"median_wait_m"`
			P90M       float64 `json:"p90_wait_m"`
			RunH       float64 `json:"run_h"`
			Never      int     `json:"never_started"`
			NeverH     float64 `json:"never_started_wait_h"`
			Reruns     int     `json:"reruns"`
			Kicks      int     `json:"manual_kicks"`
			WastedH    float64 `json:"wasted_h"`
			TotalH     float64 `json:"total_h"`
		} `json:"study"`
		Runs []Run `json:"runs"`
		Jobs []Job `json:"jobs"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	var got *RepoStats
	stats := Summarize(f.Runs, f.Jobs)
	for i := range stats {
		if stats[i].Repo == f.Study.Repo && stats[i].Kind == f.Study.Kind {
			got = &stats[i]
		}
	}
	if got == nil {
		t.Fatalf("no %s/%s row in %+v", f.Study.Repo, f.Study.Kind, stats)
	}
	s := f.Study
	if got.Jobs != s.Jobs || got.NeverStarted != s.Never || got.Reruns != s.Reruns || got.ManualKicks != s.Kicks {
		t.Errorf("exact: got jobs=%d never=%d reruns=%d kicks=%d, study %d %d %d %d",
			got.Jobs, got.NeverStarted, got.Reruns, got.ManualKicks, s.Jobs, s.Never, s.Reruns, s.Kicks)
	}
	near := func(name string, g, w, tol float64) {
		if math.Abs(g-w) > tol {
			t.Errorf("%s: got %.3f, study %.1f", name, g, w)
		}
	}
	near("WaitH", got.WaitH, s.WaitH, 0.1)
	near("RunH", got.RunH, s.RunH, 0.1)
	near("NeverStartedWaitH", got.NeverStartedWaitH, s.NeverH, 0.1)
	near("WastedH", got.WastedH, s.WastedH, 0.1)
	near("TotalH", got.TotalH, s.TotalH, 0.1)
	near("MedianWaitM", got.MedianWaitM, s.MedianM, 0.1)
	near("P90WaitM", got.P90WaitM, s.P90M, 0.1)
}

func TestSkippedJobsExcluded(t *testing.T) {
	skipped := Job{Repo: "r", Name: "s", RunID: 1, Attempt: 1, Conclusion: "skipped", Created: t0, Started: t0, Completed: t0}
	if !Skipped(skipped) {
		t.Fatal("skipped job with no labels not detected")
	}
	ran := mac("b", at(0), at(10), at(20))
	if Skipped(ran) {
		t.Fatal("ran job counted skipped")
	}
	stats := Summarize([]Run{{Repo: "r", ID: 1, Attempt: 1}}, []Job{skipped, ran})
	if len(stats) != 1 || stats[0].Jobs != 1 || stats[0].Kind != "macos" {
		t.Fatalf("%+v", stats)
	}
}

func TestPerJobDefinitions(t *testing.T) {
	j := mac("b", at(0), at(10), at(25))
	if QueueWait(j) != 10*time.Minute || RunTime(j) != 15*time.Minute || NeverStarted(j) || !IsMacOS(j) {
		t.Fatal("started job")
	}
	n := Job{Labels: []string{"macos-26-intel"}, Created: at(0), Started: at(0), Completed: at(30), Conclusion: "cancelled"}
	if !NeverStarted(n) || QueueWait(n) != 30*time.Minute || RunTime(n) != 0 || !IsMacOS(n) {
		t.Fatalf("never-started job: %v %v", QueueWait(n), RunTime(n))
	}
	if IsMacOS(Job{Labels: []string{"ubuntu-latest"}}) {
		t.Fatal("linux is not macOS")
	}
}

func TestMedianAndP90MatchStudyMethod(t *testing.T) {
	var jobs []Job
	for i := 1; i <= 10; i++ {
		jobs = append(jobs, mac("j", at(0), at(float64(i)), at(float64(i)+1)))
	}
	st := Summarize([]Run{{Repo: "r", ID: 1, Attempt: 1}}, jobs)[0]
	if st.MedianWaitM != 5.5 || st.P90WaitM != 9 {
		t.Fatalf("median %v p90 %v", st.MedianWaitM, st.P90WaitM)
	}
}

func TestRerunsAndWasted(t *testing.T) {
	run := Run{Repo: "r", ID: 1, Attempt: 3, Conclusion: "success"}
	a1 := mac("b", at(0), at(0), at(60))
	a1.Conclusion, a1.RunnerName, a1.AnyStepStarted = "cancelled", "", false // abandoned, never started: 1h wasted
	a2 := mac("b", at(0), at(0), at(30))
	a2.Attempt, a2.Conclusion = 2, "failure" // abandoned but ran and failed: not wasted
	a3 := mac("b", at(0), at(5), at(10))
	a3.Attempt = 3
	st := Summarize([]Run{run}, []Job{a1, a2, a3})[0]
	if st.Reruns != 2 {
		t.Errorf("reruns %d", st.Reruns)
	}
	if math.Abs(st.WastedH-1) > 0.01 {
		t.Errorf("wasted %v", st.WastedH)
	}
}

func TestWastedNeedsALaterAttemptOrAKick(t *testing.T) {
	cancel := func() Job {
		j := mac("b", at(0), at(0), at(60))
		j.Conclusion, j.RunnerName, j.AnyStepStarted = "cancelled", "", false
		return j
	}
	alone := Run{Repo: "r", Workflow: "release.yml", HeadBranch: "main", ID: 1, Attempt: 1, Conclusion: "cancelled", Created: at(0)}
	if st := Summarize([]Run{alone}, []Job{cancel()})[0]; st.WastedH != 0 {
		t.Errorf("a cancelled run nobody re-ran is not wasted: %v", st.WastedH)
	}
	kick := Run{Repo: "r", Workflow: "release.yml", HeadBranch: "main", ID: 2, Attempt: 1, Event: "workflow_dispatch", Actor: "schmonz", Created: at(120)}
	st := Summarize([]Run{alone, kick}, []Job{cancel()})[0]
	if math.Abs(st.WastedH-1) > 0.01 || st.ManualKicks != 1 {
		t.Errorf("kicked run: wasted %v kicks %d", st.WastedH, st.ManualKicks)
	}
}

func TestSummarizeSplitsKindsAndRanks(t *testing.T) {
	lin := func(repo string, run time.Duration) Job {
		return Job{Repo: repo, RunID: 2, Attempt: 1, Labels: []string{"ubuntu-latest"}, Created: t0, Started: t0, Completed: t0.Add(run), RunnerName: "x", AnyStepStarted: true}
	}
	m := mac("b", at(0), at(60), at(120))
	m.Repo = "big"
	jobs := []Job{lin("small", time.Minute), m, lin("big", 10*time.Minute)}
	st := Summarize(nil, jobs)
	if len(st) != 3 || st[0].Repo != "big" || st[0].Kind != "macos" || st[2].Repo != "small" {
		t.Fatalf("%+v", st)
	}
	for i := 1; i < len(st); i++ {
		if st[i].TotalH > st[i-1].TotalH {
			t.Fatal("not sorted by TotalH desc")
		}
	}
}

func TestManualKickWithin24h(t *testing.T) {
	bot := func(a string) bool { return strings.HasSuffix(a, "[bot]") }
	cancelled := Run{Repo: "r", Workflow: "release.yml", HeadBranch: "main", ID: 1, Conclusion: "cancelled", Created: t0}
	disp := func(h float64, actor string) Run {
		return Run{Repo: "r", Workflow: "release.yml", HeadBranch: "main", ID: 2, Event: "workflow_dispatch", Actor: actor, Created: t0.Add(time.Duration(h * float64(time.Hour)))}
	}
	for _, c := range []struct {
		name string
		r    Run
		want int
	}{
		{"human 3h", disp(3, "schmonz"), 1},
		{"bot", disp(3, "github-actions[bot]"), 0},
		{"25h", disp(25, "schmonz"), 0},
	} {
		if got := ManualKicks([]Run{cancelled, c.r}, bot); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
	other := disp(3, "schmonz")
	other.HeadBranch = "dev"
	if ManualKicks([]Run{cancelled, other}, bot) != 0 {
		t.Error("different branch counted")
	}
}

func TestIsBot(t *testing.T) {
	for a, want := range map[string]bool{"renovate[bot]": true, "github-actions[bot]": true, "renovate": true, "dependabot": true, "schmonz": false, "": false} {
		if IsBot(a) != want {
			t.Errorf("IsBot(%q) != %v", a, want)
		}
	}
}

func TestTableFitsWidthAndHasNoANSIOffTTY(t *testing.T) {
	st := Summarize(nil, []Job{mac("b", at(0), at(60), at(120))})
	for _, w := range []int{0, 60, 100, 200} {
		out := Table(st, w, false)
		for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
			if w > 0 && len([]rune(l)) > w {
				t.Errorf("width %d: line %d runes: %q", w, len([]rune(l)), l)
			}
			if strings.Contains(l, "\x1b") {
				t.Errorf("ANSI off tty: %q", l)
			}
		}
	}
	full := Table(st, 0, false)
	for _, h := range []string{"Repo", "Kind", "Jobs", "Wait h", "Med wait m", "P90 wait m", "Run h", "Never-started", "Reruns", "Kicks", "Wasted h", "Total h"} {
		if !strings.Contains(full, h) {
			t.Errorf("missing header %q in %q", h, full)
		}
	}
	narrow := Table(st, 60, true)
	if !strings.Contains(narrow, "\x1b") {
		t.Error("expected styling on a tty")
	}
}
