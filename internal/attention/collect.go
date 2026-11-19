package attention

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Mavergreen/fleet/internal/ghapi"
	"github.com/Mavergreen/fleet/internal/model"
	"github.com/Mavergreen/fleet/internal/render"
)

// Retry is supplied by the retry package (wired in internal/cli).
// Implementations must be safe for concurrent use: Collect calls them from one
// goroutine per repo.
type Retry interface {
	Retryable(ctx context.Context, pr PR) (bool, error)
	InProgress(ctx context.Context, repo string) ([]model.Finding, error)
}

// NoRetry is the no-op Retry: nothing is retryable, nothing in progress.
type NoRetry struct{}

func (NoRetry) Retryable(context.Context, PR) (bool, error)                 { return false, nil }
func (NoRetry) InProgress(context.Context, string) ([]model.Finding, error) { return nil, nil }

type user struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

func (u user) isBot() bool { return u.Type == "Bot" || strings.HasSuffix(u.Login, "[bot]") }

const settledContext = "ingredients-settled"

// Collect reads the org and returns findings plus what was checked. An empty
// maintainer is taken from GET /user. Per-endpoint failures become cannot-read
// findings; only failures that prevent any work (repo list, identity) are errors.
func Collect(ctx context.Context, c ghapi.Client, org, maintainer string, r Retry, now time.Time) ([]model.Finding, render.Checked, error) {
	if maintainer == "" {
		var u user
		if err := c.Get(ctx, "/user", &u); err != nil {
			return nil, render.Checked{}, fmt.Errorf("identifying maintainer: %w", err)
		}
		maintainer = u.Login
	}
	var repos []string
	err := c.GetPaged(ctx, "/orgs/"+org+"/repos?type=public&per_page=100", func(raw json.RawMessage) error {
		var rp struct {
			Name     string `json:"name"`
			Archived bool   `json:"archived"`
		}
		if err := json.Unmarshal(raw, &rp); err != nil {
			return err
		}
		if !rp.Archived {
			repos = append(repos, rp.Name)
		}
		return nil
	})
	if err != nil {
		return nil, render.Checked{}, fmt.Errorf("listing %s repos: %w", org, err)
	}
	sort.Strings(repos)

	results := make([]repoResult, len(repos))
	var wg sync.WaitGroup
	for i, name := range repos {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = collectRepo(ctx, c, org, name, maintainer, r, now)
		}()
	}
	wg.Wait()

	var all []model.Finding
	checked := render.Checked{Repos: len(repos)}
	for _, rr := range results {
		all = append(all, rr.findings...)
		checked.PRs += rr.prs
		checked.Issues += rr.issues
		checked.Runs += rr.runs
	}
	return all, checked, nil
}

type repoResult struct {
	findings          []model.Finding
	prs, issues, runs int
}

type repoCollector struct {
	ctx        context.Context
	c          ghapi.Client
	org, repo  string
	maintainer string
	r          Retry
	now        time.Time
	res        repoResult
}

func (rc *repoCollector) unread(path string, err error) {
	rc.res.findings = append(rc.res.findings, model.Finding{
		Repo: rc.repo, Item: path, Kind: model.KindCannotRead, Since: rc.now,
		Why: err.Error(), Fix: "check token scope and rate limit, then re-run",
	})
}

func collectRepo(ctx context.Context, c ghapi.Client, org, repo, maintainer string, r Retry, now time.Time) repoResult {
	rc := &repoCollector{ctx: ctx, c: c, org: org, repo: repo, maintainer: maintainer, r: r, now: now}
	rc.pulls()
	rc.issues()
	rc.workflows()
	if fs, err := r.InProgress(ctx, repo); err != nil {
		rc.unread("retry markers for "+repo, err)
	} else {
		rc.res.findings = append(rc.res.findings, fs...)
	}
	return rc.res
}

func (rc *repoCollector) base() string { return "/repos/" + rc.org + "/" + rc.repo }

