// Package timing fetches Actions runs and jobs and caches completed runs.
package timing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"sync"
	"time"

	"github.com/Mavergreen/fleet/internal/ghapi"
)

// listingCap is GitHub's limit on the runs listing for one query.
const listingCap = 1000

type Run struct {
	Repo, Workflow, HeadBranch, Event, Actor, Conclusion string
	ID                                                   int64
	Attempt                                              int
	Created                                              time.Time
	// JobsUnknown is set when the run's jobs could not be fetched.
	JobsUnknown bool
}

type Job struct {
	Repo, Workflow, Name        string
	RunID                       int64
	Attempt                     int
	Labels                      []string
	Created, Started, Completed time.Time
	RunnerName                  string
	AnyStepStarted              bool
	Conclusion                  string
}

type apiRun struct {
	ID         int64     `json:"id"`
	Path       string    `json:"path"`
	HeadBranch string    `json:"head_branch"`
	Event      string    `json:"event"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	Attempt    int       `json:"run_attempt"`
	Created    time.Time `json:"created_at"`
	Actor      struct {
		Login string `json:"login"`
	} `json:"actor"`
}

type apiJob struct {
	Name       string    `json:"name"`
	Attempt    int       `json:"run_attempt"`
	Labels     []string  `json:"labels"`
	Created    time.Time `json:"created_at"`
	Started    time.Time `json:"started_at"`
	Completed  time.Time `json:"completed_at"`
	RunnerName string    `json:"runner_name"`
	Conclusion string    `json:"conclusion"`
	Steps      []struct {
		Started *time.Time `json:"started_at"`
	} `json:"steps"`
}

type repoResult struct {
	runs []Run
	jobs []Job
	err  error
}

// Fetch returns runs created on or after since and all their jobs (every
// attempt). Repos are fetched concurrently. A repo that fails contributes
// nothing and its error (naming the repo) is joined into the returned error;
// everything that worked is still returned. cache may be nil. Warnings go to
// cache.Warn when set.
func Fetch(ctx context.Context, c ghapi.Client, org string, repos []string, since time.Time, cache *Cache) ([]Run, []Job, error) {
	results := make([]repoResult, len(repos))
	var wg sync.WaitGroup
	cw := &cacheWarner{cache: cache}
	for i, repo := range repos {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = fetchRepo(ctx, c, org, repo, since, cache, cw)
		}()
	}
	wg.Wait()

	var runs []Run
	var jobs []Job
	var errs []error
	for _, r := range results {
		runs = append(runs, r.runs...)
		jobs = append(jobs, r.jobs...)
		if r.err != nil {
			errs = append(errs, r.err)
		}
	}
	sort.SliceStable(runs, func(i, j int) bool {
		if runs[i].Repo != runs[j].Repo {
			return runs[i].Repo < runs[j].Repo
		}
		return runs[i].ID < runs[j].ID
	})
	sort.SliceStable(jobs, func(i, j int) bool {
		a, b := jobs[i], jobs[j]
		if a.Repo != b.Repo {
			return a.Repo < b.Repo
		}
		if a.RunID != b.RunID {
			return a.RunID < b.RunID
		}
		return a.Started.Before(b.Started)
	})
	return runs, jobs, errors.Join(errs...)
}

// cacheWarner emits at most one cache-write warning per Fetch.
type cacheWarner struct {
	cache *Cache
	once  sync.Once
}

func (w *cacheWarner) warn(repo string, err error) {
	if w.cache == nil || w.cache.Warn == nil {
		return
	}
	w.once.Do(func() {
		fmt.Fprintf(w.cache.Warn, "warning: cannot write timing cache for %s: %v (continuing without caching)\n", repo, err)
	})
}

func fetchRepo(ctx context.Context, c ghapi.Client, org, repo string, since time.Time, cache *Cache, cw *cacheWarner) repoResult {
	var res repoResult
	var raws []apiRun
	urlPath := fmt.Sprintf("/repos/%s/%s/actions/runs?created=>=%s&per_page=100", org, repo, since.UTC().Format("2006-01-02"))
	err := c.GetPaged(ctx, urlPath, func(m json.RawMessage) error {
		var r apiRun
		if err := json.Unmarshal(m, &r); err != nil {
			return err
		}
		raws = append(raws, r)
		return nil
	})
	if err != nil {
		res.err = fmt.Errorf("%s: listing runs: %w", repo, err)
		return res
	}
	if len(raws) >= listingCap && cache != nil && cache.Warn != nil {
		fmt.Fprintf(cache.Warn, "warning: %s: runs listing reached GitHub's %d-result cap; older runs in the window may be missing\n", repo, listingCap)
	}

	type jr struct {
		jobs []Job
		err  error
	}
	jrs := make([]jr, len(raws))
	var wg sync.WaitGroup
	for i, r := range raws {
		run := Run{Repo: repo, Workflow: workflowName(r.Path), HeadBranch: r.HeadBranch, Event: r.Event,
			Actor: r.Actor.Login, Conclusion: r.Conclusion, ID: r.ID, Attempt: r.Attempt, Created: r.Created}
		res.runs = append(res.runs, run)
		completed := r.Status == "completed"
		if completed {
			if jobs, ok := cache.Get(repo, r.ID, r.Attempt); ok {
				jrs[i].jobs = jobs
				continue
			}
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			jobs, err := fetchJobs(ctx, c, org, run)
			if err != nil {
				jrs[i].err = fmt.Errorf("%s: jobs for run %d: %w", repo, run.ID, err)
				return
			}
			jrs[i].jobs = jobs
			if completed && cache != nil {
				if err := cache.Put(run, jobs); err != nil {
					cw.warn(repo, err)
				}
			}
		}()
	}
	wg.Wait()
	var errs []error
	for i, j := range jrs {
		if j.err != nil {
			res.runs[i].JobsUnknown = true
		}
		res.jobs = append(res.jobs, j.jobs...)
		if j.err != nil {
			errs = append(errs, j.err)
		}
	}
	res.err = errors.Join(errs...)
	if cache != nil && cache.Dir != "" {
		if err := cache.Prune(repo, time.Now()); err != nil {
			cw.warn(repo, err)
		}
	}
	return res
}

func fetchJobs(ctx context.Context, c ghapi.Client, org string, run Run) ([]Job, error) {
	var jobs []Job
	urlPath := fmt.Sprintf("/repos/%s/%s/actions/runs/%d/jobs?filter=all", org, run.Repo, run.ID)
	err := c.GetPaged(ctx, urlPath, func(m json.RawMessage) error {
		var j apiJob
		if err := json.Unmarshal(m, &j); err != nil {
			return err
		}
		job := Job{Repo: run.Repo, Workflow: run.Workflow, Name: j.Name, RunID: run.ID, Attempt: j.Attempt,
			Labels: j.Labels, Created: j.Created, Started: j.Started, Completed: j.Completed,
			RunnerName: j.RunnerName, Conclusion: j.Conclusion}
		for _, s := range j.Steps {
			if s.Started != nil && !s.Started.IsZero() {
				job.AnyStepStarted = true
			}
		}
		jobs = append(jobs, job)
		return nil
	})
	return jobs, err
}

func workflowName(p string) string {
	if p == "" {
		return ""
	}
	return path.Base(p)
}
