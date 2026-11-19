package retry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Mavergreen/fleet/internal/attention"
	"github.com/Mavergreen/fleet/internal/ghapi"
	"github.com/Mavergreen/fleet/internal/model"
)

// RerunsWork records whether Task 0's spike proved that re-running a failed
// run passes install@v1 (github.action_ref is empty on a re-run). Until it
// has, runs on main and scheduled runs are report-only.
const RerunsWork = false

// rerunsEnabled is RerunsWork; tests flip it to exercise the re-run path.
var rerunsEnabled = RerunsWork

// ActionKind is what remediate does about one red item.
type ActionKind string

const (
	KindTickBox ActionKind = "tick-box" // tick a Renovate PR's rebase/retry box
	KindRerun   ActionKind = "rerun"    // re-run a main or scheduled run's failed jobs
	KindReport  ActionKind = "report"   // leave it for a human
)

const (
	noteExhausted  = "retries exhausted (2/2)"
	noteRequested  = "already requested"
	noteNoBox      = "no rebase/retry checkbox"
	noteDisabled   = "reruns disabled until verified (Task 0)"
	noteDeliberate = "cancelled — not retrying a possibly deliberate cancel"
	noteCutRelease = "re-dispatch with cut-release.sh"
	noteInProgress = "run in progress"
	noteNotFailed  = "run no longer failed"
)

// neverStartedMinWait is how long a never-started job must have waited for a
// runner before its cancellation counts as starvation, not a deliberate cancel.
const neverStartedMinWait = time.Hour

// Action is one planned remediation. Update and HeadSHA record what Plan saw,
// so Apply can refuse to act if either has changed.
type Action struct {
	Kind      ActionKind `json:"kind"`
	Org       string     `json:"org"`
	Repo      string     `json:"repo"`
	PR        int        `json:"pr,omitempty"`
	RunID     int64      `json:"run_id,omitempty"`
	RunURL    string     `json:"run_url,omitempty"`
	PatternID string     `json:"pattern,omitempty"`
	Attempt   int        `json:"attempt,omitempty"`
	Note      string     `json:"note"`
	// Error is the read error behind a "cannot read" report. Such a report
	// is not a failure of remediate: the exit code stays 0, as for attention.
	Error   string  `json:"error,omitempty"`
	Update  string  `json:"update,omitempty"`
	HeadSHA string  `json:"head_sha,omitempty"`
	RunIDs  []int64 `json:"run_ids,omitempty"` // every failed run a tick-box retries
}

// Result is what Apply did. Action is the action as finally taken: an action
// Apply declined comes back as a report carrying the reason.
type Result struct {
	Action Action
	Done   bool
	Err    error
	Note   string
}

// Plan reads the org and decides, for every red open PR and every red latest
// main or scheduled run, whether to tick, re-run or report. It writes nothing.
// Only failing to identify the token's user or to list repos is an error;
// anything else unreadable becomes a report naming what could not be read.
func Plan(ctx context.Context, c ghapi.Client, org string, scripts string, now time.Time) ([]Action, error) {
	login, err := whoami(ctx, c)
	if err != nil {
		return nil, err
	}
	repos, err := attention.Repos(ctx, c, org)
	if err != nil {
		return nil, err
	}
	plans := make([]*planner, len(repos))
	var wg sync.WaitGroup
	for i, repo := range repos {
		wg.Add(1)
		go func() {
			defer wg.Done()
			plans[i] = planRepo(ctx, c, org, repo, scripts, login, now, false)
		}()
	}
	wg.Wait()
	var out []Action
	for _, p := range plans {
		out = append(out, p.actions...)
	}
	return out, nil
}

func whoami(ctx context.Context, c ghapi.Client) (string, error) {
	var u struct {
		Login string `json:"login"`
	}
	if err := c.Get(ctx, "/user", &u); err != nil {
		return "", fmt.Errorf("identifying the token's user: %w", err)
	}
	if u.Login == "" {
		return "", errors.New("identifying the token's user: empty login")
	}
	return u.Login, nil
}