type apiPull struct {
	Number    int       `json:"number"`
	HTMLURL   string    `json:"html_url"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	User      user      `json:"user"`
	Head      struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
}

func (rc *repoCollector) pulls() {
	path := rc.base() + "/pulls?state=open"
	var pulls []apiPull
	err := rc.c.GetPaged(rc.ctx, path, func(raw json.RawMessage) error {
		var p apiPull
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		pulls = append(pulls, p)
		return nil
	})
	if err != nil {
		rc.unread(path, err)
		return
	}
	rc.res.prs = len(pulls)
	for _, p := range pulls {
		pr := PR{Repo: rc.repo, Number: p.Number, Author: p.User.Login, HeadRef: p.Head.Ref,
			URL: p.HTMLURL, Body: p.Body, CreatedAt: p.CreatedAt}
		if !rc.checks(&pr, p.Head.SHA) {
			continue
		}
		retryable := false
		if pr.Checks.State == "red" {
			ok, err := rc.r.Retryable(rc.ctx, pr)
			if err != nil {
				rc.unread(fmt.Sprintf("retryable check for %s#%d", rc.repo, pr.Number), err)
			}
			retryable = ok && err == nil
		}
		if pr.Checks.State == "green" && rc.now.Sub(pr.Checks.Since) > stuckAfter {
			// mergeable is only worth a call when it could appear in a finding
			ppath := fmt.Sprintf("%s/pulls/%d", rc.base(), pr.Number)
			var full struct {
				Mergeable *bool `json:"mergeable"`
			}
			if err := rc.c.Get(rc.ctx, ppath, &full); err != nil {
				rc.unread(ppath, err)
			} else {
				pr.Mergeable = full.Mergeable
			}
		}
		if f, ok := ClassifyPR(pr, retryable, rc.now); ok {
			rc.res.findings = append(rc.res.findings, f)
		}
	}
}

// checks fills pr.Checks and pr.Settled; false means the head could not be read.
func (rc *repoCollector) checks(pr *PR, sha string) bool {
	crPath := fmt.Sprintf("%s/commits/%s/check-runs?per_page=100", rc.base(), sha)
	type checkRun struct {
		Status      string    `json:"status"`
		Conclusion  string    `json:"conclusion"`
		StartedAt   time.Time `json:"started_at"`
		CompletedAt time.Time `json:"completed_at"`
	}
	var checkRuns []checkRun
	err := rc.c.GetPaged(rc.ctx, crPath, func(raw json.RawMessage) error {
		var k checkRun
		if err := json.Unmarshal(raw, &k); err != nil {
			return err
		}
		checkRuns = append(checkRuns, k)
		return nil
	})
	if err != nil {
		rc.unread(crPath, err)
		return false
	}
	stPath := fmt.Sprintf("%s/commits/%s/status", rc.base(), sha)
	var st struct {
		Statuses []struct {
			Context     string    `json:"context"`
			State       string    `json:"state"`
			Description string    `json:"description"`
			UpdatedAt   time.Time `json:"updated_at"`
		} `json:"statuses"`
	}
	if err := rc.c.Get(rc.ctx, stPath, &st); err != nil {
		rc.unread(stPath, err)
		return false
	}
	red, pending := false, false
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
		}
	}
	for _, s := range st.Statuses {
		if s.Context == settledContext {
			pr.Settled = &Status{State: s.State, Description: s.Description}
			continue
		}
		later(s.UpdatedAt)
		switch s.State {
		case "failure", "error":
			red = true
		case "pending":
			pending = true
		}
	}
	switch {
	case red:
		pr.Checks.State = "red"
	case pending:
		pr.Checks.State = "pending"
	default:
		pr.Checks.State = "green"
	}
	if since.IsZero() {
		since = pr.CreatedAt
	}
	pr.Checks.Since = since
	return true
}

type apiIssue struct {
	Number    int             `json:"number"`
	HTMLURL   string          `json:"html_url"`
	Title     string          `json:"title"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
	User      user            `json:"user"`
	PR        json.RawMessage `json:"pull_request"`
}

