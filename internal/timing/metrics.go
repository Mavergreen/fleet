package timing

import (
	"math"
	"sort"
	"strings"
	"time"
)

// IsMacOS reports whether any label is macos-*.
func IsMacOS(j Job) bool {
	for _, l := range j.Labels {
		if strings.HasPrefix(l, "macos-") {
			return true
		}
	}
	return false
}

// Skipped reports whether the job was skipped, so never occupied a runner.
// The spec says "conclusion skipped, no labels", but GitHub sometimes keeps the
// labels of a skipped job and the runner-cost study's figures exclude those
// too (see TestDefinitionsMatchRunnerCostStudy), so a skip is a skip.
func Skipped(j Job) bool { return j.Conclusion == "skipped" }

// NeverStarted is: no runner_name and no step started.
func NeverStarted(j Job) bool { return j.RunnerName == "" && !j.AnyStepStarted }

func nonNeg(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d
}

// QueueWait is Started-Created; for a never-started job, Completed-Created
// (the wait runs until cancellation).
func QueueWait(j Job) time.Duration {
	if NeverStarted(j) {
		if j.Completed.IsZero() {
			return 0
		}
		return nonNeg(j.Completed.Sub(j.Created))
	}
	if j.Started.IsZero() {
		return 0
	}
	return nonNeg(j.Started.Sub(j.Created))
}

// RunTime is Completed-Started; a never-started job ran for nothing.
func RunTime(j Job) time.Duration {
	if NeverStarted(j) || j.Started.IsZero() || j.Completed.IsZero() {
		return 0
	}
	return nonNeg(j.Completed.Sub(j.Started))
}

// IsBot reports whether an actor is automation: a login ending in [bot], or
// renovate or dependabot.
func IsBot(actor string) bool {
	return strings.HasSuffix(actor, "[bot]") || actor == "renovate" || actor == "dependabot"
}

// RepoStats is one repo's figures for one runner kind.
type RepoStats struct {
	Repo              string  `json:"repo"`
	Kind              string  `json:"kind"` // "macos" or "linux"
	Jobs              int     `json:"jobs"`
	WaitH             float64 `json:"wait_h"`
	MedianWaitM       float64 `json:"median_wait_m"`
	P90WaitM          float64 `json:"p90_wait_m"`
	RunH              float64 `json:"run_h"`
	NeverStarted      int     `json:"never_started"`
	NeverStartedWaitH float64 `json:"never_started_wait_h"`
	Reruns            int     `json:"reruns"`
	ManualKicks       int     `json:"manual_kicks"`
	WastedH           float64 `json:"wasted_h"`
	TotalH            float64 `json:"total_h"`
}

type runKey struct {
	repo string
	id   int64
}

type groupKey struct{ repo, kind string }

func kindOf(j Job) string {
	if IsMacOS(j) {
		return "macos"
	}
	return "linux"
}