func repoBase(org, repo string) string { return "/repos/" + org + "/" + repo }

// red reports whether a completed run ended in a way remediate looks at.
// Cancelled is included so a never-started job can be recognised; the
// never-started gate keeps deliberate cancels report-only.
func red(r attention.Run) bool {
	return r.Failed() || (r.Status == "completed" && r.Conclusion == "cancelled")
}

type ghComment struct {
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
}

// ourComments returns only the comments the token's user wrote: anyone can
// post a marker, and a spoofed one must not count.
func ourComments(ctx context.Context, c ghapi.Client, base string, pr int, login string) ([]ghComment, error) {
	p := fmt.Sprintf("%s/issues/%d/comments?per_page=100", base, pr)
	var out []ghComment
	err := c.GetPaged(ctx, p, func(raw json.RawMessage) error {
		var cm ghComment
		if err := json.Unmarshal(raw, &cm); err != nil {
			return err
		}
		if cm.User.Login == login {
			out = append(out, cm)
		}
		return nil
	})
	if err != nil {
		return nil, &attention.ReadError{Path: p, Err: err}
	}
	return out, nil
}

func bodies(cs []ghComment) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Body
	}
	return out
}

func readNote(err error) string {
	var re *attention.ReadError
	if errors.As(err, &re) {
		return "cannot read " + re.Path + ": " + err.Error()
	}
	return "cannot read: " + err.Error()
}

// planner plans one repo.
type planner struct {
	ctx                       context.Context
	c                         ghapi.Client
	org, repo, scripts, login string
	now                       time.Time
	progress                  bool // also collect in-progress findings

	pats      []Pattern
	patErr    error
	patLoaded bool
	comments  map[int][]ghComment
	headRuns  map[string][]attention.Run

	actions    []Action
	retryable  map[int]bool
	unreadable map[int]error
	findings   []model.Finding
}

func planRepo(ctx context.Context, c ghapi.Client, org, repo, scripts, login string, now time.Time, progress bool) *planner {
	p := &planner{ctx: ctx, c: c, org: org, repo: repo, scripts: scripts, login: login, now: now,
		progress: progress, comments: map[int][]ghComment{}, headRuns: map[string][]attention.Run{},
		retryable: map[int]bool{}, unreadable: map[int]error{}}
	pulls, err := attention.OpenPulls(ctx, c, org, repo)
	if err != nil {
		p.reportErr(0, 0, readNote(err), err)
	}
	sort.Slice(pulls, func(i, j int) bool { return pulls[i].Number < pulls[j].Number })
	for _, pl := range pulls {
		p.pull(pl)
	}
	p.runs()
	return p
}

func (p *planner) base() string { return repoBase(p.org, p.repo) }

func (p *planner) action(kind ActionKind, pr int, runID int64, note string) Action {
	return Action{Kind: kind, Org: p.org, Repo: p.repo, PR: pr, RunID: runID, Note: note}
}

func (p *planner) report(pr int, runID int64, note string) {
	p.actions = append(p.actions, p.action(KindReport, pr, runID, note))
}

// reportErr reports something that could not be read, carrying the error.
func (p *planner) reportErr(pr int, runID int64, note string, err error) {
	a := p.action(KindReport, pr, runID, note)
	a.Error = err.Error()
	p.actions = append(p.actions, a)
}

// unreadPR reports a PR whose verdict could not be read; Retryable returns err for it.
func (p *planner) unreadPR(pr int, runID int64, note string, err error) {
	p.unreadable[pr] = err
	p.reportErr(pr, runID, note, err)
}

func (p *planner) patterns() ([]Pattern, error) {
	if !p.patLoaded {
		p.patLoaded = true
		decl, err := LoadDeclared(p.ctx, p.c, p.org, p.repo, p.scripts)
		if err != nil {
			p.patErr = err
		} else {
			p.pats = append(Builtin(), decl...)
		}
	}
	return p.pats, p.patErr
}

