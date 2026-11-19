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
		cs, settled, err := HeadChecks(rc.ctx, rc.c, rc.org, rc.repo, p.Head.SHA, p.CreatedAt)
		if err != nil {
			rc.unreadErr("checks for "+item(rc.repo, p.Number), err)
			continue
		}
		pr.Checks, pr.Settled = cs, settled
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
		for _, run := range wr.Runs {
			if !run.Failed() {
				continue
			}
			rc.res.findings = append(rc.res.findings, model.Finding{
				Repo: rc.repo, Item: rc.repo + ":" + wr.Workflow.Name, URL: run.HTMLURL, Kind: model.KindRedWorkflow,
				Since: run.UpdatedAt,
				Why:   fmt.Sprintf("latest run %s %s ago", run.Conclusion, age(rc.now.Sub(run.UpdatedAt))),
				Fix:   "open the run, read the failing step",
			})
			break
		}
	}
}