func (rc *repoCollector) issues() {
	path := rc.base() + "/issues?state=open"
	var list []apiIssue
	err := rc.c.GetPaged(rc.ctx, path, func(raw json.RawMessage) error {
		var i apiIssue
		if err := json.Unmarshal(raw, &i); err != nil {
			return err
		}
		list = append(list, i)
		return nil
	})
	if err != nil {
		rc.unread(path, err)
		return
	}
	for _, i := range list {
		isPR := len(i.PR) > 0 && string(i.PR) != "null"
		if !isPR {
			rc.res.issues++
		}
		t := Thread{Repo: rc.repo, Number: i.Number, URL: i.HTMLURL, Author: i.User.Login,
			AuthorIsBot: i.User.isBot(), Title: i.Title, IsPR: isPR,
			LastCommenter: i.User.Login, LastActivity: i.CreatedAt}
		if t.AuthorIsBot {
			continue // NeedsReply would drop it; save the comments call
		}
		cpath := fmt.Sprintf("%s/issues/%d/comments?per_page=100", rc.base(), i.Number)
		err := rc.c.GetPaged(rc.ctx, cpath, func(raw json.RawMessage) error {
			var cm struct {
				CreatedAt time.Time `json:"created_at"`
				User      user      `json:"user"`
			}
			if err := json.Unmarshal(raw, &cm); err != nil {
				return err
			}
			if !cm.User.isBot() { // a bot is never the person waiting
				t.LastCommenter, t.LastActivity = cm.User.Login, cm.CreatedAt
			}
			return nil
		})
		if err != nil {
			rc.unread(cpath, err)
			continue
		}
		if f, ok := NeedsReply(t, rc.maintainer); ok {
			rc.res.findings = append(rc.res.findings, f)
		}
	}
}

type apiRun struct {
	ID         int64     `json:"id"`
	WorkflowID int64     `json:"workflow_id"`
	Name       string    `json:"name"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	HTMLURL    string    `json:"html_url"`
}

// workflows flags each active workflow whose newest run on main, or newest
// scheduled run, did not succeed. Newest-first means one run per query is enough.
func (rc *repoCollector) workflows() {
	wpath := rc.base() + "/actions/workflows"
	type wf struct {
		ID   int64
		Name string
	}
	var active []wf
	err := rc.c.GetPaged(rc.ctx, wpath, func(raw json.RawMessage) error {
		var w struct {
			ID    int64  `json:"id"`
			Name  string `json:"name"`
			State string `json:"state"`
		}
		if err := json.Unmarshal(raw, &w); err != nil {
			return err
		}
		if w.State == "active" {
			active = append(active, wf{w.ID, w.Name})
		}
		return nil
	})
	if err != nil {
		rc.unread(wpath, err) // cannot tell live workflows from dead ones
		return
	}
	sort.Slice(active, func(i, j int) bool { return active[i].ID < active[j].ID })
	for _, w := range active {
		seen := map[int64]bool{}
		reported := false
		for _, q := range []string{"branch=main", "event=schedule"} {
			path := fmt.Sprintf("%s/actions/workflows/%d/runs?%s&per_page=1", rc.base(), w.ID, q)
			var resp struct {
				Runs []apiRun `json:"workflow_runs"`
			}
			if err := rc.c.Get(rc.ctx, path, &resp); err != nil {
				rc.unread(path, err)
				continue
			}
			if len(resp.Runs) == 0 {
				continue
			}
			run := resp.Runs[0]
			if !seen[run.ID] {
				seen[run.ID] = true
				rc.res.runs++
			}
			if run.Status != "completed" || reported {
				continue
			}
			switch run.Conclusion {
			case "failure", "timed_out", "startup_failure":
			default:
				continue
			}
			reported = true
			rc.res.findings = append(rc.res.findings, model.Finding{
				Repo: rc.repo, Item: rc.repo + ":" + w.Name, URL: run.HTMLURL, Kind: model.KindRedWorkflow,
				Since: run.UpdatedAt,
				Why:   fmt.Sprintf("latest run %s %s ago", run.Conclusion, age(rc.now.Sub(run.UpdatedAt))),
				Fix:   "open the run, read the failing step",
			})
		}
	}
}