func (p *planner) ourComments(pr int) ([]ghComment, error) {
	if cs, ok := p.comments[pr]; ok {
		return cs, nil
	}
	cs, err := ourComments(p.ctx, p.c, p.base(), pr, p.login)
	if err == nil {
		p.comments[pr] = cs
	}
	return cs, err
}

// runsOnHead returns the newest run of each workflow on sha.
func (p *planner) runsOnHead(sha string) ([]attention.Run, error) {
	if rs, ok := p.headRuns[sha]; ok {
		return rs, nil
	}
	out, err := newestOnHead(p.ctx, p.c, p.base(), sha)
	if err == nil {
		p.headRuns[sha] = out
	}
	return out, err
}

// newestOnHead returns the newest run on sha for each (workflow, event): one
// workflow can run for both push and pull_request on the same head.
func newestOnHead(ctx context.Context, c ghapi.Client, base, sha string) ([]attention.Run, error) {
	path := base + "/actions/runs?head_sha=" + sha + "&per_page=100"
	var resp struct {
		Runs []attention.Run `json:"workflow_runs"`
	}
	if err := c.Get(ctx, path, &resp); err != nil {
		return nil, &attention.ReadError{Path: path, Err: err}
	}
	type key struct {
		wid   int64
		event string
	}
	seen := map[key]bool{}
	var out []attention.Run
	for _, r := range resp.Runs { // newest first
		k := key{r.WorkflowID, r.Event}
		if !seen[k] {
			seen[k] = true
			out = append(out, r)
		}
	}
	return out, nil
}

func isRenovate(pl attention.Pull) bool {
	return strings.HasPrefix(pl.Head.Ref, "renovate/") && !pl.FromFork()
}

func (p *planner) pull(pl attention.Pull) {
	n := pl.Number
	if p.progress && isRenovate(pl) {
		p.prProgress(pl)
	}
	cs, _, reds, err := attention.HeadChecksDetail(p.ctx, p.c, p.org, p.repo, pl.Head.SHA, pl.CreatedAt)
	if err != nil {
		p.unreadPR(n, 0, readNote(err), err)
		return
	}
	if cs.State != "red" {
		return
	}
	switch {
	case pl.FromFork():
		p.report(n, 0, "fork PR: report-only")
		return
	case !strings.HasPrefix(pl.Head.Ref, "renovate/"):
		p.report(n, 0, "not a Renovate branch: report-only")
		return
	}
	comments, err := p.ourComments(n)
	if err != nil {
		p.unreadPR(n, 0, readNote(err), err)
		return
	}
	update := UpdateKey(pl.Title)
	attempt := CountRetries(bodies(comments), update) + 1
	if attempt > Cap {
		p.report(n, 0, noteExhausted)
		return
	}
	switch _, st := TickRebaseBox(pl.Body); st {
	case BoxAbsent:
		p.report(n, 0, noteNoBox)
		return
	case BoxAlreadyTicked:
		p.report(n, 0, noteRequested)
		return
	}
	runs, err := p.runsOnHead(pl.Head.SHA)
	if err != nil {
		p.unreadPR(n, 0, readNote(err), err)
		return
	}
	var failed []attention.Run
	for _, r := range runs {
		if r.Status != "completed" {
			p.report(n, r.ID, "a run on the head is still in progress")
			return
		}
		if red(r) {
			failed = append(failed, r)
		}
	}
	// Ticking re-runs only workflows; a red check from anything else would
	// still be red afterwards, so never tick past one.
	suites := map[int64]bool{}
	for _, r := range failed {
		if r.CheckSuiteID != 0 {
			suites[r.CheckSuiteID] = true
		}
	}
	for _, rc := range reds {
		if rc.IsStatus || !suites[rc.CheckSuiteID] {
			p.report(n, 0, "red check not from a classified workflow run: "+rc.Name)
			return
		}
	}
	if len(failed) == 0 {
		p.report(n, 0, "red, but no failed workflow run on the head (a status or external check)")
		return
	}
	idset := map[string]bool{}
	for _, r := range failed {
		v := p.classifyRun(r)
		if v.err != nil {
			p.unreadPR(n, r.ID, v.note, v.err)
			return
		}
		if v.note != "" {
			p.report(n, r.ID, v.note)
			return
		}
		for _, id := range v.ids {
			idset[id] = true
		}
	}
	ids := sortedKeys(idset)
	a := p.action(KindTickBox, n, failed[0].ID,
		fmt.Sprintf("retry %d/%d, pattern %s", attempt, Cap, strings.Join(ids, ",")))
	a.RunURL, a.PatternID, a.Attempt, a.Update, a.HeadSHA = failed[0].HTMLURL, strings.Join(ids, ","), attempt, update, pl.Head.SHA
	for _, r := range failed {
		a.RunIDs = append(a.RunIDs, r.ID)
	}
	p.actions = append(p.actions, a)
	p.retryable[n] = true
}

