package timing

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/Mavergreen/fleet/internal/ghapi"
)

func toRun(repo string, r apiRun) Run {
	return Run{Repo: repo, Workflow: workflowName(r.Path), HeadBranch: r.HeadBranch, Event: r.Event,
		Actor: r.Actor.Login, Conclusion: r.Conclusion, ID: r.ID, Attempt: r.Attempt, Created: r.Created}
}

// FetchRun returns one run and its jobs from every attempt, using the cache
// for a completed run.
func FetchRun(ctx context.Context, c ghapi.Client, org, repo string, runID int64, cache *Cache) (Run, []Job, error) {
	var r apiRun
	if err := c.Get(ctx, fmt.Sprintf("/repos/%s/%s/actions/runs/%d", org, repo, runID), &r); err != nil {
		return Run{}, nil, fmt.Errorf("%s: run %d: %w", repo, runID, err)
	}
	run := toRun(repo, r)
	completed := r.Status == "completed"
	if completed {
		if jobs, ok := cache.Get(repo, runID, r.Attempt); ok {
			return run, jobs, nil
		}
	}
	jobs, err := fetchJobs(ctx, c, org, run)
	if err != nil {
		return run, nil, fmt.Errorf("%s: jobs for run %d: %w", repo, runID, err)
	}
	if completed && cache != nil {
		if err := cache.Put(run, jobs); err != nil && cache.Warn != nil {
			fmt.Fprintf(cache.Warn, "warning: cannot write timing cache for %s: %v (continuing without caching)\n", repo, err)
		}
	}
	return run, jobs, nil
}

// LatestRun returns the repo's most recent run created on or after since; of
// the given workflow file (e.g. "release.yml") when workflow is not empty.
// ok is false when there is none (or no such workflow).
func LatestRun(ctx context.Context, c ghapi.Client, org, repo, workflow string, since time.Time) (Run, bool, error) {
	p := fmt.Sprintf("/repos/%s/%s/actions/runs", org, repo)
	if workflow != "" {
		p = fmt.Sprintf("/repos/%s/%s/actions/workflows/%s/runs", org, repo, url.PathEscape(workflow))
	}
	p += "?per_page=1&created=>=" + since.UTC().Format("2006-01-02")
	var out struct {
		Runs []apiRun `json:"workflow_runs"`
	}
	if err := c.Get(ctx, p, &out); err != nil {
		var he *ghapi.HTTPError
		if workflow != "" && errors.As(err, &he) && he.Status == 404 {
			return Run{}, false, nil
		}
		return Run{}, false, fmt.Errorf("%s: latest run: %w", repo, err)
	}
	if len(out.Runs) == 0 {
		return Run{}, false, nil
	}
	return toRun(repo, out.Runs[0]), true, nil
}
