package attention

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Mavergreen/fleet/internal/ghapi"
)

// The org enumeration below is shared with the retry package, so attention and
// remediate agree on which PRs are red and which runs are latest.

// ReadError names the endpoint that could not be read; its message is the
// underlying error's.
type ReadError struct {
	Path string
	Err  error
}

func (e *ReadError) Error() string { return e.Err.Error() }
func (e *ReadError) Unwrap() error { return e.Err }

func base(org, repo string) string { return "/repos/" + org + "/" + repo }

type orgRepo struct {
	Name        string `json:"name"`
	Archived    bool   `json:"archived"`
	Permissions struct {
		Push bool `json:"push"`
	} `json:"permissions"`
}

func listRepos(ctx context.Context, c ghapi.Client, org string) ([]orgRepo, error) {
	var repos []orgRepo
	err := c.GetPaged(ctx, "/orgs/"+org+"/repos?type=public&per_page=100", func(raw json.RawMessage) error {
		var rp orgRepo
		if err := json.Unmarshal(raw, &rp); err != nil {
			return err
		}
		if !rp.Archived {
			repos = append(repos, rp)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing %s repos: %w", org, err)
	}
	return repos, nil
}

// Repos lists the org's non-archived repos, sorted.
func Repos(ctx context.Context, c ghapi.Client, org string) ([]string, error) {
	rs, err := listRepos(ctx, c, org)
	if err != nil {
		return nil, err
	}
	repos := make([]string, len(rs))
	for i, r := range rs {
		repos[i] = r.Name
	}
	sort.Strings(repos)
	return repos, nil
}

// Writable says which of the org's non-archived repos the token can push
// to, from the same listing as Repos (so a memoized client reads it once).
func Writable(ctx context.Context, c ghapi.Client, org string) (map[string]bool, error) {
	rs, err := listRepos(ctx, c, org)
	if err != nil {
		return nil, err
	}
	w := make(map[string]bool, len(rs))
	for _, r := range rs {
		w[r.Name] = r.Permissions.Push
	}
	return w, nil
}

// Pull is an open pull request as the pulls API reports it.
type Pull struct {
	Number    int       `json:"number"`
	HTMLURL   string    `json:"html_url"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
	User      user      `json:"user"`
	Head      struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
}

// Author is the PR author's login.
func (p Pull) Author() string { return p.User.Login }

// FromFork reports whether the head branch lives outside the base repo
// (including a deleted fork, whose head repo is null).
func (p Pull) FromFork() bool {
	return p.Head.Repo == nil || p.Head.Repo.FullName != p.Base.Repo.FullName
}

// OpenPulls lists a repo's open pull requests.
func OpenPulls(ctx context.Context, c ghapi.Client, org, repo string) ([]Pull, error) {
	path := base(org, repo) + "/pulls?state=open"
	var pulls []Pull
	err := c.GetPaged(ctx, path, func(raw json.RawMessage) error {
		var p Pull
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		pulls = append(pulls, p)
		return nil
	})
	if err != nil {
		return nil, &ReadError{path, err}
	}
	return pulls, nil
}

// HeadChecks reads the check runs and commit statuses on sha and summarises
// them: red if anything failed, else pending if anything is unfinished, else
// green. Since falls back to created when nothing has a time. The
// ingredients-settled status is returned separately and never colours the head.
func HeadChecks(ctx context.Context, c ghapi.Client, org, repo, sha string, created time.Time) (CheckState, *Status, error) {
	cs, settled, _, err := HeadChecksDetail(ctx, c, org, repo, sha, created)
	return cs, settled, err
}

// RedCheck is one failed check run or commit status on a head.
type RedCheck struct {
	Name         string // check run name, or status context
	CheckSuiteID int64  // 0 for a commit status
	IsStatus     bool
}

// HeadChecksDetail is HeadChecks plus the red items that made the head red.
func HeadChecksDetail(ctx context.Context, c ghapi.Client, org, repo, sha string, created time.Time) (CheckState, *Status, []RedCheck, error) {
	crPath := fmt.Sprintf("%s/commits/%s/check-runs?per_page=100", base(org, repo), sha)
	type checkRun struct {
		Name       string `json:"name"`
		CheckSuite struct {
			ID int64 `json:"id"`
		} `json:"check_suite"`
		Status      string    `json:"status"`
		Conclusion  string    `json:"conclusion"`
		StartedAt   time.Time `json:"started_at"`
		CompletedAt time.Time `json:"completed_at"`
	}
	var checkRuns []checkRun
	err := c.GetPaged(ctx, crPath, func(raw json.RawMessage) error {
		var k checkRun
		if err := json.Unmarshal(raw, &k); err != nil {
			return err
		}
		checkRuns = append(checkRuns, k)
		return nil
	})
	if err != nil {
		return CheckState{}, nil, nil, &ReadError{crPath, err}
	}
	stPath := fmt.Sprintf("%s/commits/%s/status", base(org, repo), sha)
	var st struct {
		Statuses []struct {
			Context     string    `json:"context"`
			State       string    `json:"state"`
			Description string    `json:"description"`
			UpdatedAt   time.Time `json:"updated_at"`
		} `json:"statuses"`
	}
	if err := c.Get(ctx, stPath, &st); err != nil {
		return CheckState{}, nil, nil, &ReadError{stPath, err}
	}
	var settled *Status
	var reds []RedCheck
	red, pending := false, false
	approvalOnly := true // every red item is a run awaiting approval
	var since time.Time
	later := func(t time.Time) {
		if t.After(since) {
			since = t
		}
	}
	for _, k := range checkRuns {
		if k.Status != "completed" {
			pending = true
			later(k.StartedAt)
			continue
		}
		later(k.CompletedAt)
		switch k.Conclusion {
		case "failure", "timed_out", "cancelled", "action_required":
			red = true
			approvalOnly = approvalOnly && k.Conclusion == "action_required"
			reds = append(reds, RedCheck{Name: k.Name, CheckSuiteID: k.CheckSuite.ID})
		}
	}
	for _, s := range st.Statuses {
		if s.Context == settledContext {
			settled = &Status{State: s.State, Description: s.Description}
			continue
		}
		later(s.UpdatedAt)
		switch s.State {
		case "failure", "error":
			red = true
			approvalOnly = false
			reds = append(reds, RedCheck{Name: s.Context, IsStatus: true})
		case "pending":
			pending = true
		}
	}
	var cs CheckState
	switch {
	case red:
		cs.State = "red"
		cs.NeedsApproval = approvalOnly
	case pending:
		cs.State = "pending"
	default:
		cs.State = "green"
	}
	if since.IsZero() {
		since = created
	}
	cs.Since = since
	return cs, settled, reds, nil
}

// Run is a workflow run as the Actions API reports it.
type Run struct {
	ID         int64  `json:"id"`
	WorkflowID int64  `json:"workflow_id"`
	Name       string `json:"name"`
	Path       string `json:"path"`
	Event      string `json:"event"`
	HeadBranch string `json:"head_branch"`
	HeadSHA    string `json:"head_sha"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	RunAttempt int    `json:"run_attempt"`
	// CheckSuiteID links the run to the check runs it reported.
	CheckSuiteID int64     `json:"check_suite_id"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	// RunStartedAt is when the current attempt started.
	RunStartedAt time.Time `json:"run_started_at"`
	HTMLURL      string    `json:"html_url"`
}

// Failed reports whether a completed run ended red.
func (r Run) Failed() bool {
	if r.Status != "completed" {
		return false
	}
	switch r.Conclusion {
	case "failure", "timed_out", "startup_failure":
		return true
	}
	return false
}

// Workflow is an active workflow.
type Workflow struct {
	ID   int64
	Name string
}

// WorkflowRuns holds a workflow's newest run on main and its newest scheduled
// run, in that order, without duplicates.
type WorkflowRuns struct {
	Workflow Workflow
	Runs     []Run
	// Streaks is index-aligned with Runs: how long each kept run's query has
	// been failing. A run that did not fail has the zero Streak.
	Streaks []Streak
}

// runsPerQuery runs are read per query, newest first: enough for a streak, at
// the cost of one request, the same as reading only the newest.
const runsPerQuery = 30

// Streak counts consecutive failed runs, newest first. Capped means every run
// read failed, so the true count is at least Count. Since is when the oldest
// of them finished.
type Streak struct {
	Count  int
	Capped bool
	Since  time.Time
}

func streakOf(runs []Run) Streak {
	var s Streak
	for _, r := range runs {
		if !r.Failed() {
			return s
		}
		s.Count, s.Since = s.Count+1, r.UpdatedAt
	}
	s.Capped = len(runs) >= runsPerQuery
	return s
}

// LatestRuns returns, for each active workflow (sorted by ID), its newest run
// on main and newest scheduled run. A failure to list workflows is returned as
// err; failures of single run queries are returned in unread and skipped.
func LatestRuns(ctx context.Context, c ghapi.Client, org, repo string) (wrs []WorkflowRuns, unread []error, err error) {
	wpath := base(org, repo) + "/actions/workflows"
	var active []Workflow
	err = c.GetPaged(ctx, wpath, func(raw json.RawMessage) error {
		var w struct {
			ID    int64  `json:"id"`
			Name  string `json:"name"`
			State string `json:"state"`
		}
		if err := json.Unmarshal(raw, &w); err != nil {
			return err
		}
		if w.State == "active" {
			active = append(active, Workflow{w.ID, w.Name})
		}
		return nil
	})
	if err != nil {
		return nil, nil, &ReadError{wpath, err} // cannot tell live workflows from dead ones
	}
	sort.Slice(active, func(i, j int) bool { return active[i].ID < active[j].ID })
	for _, w := range active {
		wr := WorkflowRuns{Workflow: w}
		for _, q := range []string{"branch=main", "event=schedule"} {
			path := fmt.Sprintf("%s/actions/workflows/%d/runs?%s&per_page=%d", base(org, repo), w.ID, q, runsPerQuery)
			var resp struct {
				Runs []Run `json:"workflow_runs"`
			}
			if err := c.Get(ctx, path, &resp); err != nil {
				unread = append(unread, &ReadError{path, err})
				continue
			}
			if len(resp.Runs) == 0 {
				continue
			}
			dup := false
			for _, r := range wr.Runs {
				dup = dup || r.ID == resp.Runs[0].ID
			}
			if !dup {
				wr.Runs = append(wr.Runs, resp.Runs[0])
				wr.Streaks = append(wr.Streaks, streakOf(resp.Runs))
			}
		}
		wrs = append(wrs, wr)
	}
	return wrs, unread, nil
}

var genericExit = regexp.MustCompile(`^Process completed with exit code \d+\.?$`)

// FailingStep names a failed run's first failed job and step, and the first
// failure annotation on that job that says more than its exit code (first line
// only). job is "" when no step failed. A jobs read error comes back as err; an
// annotations read error comes back as err with job and step filled in.
func FailingStep(ctx context.Context, c ghapi.Client, org, repo string, runID int64) (job, step, msg string, err error) {
	jpath := fmt.Sprintf("%s/actions/runs/%d/jobs?per_page=100", base(org, repo), runID)
	var jobs struct {
		Jobs []struct {
			ID         int64  `json:"id"`
			Name       string `json:"name"`
			Conclusion string `json:"conclusion"`
			Steps      []struct {
				Name       string `json:"name"`
				Conclusion string `json:"conclusion"`
			} `json:"steps"`
		} `json:"jobs"`
	}
	if err := c.Get(ctx, jpath, &jobs); err != nil {
		return "", "", "", &ReadError{jpath, err}
	}
	var jobID int64
	for _, j := range jobs.Jobs {
		if j.Conclusion != "failure" {
			continue
		}
		for _, st := range j.Steps {
			if st.Conclusion == "failure" {
				job, step, jobID = j.Name, st.Name, j.ID
				break
			}
		}
		if job != "" {
			break
		}
	}
	if job == "" {
		return "", "", "", nil
	}
	apath := fmt.Sprintf("%s/check-runs/%d/annotations", base(org, repo), jobID)
	var anns []struct {
		Level   string `json:"annotation_level"`
		Message string `json:"message"`
	}
	if err := c.Get(ctx, apath, &anns); err != nil {
		return job, step, "", &ReadError{apath, err}
	}
	for _, a := range anns {
		line := strings.TrimSpace(strings.SplitN(a.Message, "\n", 2)[0])
		if a.Level == "failure" && line != "" && !genericExit.MatchString(line) {
			return job, step, line, nil
		}
	}
	return job, step, "", nil
}