func sortedKeys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// runs plans each active workflow's newest main run and newest scheduled run.
func (p *planner) runs() {
	wrs, unread, err := attention.LatestRuns(p.ctx, p.c, p.org, p.repo)
	if err != nil {
		p.reportErr(0, 0, readNote(err), err)
		return
	}
	for _, e := range unread {
		p.reportErr(0, 0, readNote(e), e)
	}
	seen := map[int64]bool{}
	for _, wr := range wrs {
		for _, r := range wr.Runs {
			if seen[r.ID] {
				continue
			}
			seen[r.ID] = true
			if p.progress && r.Status != "completed" && r.RunAttempt > 1 {
				since := r.RunStartedAt
				if since.IsZero() {
					since = r.UpdatedAt
				}
				p.findings = append(p.findings, model.Finding{
					Repo: p.repo, Item: p.repo + ":" + wr.Workflow.Name, URL: r.HTMLURL,
					Kind: model.KindRetryInProgress, Since: since,
					Why: fmt.Sprintf("re-run %d/%d in progress, started %s ago", r.RunAttempt-1, Cap, attention.Age(p.now.Sub(since))),
					Fix: "wait for it to finish",
				})
			}
			if red(r) {
				p.mainRun(r)
			}
		}
	}
}

// rerunEvents are the events whose runs belong to the repo itself; a
// pull_request run on a branch named main may come from a fork.
var rerunEvents = map[string]bool{"push": true, "schedule": true, "workflow_dispatch": true}

func (p *planner) mainRun(r attention.Run) {
	switch {
	case strings.HasPrefix(r.HeadBranch, "renovate/"):
		p.report(0, r.ID, "renovate branch: retried by its PR's checkbox, never re-run")
		return
	case !rerunEvents[r.Event]:
		p.report(0, r.ID, fmt.Sprintf("run from event %q: report-only", r.Event))
		return
	case r.RunAttempt > Cap:
		p.report(0, r.ID, noteExhausted)
		return
	}
	v := p.classifyRun(r)
	if v.err != nil {
		p.reportErr(0, r.ID, v.note, v.err)
		return
	}
	if v.note != "" {
		p.report(0, r.ID, v.note)
		return
	}
	kind, note := KindRerun, fmt.Sprintf("re-run %d/%d, pattern %s", r.RunAttempt, Cap, strings.Join(v.ids, ","))
	if !rerunsEnabled {
		kind, note = KindReport, noteDisabled
	}
	a := p.action(kind, 0, r.ID, note)
	a.RunURL, a.PatternID, a.Attempt, a.HeadSHA = r.HTMLURL, strings.Join(v.ids, ","), r.RunAttempt, r.HeadSHA
	p.actions = append(p.actions, a)
}

// verdict on one run: retryable with ids, or a note saying why not. err is
// set when the reason is that something could not be read.
type verdict struct {
	ids  []string
	note string
	err  error
}

type apiJob struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	Conclusion string    `json:"conclusion"`
	RunnerName string    `json:"runner_name"`
	CreatedAt  time.Time `json:"created_at"`
	Steps      []struct {
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	} `json:"steps"`
}

