package attention

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	Retryable(ctx context.Context, pr PR) (pattern string, ok bool, err error)
	InProgress(ctx context.Context, repo string) ([]model.Finding, error)
}

// NoRetry is the no-op Retry: nothing is retryable, nothing in progress.
type NoRetry struct{}

func (NoRetry) Retryable(context.Context, PR) (string, bool, error)         { return "", false, nil }
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
	repos, err := Repos(ctx, c, org)
	if err != nil {
		return nil, render.Checked{}, err
	}

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
	retrying   map[string]bool // PR items with a retry finding from r
}

func (rc *repoCollector) unread(path string, err error) {
	rc.res.findings = append(rc.res.findings, model.Finding{
		Repo: rc.repo, Item: path, Kind: model.KindCannotRead, Since: rc.now,
		Why: err.Error(), Fix: "check token scope and rate limit, then re-run",
	})
}

// unreadErr records err under the endpoint a *ReadError names, else under what.
func (rc *repoCollector) unreadErr(what string, err error) {
	var re *ReadError
	if errors.As(err, &re) {
		what = re.Path
	}
	rc.unread(what, err)
}

func collectRepo(ctx context.Context, c ghapi.Client, org, repo, maintainer string, r Retry, now time.Time) repoResult {
	rc := &repoCollector{ctx: ctx, c: c, org: org, repo: repo, maintainer: maintainer, r: r, now: now}
	// Retries fleet already requested come first: such a PR is reported by
	// its retry finding, not again by its red or green age.
	progress, err := r.InProgress(ctx, repo)
	if err != nil {
		rc.unread("retry markers for "+repo, err)
	}
	rc.retrying = map[string]bool{}
	for _, f := range progress {
		if f.Kind == model.KindRetryInProgress || f.Kind == model.KindStuckPR {
			rc.retrying[f.Item] = true
		}
	}
	rc.pulls()
	rc.issues()
	rc.workflows()
	rc.res.findings = append(rc.res.findings, progress...)
	return rc.res
}

func (rc *repoCollector) base() string { return base(rc.org, rc.repo) }

func (rc *repoCollector) pulls() {
	pulls, err := OpenPulls(rc.ctx, rc.c, rc.org, rc.repo)
	if err != nil {
		rc.unreadErr(rc.base()+"/pulls?state=open", err)
		return
	}
	rc.res.prs = len(pulls)
	for _, p := range pulls {
		pr := PR{Repo: rc.repo, Number: p.Number, Author: p.User.Login, HeadRef: p.Head.Ref,
			URL: p.HTMLURL, Body: p.Body, CreatedAt: p.CreatedAt}
		if rc.retrying[item(rc.repo, pr.Number)] {
			continue // its retry finding speaks for it
		}
		cs, settled, err := HeadChecks(rc.ctx, rc.c, rc.org, rc.repo, p.Head.SHA, p.CreatedAt)
		if err != nil {
			rc.unreadErr("checks for "+item(rc.repo, p.Number), err)
			continue
		}
		pr.Checks, pr.Settled = cs, settled
		// The verdict only matters once red is old enough to be stuck.
		if pr.Checks.State == "red" && !pr.Checks.NeedsApproval && rc.now.Sub(pr.Checks.Since) > stuckAfter {
			pattern, ok, err := rc.r.Retryable(rc.ctx, pr)
			if err != nil {
				// Unknown is not "not retryable": report only what failed.
				rc.unread(fmt.Sprintf("retryable check for %s#%d", rc.repo, pr.Number), err)
				continue
			}
			pr.Retryable, pr.RetryPattern = ok, pattern
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
		if f, ok := ClassifyPR(pr, rc.now); ok {
			rc.res.findings = append(rc.res.findings, f)
		}
	}
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

// workflows flags each active workflow whose newest run on main, or newest
// scheduled run, did not succeed. Newest-first means one run per query is enough.
func (rc *repoCollector) workflows() {
	wrs, unread, err := LatestRuns(rc.ctx, rc.c, rc.org, rc.repo)
	if err != nil {
		rc.unreadErr(rc.base()+"/actions/workflows", err)
		return
	}
	for _, e := range unread {
		rc.unreadErr("workflow runs for "+rc.repo, e)
	}
	for _, wr := range wrs {
		rc.res.runs += len(wr.Runs)
		for i, run := range wr.Runs {
			if !run.Failed() {
				continue
			}
			st := wr.Streaks[i]
			rc.res.findings = append(rc.res.findings, model.Finding{
				Repo: rc.repo, Item: rc.repo + ":" + wr.Workflow.Name, URL: run.HTMLURL, Kind: model.KindRedWorkflow,
				Since: st.Since,
				Why:   streakWhy(st),
				Fix:   rc.failingStep(run.ID),
			})
			break
		}
	}
}

// streakWhy says how long a workflow has been red: "red 3 runs in a row, since Nov 15".
func streakWhy(s Streak) string {
	since := s.Since.Format("Jan 2")
	switch {
	case s.Capped:
		return fmt.Sprintf("red %d+ runs in a row, since %s", s.Count, since)
	case s.Count == 1:
		return "red 1 run, since " + since
	}
	return fmt.Sprintf("red %d runs in a row, since %s", s.Count, since)
}

// failingStep is a red run's Fix: "job › step: error", as much of it as can be read.
func (rc *repoCollector) failingStep(runID int64) string {
	job, step, msg, err := FailingStep(rc.ctx, rc.c, rc.org, rc.repo, runID)
	if err != nil {
		rc.unreadErr(fmt.Sprintf("jobs for %s run %d", rc.repo, runID), err)
	}
	switch {
	case job == "":
		return "open the run, read the failing step"
	case msg == "":
		return job + " › " + step
	}
	return job + " › " + step + ": " + msg
}