// median is the mean of the two middle values for an even count. The study's
// ed25519 row (34 jobs: median 1.4m, p90 101.0m) fixes the method: nearest-rank
// would give 0.15 and 138.5.
func median(sorted []float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// p90 is the value at index floor(0.9*(n-1)) of the sorted waits (the study's
// method, inferred from its ed25519 row).
func p90(sorted []float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[int(math.Floor(0.9*float64(len(sorted)-1)))]
}

// Summarize computes one row per (repo, kind), ranked by TotalH descending.
// Runs flagged JobsUnknown, and jobs not yet completed, are excluded from
// every figure (a re-run still counts toward Reruns once it exists).
//
// Reruns follows the study: for each run, its highest attempt minus one (so a
// run attempted three times is two re-runs), counted per repo whether or not
// the re-run's own jobs were skipped, and shown on the repo's macOS row (its
// Linux row when it has none), as the study's table is macOS-only.
func Summarize(runs []Run, jobs []Job) []RepoStats {
	unknown := map[runKey]bool{}
	byRun := map[runKey]Run{}
	for _, r := range runs {
		k := runKey{r.Repo, r.ID}
		byRun[k] = r
		if r.JobsUnknown {
			unknown[k] = true
		}
	}
	var kept []Job
	maxAttempt := map[runKey]int{}
	for _, j := range jobs {
		k := runKey{j.Repo, j.RunID}
		if unknown[k] {
			continue
		}
		if j.Attempt > maxAttempt[k] {
			maxAttempt[k] = j.Attempt
		}
		// A job with no Completed is queued or running: it has no figures
		// yet, and a queued one would otherwise look never-started.
		if !Skipped(j) && !j.Completed.IsZero() {
			kept = append(kept, j)
		}
	}
	for k, r := range byRun {
		if !unknown[k] && r.Attempt > maxAttempt[k] {
			maxAttempt[k] = r.Attempt
		}
	}

	type acc struct {
		st    RepoStats
		waits []float64
	}
	groups := map[groupKey]*acc{}
	get := func(repo, kind string) *acc {
		g := groupKey{repo, kind}
		if groups[g] == nil {
			groups[g] = &acc{st: RepoStats{Repo: repo, Kind: kind}}
		}
		return groups[g]
	}
	for _, j := range kept {
		a := get(j.Repo, kindOf(j))
		wait, run := QueueWait(j), RunTime(j)
		a.st.Jobs++
		a.st.WaitH += wait.Hours()
		a.st.RunH += run.Hours()
		a.waits = append(a.waits, wait.Minutes())
		never := NeverStarted(j)
		if never {
			a.st.NeverStarted++
			a.st.NeverStartedWaitH += wait.Hours()
		}
	}

	// A repo-level count goes on its macOS row, else its Linux row.
	home := func(repo string) *acc {
		if g := groups[groupKey{repo, "macos"}]; g != nil {
			return g
		}
		return groups[groupKey{repo, "linux"}]
	}
	for k, a := range maxAttempt {
		if g := home(k.repo); g != nil && a > 1 {
			g.st.Reruns += a - 1
		}
	}

	// Manual kicks belong to the kind of the dispatched run's jobs.
	kindsOfRun := map[runKey]map[string]bool{}
	neverRun := map[runKey]bool{}
	started := map[runKey]bool{}
	for _, j := range kept {
		k := runKey{j.Repo, j.RunID}
		if kindsOfRun[k] == nil {
			kindsOfRun[k] = map[string]bool{}
		}
		kindsOfRun[k][kindOf(j)] = true
		if !NeverStarted(j) {
			started[k] = true
		}
		neverRun[k] = true
	}
	for k := range neverRun {
		neverRun[k] = !started[k]
	}
	var live []Run
	for _, r := range runs {
		if !r.JobsUnknown {
			live = append(live, r)
		}
	}
	kicks, kicked := kickRuns(live, neverRun, IsBot)

	// Wasted: wait + run of cancelled or never-started jobs in an abandoned
	// attempt, one a later attempt superseded or a manual kick followed.
	// (The study's rust row has cancelled jobs but 0.0 wasted: a cancelled run
	// nobody re-ran or kicked is not abandoned work.)
	for _, j := range kept {
		k := runKey{j.Repo, j.RunID}
		if (j.Attempt < maxAttempt[k] || kicked[k]) && (j.Conclusion == "cancelled" || NeverStarted(j)) {
			groups[groupKey{j.Repo, kindOf(j)}].st.WastedH += (QueueWait(j) + RunTime(j)).Hours()
		}
	}
	for _, r := range kicks {
		k := runKey{r.Repo, r.ID}
		kind := "linux"
		switch {
		case kindsOfRun[k]["macos"]:
			kind = "macos"
		case len(kindsOfRun[k]) == 0:
			if groups[groupKey{r.Repo, "macos"}] != nil {
				kind = "macos"
			}
		}
		if g := groups[groupKey{r.Repo, kind}]; g != nil {
			g.st.ManualKicks++
		} else if g := home(r.Repo); g != nil {
			g.st.ManualKicks++
		}
	}

	out := make([]RepoStats, 0, len(groups))
	for _, a := range groups {
		sort.Float64s(a.waits)
		a.st.MedianWaitM = median(a.waits)
		a.st.P90WaitM = p90(a.waits)
		a.st.TotalH = a.st.WaitH + a.st.RunH
		out = append(out, a.st)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TotalH != out[j].TotalH {
			return out[i].TotalH > out[j].TotalH
		}
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

// ManualKicks counts a workflow_dispatch by a non-bot within 24h after a
// cancelled run of the same repo, workflow and branch.
func ManualKicks(runs []Run, isBot func(actor string) bool) int {
	kicks, _ := kickRuns(runs, nil, isBot)
	return len(kicks)
}

// kickRuns lists the dispatches that are manual kicks, and the runs they
// followed (kicked: the abandoned ones). A prior run counts as abandoned when
// cancelled or when neverRun reports its jobs never started.
func kickRuns(runs []Run, neverRun map[runKey]bool, isBot func(string) bool) (kicks []Run, kicked map[runKey]bool) {
	kicked = map[runKey]bool{}
	for _, d := range runs {
		if d.Event != "workflow_dispatch" || isBot(d.Actor) {
			continue
		}
		isKick := false
		for _, p := range runs {
			if p.Repo != d.Repo || p.Workflow != d.Workflow || p.HeadBranch != d.HeadBranch || p.ID == d.ID {
				continue
			}
			gap := d.Created.Sub(p.Created)
			if gap < 0 || gap > 24*time.Hour {
				continue
			}
			if p.Conclusion == "cancelled" || neverRun[runKey{p.Repo, p.ID}] {
				if !isKick {
					kicks = append(kicks, d)
					isKick = true
				}
				kicked[runKey{p.Repo, p.ID}] = true
			}
		}
	}
	return kicks, kicked
}