func (j apiJob) anyStepStarted() bool {
	for _, s := range j.Steps {
		if (s.Status == "in_progress" || s.Status == "completed") && s.Conclusion != "skipped" {
			return true
		}
	}
	return false
}

func failedJob(conclusion string) bool {
	switch conclusion {
	case "failure", "timed_out", "cancelled", "startup_failure":
		return true
	}
	return false
}

// classifyRun is retryable only if every failed job matches a pattern and any
// never-started job passes the starvation gate.
func (p *planner) classifyRun(r attention.Run) verdict {
	pats, err := p.patterns()
	if err != nil {
		return verdict{note: "cannot read declared retryable patterns: " + err.Error(), err: err}
	}
	jpath := fmt.Sprintf("%s/actions/runs/%d/jobs?per_page=100", p.base(), r.ID)
	var jobs []apiJob
	err = p.c.GetPaged(p.ctx, jpath, func(raw json.RawMessage) error {
		var j apiJob
		if err := json.Unmarshal(raw, &j); err != nil {
			return err
		}
		if failedJob(j.Conclusion) {
			jobs = append(jobs, j)
		}
		return nil
	})
	if err != nil {
		return verdict{note: readNote(&attention.ReadError{Path: jpath, Err: err}), err: err}
	}
	if len(jobs) == 0 {
		return verdict{note: fmt.Sprintf("run %s with no failed job to classify", r.Conclusion)}
	}
	idset := map[string]bool{}
	var neverStarted []apiJob
	for _, j := range jobs {
		job := Job{Repo: p.repo, Workflow: path.Base(r.Path), Name: j.Name, HeadBranch: r.HeadBranch,
			RunID: r.ID, JobID: j.ID, Attempt: r.RunAttempt, Conclusion: j.Conclusion,
			RunnerName: j.RunnerName, AnyStepStarted: j.anyStepStarted()}
		var log []byte
		// A job that never started has no log to fetch.
		if !(job.Conclusion == "cancelled" && job.RunnerName == "" && !job.AnyStepStarted) {
			var v verdict
			if log, v = p.jobLog(j); v.note != "" {
				return v
			}
		}
		pat, ok := Classify(job, log, pats)
		if !ok {
			return verdict{note: fmt.Sprintf("no retry pattern matches job %q", j.Name)}
		}
		if pat.NeverStarted {
			neverStarted = append(neverStarted, j)
		}
		idset[pat.ID] = true
	}
	if len(neverStarted) > 0 {
		if v := p.starvationGate(r, neverStarted); v.note != "" {
			return v
		}
	}
	return verdict{ids: sortedKeys(idset)}
}

// starvationGate passes a never-started cancel only when the run is the newest
// of its workflow on its SHA and every such job waited at least an hour.
func (p *planner) starvationGate(r attention.Run, jobs []apiJob) verdict {
	path := fmt.Sprintf("%s/actions/workflows/%d/runs?head_sha=%s&per_page=1", p.base(), r.WorkflowID, r.HeadSHA)
	var resp struct {
		Runs []attention.Run `json:"workflow_runs"`
	}
	if err := p.c.Get(p.ctx, path, &resp); err != nil {
		return verdict{note: readNote(&attention.ReadError{Path: path, Err: err}), err: err}
	}
	if len(resp.Runs) == 0 || resp.Runs[0].ID != r.ID {
		return verdict{note: noteDeliberate}
	}
	for _, j := range jobs {
		if j.CreatedAt.IsZero() || r.UpdatedAt.Sub(j.CreatedAt) < neverStartedMinWait {
			return verdict{note: noteDeliberate}
		}
	}
	return verdict{}
}

// jobLog returns the job's log with its failure annotations appended as
// ##[error] lines: GitHub reports a lost runner there, not in the log.
func (p *planner) jobLog(j apiJob) ([]byte, verdict) {
	lpath := fmt.Sprintf("%s/actions/jobs/%d/logs", p.base(), j.ID)
	log, err := p.c.GetRaw(p.ctx, lpath)
	if err != nil {
		var he *ghapi.HTTPError
		if errors.As(err, &he) && (he.Status == 404 || he.Status == 410) {
			return nil, verdict{note: fmt.Sprintf("logs expired (job %q)", j.Name)}
		}
		return nil, verdict{note: fmt.Sprintf("cannot read logs for job %q: %v", j.Name, err), err: err}
	}
	apath := fmt.Sprintf("%s/check-runs/%d/annotations", p.base(), j.ID)
	var extra []string
	err = p.c.GetPaged(p.ctx, apath, func(raw json.RawMessage) error {
		var a struct {
			Level   string `json:"annotation_level"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(raw, &a); err != nil {
			return err
		}
		if a.Level == "failure" {
			msg := strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(a.Message)
			extra = append(extra, "##[error]"+msg)
		}
		return nil
	})
	if err != nil {
		return nil, verdict{note: fmt.Sprintf("cannot read annotations for job %q: %v", j.Name, err), err: err}
	}
	if len(extra) > 0 {
		out := append([]byte{}, log...)
		if len(out) > 0 && out[len(out)-1] != '\n' {
			out = append(out, '\n')
		}
		log = append(out, strings.Join(extra, "\n")+"\n"...)
	}
	return log, verdict{}
}

// prProgress emits a finding for a Renovate PR whose latest fleet marker for
// its current update is newer than the head's last completed run.
func (p *planner) prProgress(pl attention.Pull) {
	item := fmt.Sprintf("%s#%d", p.repo, pl.Number)
	unread := func(err error) {
		p.findings = append(p.findings, model.Finding{Repo: p.repo, Item: item, URL: pl.HTMLURL,
			Kind: model.KindCannotRead, Since: p.now, Why: readNote(err),
			Fix: "check token scope and rate limit, then re-run"})
	}
	comments, err := p.ourComments(pl.Number)
	if err != nil {
		unread(err)
		return
	}
	update := UpdateKey(pl.Title)
	var at time.Time
	var last marker
	for _, c := range comments {
		for _, m := range markers(c.Body) {
			if m.update == update && !c.CreatedAt.Before(at) {
				at, last = c.CreatedAt, m
			}
		}
	}
	if at.IsZero() {
		return
	}
	runs, err := p.runsOnHead(pl.Head.SHA)
	if err != nil {
		unread(err)
		return
	}
	started := false
	for _, r := range runs {
		if r.Status == "completed" && !r.UpdatedAt.Before(at) {
			return // a run finished after the request: no longer in progress
		}
		started = started || r.CreatedAt.After(at)
	}
	f := model.Finding{Repo: p.repo, Item: item, URL: pl.HTMLURL, Since: at}
	if !started && p.now.Sub(at) > time.Hour {
		f.Kind = model.KindStuckPR
		f.Why = fmt.Sprintf("retry requested %s ago, no run started", attention.Age(p.now.Sub(at)))
		f.Fix = "check Renovate picked up the retry"
	} else {
		f.Kind = model.KindRetryInProgress
		f.Why = fmt.Sprintf("retry %d/%d, pattern %s, requested %s ago", last.attempt, Cap, last.pattern, attention.Age(p.now.Sub(at)))
		f.Fix = "wait; fleet flags it if no run starts within an hour"
	}
	p.findings = append(p.findings, f)
}

// Apply carries out one action. It first re-reads everything the action
// depends on and does nothing if any of it changed, so two agents running
// remediate at once cannot exceed the cap. With dryRun it does
// the same reads and writes nothing.
func Apply(ctx context.Context, c ghapi.Client, a Action, dryRun bool) Result {
	switch a.Kind {
	case KindTickBox:
		return applyTick(ctx, c, a, dryRun)
	case KindRerun:
		return applyRerun(ctx, c, a, dryRun)
	}
	return Result{Action: a, Note: a.Note}
}

// declined turns a into a report with the reason.
func declined(a Action, note string, err error) Result {
	a.Kind, a.Note = KindReport, note
	return Result{Action: a, Note: note, Err: err}
}

// recheckRun re-reads the run an action was planned on.
func recheckRun(ctx context.Context, c ghapi.Client, a Action) (attention.Run, string, error) {
	path := fmt.Sprintf("%s/actions/runs/%d", repoBase(a.Org, a.Repo), a.RunID)
	var r attention.Run
	if err := c.Get(ctx, path, &r); err != nil {
		return r, readNote(&attention.ReadError{Path: path, Err: err}), err
	}
	switch {
	case r.Status != "completed":
		return r, noteInProgress, nil
	case !red(r):
		return r, noteNotFailed, nil
	}
	return r, "", nil
}

// recheckHead re-reads the runs on the PR head: nothing may be running, and
// every run the tick was planned for must still be the newest of its kind and
// still red.
func recheckHead(ctx context.Context, c ghapi.Client, base string, a Action) (string, error) {
	runs, err := newestOnHead(ctx, c, base, a.HeadSHA)
	if err != nil {
		return readNote(err), err
	}
	byID := map[int64]attention.Run{}
	for _, r := range runs {
		if r.Status != "completed" {
			return noteInProgress, nil
		}
		byID[r.ID] = r
	}
	ids := a.RunIDs
	if len(ids) == 0 {
		ids = []int64{a.RunID}
	}
	for _, id := range ids {
		r, ok := byID[id]
		if !ok {
			return noteRequested, nil // superseded by a newer run
		}
		if !red(r) {
			return noteNotFailed, nil
		}
	}
	return "", nil
}

func applyTick(ctx context.Context, c ghapi.Client, a Action, dryRun bool) Result {
	if a.Attempt < 1 || a.Attempt > Cap {
		return declined(a, noteExhausted, nil)
	}
	base := repoBase(a.Org, a.Repo)
	login, err := whoami(ctx, c)
	if err != nil {
		return declined(a, err.Error(), err)
	}
	ppath := fmt.Sprintf("%s/pulls/%d", base, a.PR)
	var pl attention.Pull
	if err := c.Get(ctx, ppath, &pl); err != nil {
		return declined(a, readNote(&attention.ReadError{Path: ppath, Err: err}), err)
	}
	switch {
	case pl.State != "open":
		return declined(a, "PR no longer open", nil)
	case !isRenovate(pl):
		return declined(a, "not a same-repo Renovate PR", nil)
	case pl.Head.SHA != a.HeadSHA, UpdateKey(pl.Title) != a.Update:
		return declined(a, noteRequested, nil) // Renovate has already regenerated it
	}
	comments, err := ourComments(ctx, c, base, a.PR, login)
	if err != nil {
		return declined(a, readNote(err), err)
	}
	if CountRetries(bodies(comments), a.Update)+1 != a.Attempt {
		return declined(a, noteRequested, nil)
	}
	switch _, st := TickRebaseBox(pl.Body); st {
	case BoxAbsent:
		return declined(a, noteNoBox, nil)
	case BoxAlreadyTicked:
		return declined(a, noteRequested, nil)
	}
	if note, err := recheckHead(ctx, c, base, a); note != "" {
		return declined(a, note, err)
	}
	if dryRun {
		return Result{Action: a, Note: a.Note}
	}
	// Comment first: if the tick then fails, the attempt is already counted,
	// so a later run cannot make it a third.
	cpath := fmt.Sprintf("%s/issues/%d/comments", base, a.PR)
	body := Comment(a.Attempt, Cap, a.PatternID, a.RunURL, a.Update)
	if err := c.Post(ctx, cpath, map[string]string{"body": body}, nil); err != nil {
		return Result{Action: a, Err: err, Note: "retry comment failed; nothing ticked"}
	}
	// The body read above may be minutes old by now (paging, backoff, the
	// POST): tick the body as it is now, so nothing but the box changes.
	tickFailed := fmt.Sprintf("comment posted but tick failed (attempt %d/%d used)", a.Attempt, Cap)
	var fresh attention.Pull
	if err := c.Get(ctx, ppath, &fresh); err != nil {
		return Result{Action: a, Err: err, Note: tickFailed}
	}
	ticked, st := TickRebaseBox(fresh.Body)
	if fresh.State != "open" || fresh.Head.SHA != a.HeadSHA || UpdateKey(fresh.Title) != a.Update || st != BoxTicked {
		return declined(a, fmt.Sprintf("already requested (comment posted, attempt %d used)", a.Attempt), nil)
	}
	if err := c.Patch(ctx, ppath, map[string]string{"body": ticked}, nil); err != nil {
		return Result{Action: a, Err: err, Note: tickFailed}
	}
	return Result{Action: a, Done: true, Note: a.Note}
}

func applyRerun(ctx context.Context, c ghapi.Client, a Action, dryRun bool) Result {
	if !rerunsEnabled {
		return declined(a, noteDisabled, nil)
	}
	if a.Attempt < 1 || a.Attempt > Cap {
		return declined(a, noteExhausted, nil)
	}
	r, note, err := recheckRun(ctx, c, a)
	switch {
	case note != "":
		return declined(a, note, err)
	case strings.HasPrefix(r.HeadBranch, "renovate/"):
		return declined(a, "renovate branch: retried by its PR's checkbox, never re-run", nil)
	case r.RunAttempt != a.Attempt:
		return declined(a, noteRequested, nil)
	}
	if dryRun {
		return Result{Action: a, Note: a.Note}
	}
	path := fmt.Sprintf("%s/actions/runs/%d/rerun-failed-jobs", repoBase(a.Org, a.Repo), a.RunID)
	if err := c.Post(ctx, path, nil, nil); err != nil {
		var he *ghapi.HTTPError
		if errors.As(err, &he) && he.Status == 403 {
			return declined(a, noteCutRelease, nil)
		}
		return Result{Action: a, Err: err, Note: "re-run request failed"}
	}
	return Result{Action: a, Done: true, Note: a.Note}
}

// Attention is the attention.Retry backed by remediate's planning. It plans
// each repo once, on first use, and is safe for concurrent use.
type Attention struct {
	c            ghapi.Client
	org, scripts string
	now          time.Time

	mu       sync.Mutex
	login    string
	loginErr error
	haveUser bool
	repos    map[string]*attnRepo
}

type attnRepo struct {
	once sync.Once
	p    *planner
}

func NewAttention(c ghapi.Client, org, scripts string, now time.Time) *Attention {
	return &Attention{c: c, org: org, scripts: scripts, now: now, repos: map[string]*attnRepo{}}
}

func (a *Attention) plan(ctx context.Context, repo string) (*planner, error) {
	a.mu.Lock()
	if !a.haveUser {
		a.login, a.loginErr = whoami(ctx, a.c)
		a.haveUser = true
	}
	login, lerr := a.login, a.loginErr
	e := a.repos[repo]
	if e == nil {
		e = &attnRepo{}
		a.repos[repo] = e
	}
	a.mu.Unlock()
	if lerr != nil {
		return nil, lerr
	}
	e.once.Do(func() { e.p = planRepo(ctx, a.c, a.org, repo, a.scripts, login, a.now, true) })
	return e.p, nil
}

// Retryable is true when remediate would tick or re-run for this PR's head.
// A verdict that could not be read is an error, never a plain false.
func (a *Attention) Retryable(ctx context.Context, pr attention.PR) (bool, error) {
	p, err := a.plan(ctx, pr.Repo)
	if err != nil {
		return false, err
	}
	if err := p.unreadable[pr.Number]; err != nil {
		return false, err
	}
	return p.retryable[pr.Number], nil
}

// InProgress lists retries fleet has requested that have not yet finished.
func (a *Attention) InProgress(ctx context.Context, repo string) ([]model.Finding, error) {
	p, err := a.plan(ctx, repo)
	if err != nil {
		return nil, err
	}
	return append([]model.Finding{}, p.findings...), nil
}
