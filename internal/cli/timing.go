package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Mavergreen/fleet/internal/attention"
	"github.com/Mavergreen/fleet/internal/ghapi"
	"github.com/Mavergreen/fleet/internal/render"
	"github.com/Mavergreen/fleet/internal/timing"
)

const (
	timingArgs  = "timing [--since YYYY-MM-DD|7d|48h] [--force] [repo [run-id]]"
	timingUsage = usagePrefix + timingArgs
)

// quotaReserve is how much of the shared hourly quota timing leaves for
// everything else (other agents, remediate) unless --force.
const quotaReserve = 500

// errQuota stops a fleet timing that would use most of the shared quota.
type errQuota struct{ need, left int }

func (e errQuota) Error() string {
	return fmt.Sprintf("timing would use ~%d requests; only %d left this hour (shared quota). Use --since to narrow, or wait.", e.need, e.left)
}

// parseSince reads a date (YYYY-MM-DD, UTC) or a duration back from now
// (7d, 48h).
func parseSince(s string, now time.Time) (time.Time, error) {
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	if n, ok := strings.CutSuffix(s, "d"); ok {
		if days, err := strconv.Atoi(n); err == nil && days > 0 {
			return now.Add(-time.Duration(days) * 24 * time.Hour), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return now.Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("bad --since %q: want YYYY-MM-DD or a duration like 7d or 48h", s)
}

func runTiming(ctx context.Context, e Env, args []string, asJSON bool) int {
	now := e.Now()
	sinceArg := ""
	force := false
	var pos []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--since" && i+1 < len(args):
			i++
			sinceArg = args[i]
		case strings.HasPrefix(a, "--since="):
			sinceArg = strings.TrimPrefix(a, "--since=")
		case a == "--force":
			force = true
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(e.Stderr, "mavergreen-fleet: timing: unknown argument %q\n%s\n", a, timingUsage)
			return 2
		default:
			pos = append(pos, a)
		}
	}
	since := now.Add(-7 * 24 * time.Hour)
	if sinceArg != "" {
		var err error
		if since, err = parseSince(sinceArg, now); err != nil {
			fmt.Fprintf(e.Stderr, "mavergreen-fleet: timing: %v\n%s\n", err, timingUsage)
			return 2
		}
	}
	var runID int64
	if len(pos) > 2 {
		fmt.Fprintf(e.Stderr, "mavergreen-fleet: timing: too many arguments\n%s\n", timingUsage)
		return 2
	}
	if len(pos) == 2 {
		var err error
		if runID, err = strconv.ParseInt(pos[1], 10, 64); err != nil || runID <= 0 {
			fmt.Fprintf(e.Stderr, "mavergreen-fleet: timing: bad run id %q\n%s\n", pos[1], timingUsage)
			return 2
		}
	}
	c, err := connect(e)
	if err != nil {
		fmt.Fprintf(e.Stderr, "mavergreen-fleet: %v\n", err)
		return 1
	}
	cache, cerr := timing.DefaultCache()
	if cerr != nil {
		fmt.Fprintf(e.Stderr, "mavergreen-fleet: warning: no timing cache: %v\n", cerr)
		cache = nil
	} else {
		cache.Warn = e.Stderr
	}
	if len(pos) == 0 {
		return timingFleet(ctx, e, c, since, cache, force, asJSON, now)
	}
	return timingRun(ctx, e, c, pos[0], runID, since, cache, asJSON, now)
}

func timingFleet(ctx context.Context, e Env, c ghapi.Client, since time.Time, cache *timing.Cache, force, asJSON bool, now time.Time) int {
	repos, err := attention.Repos(ctx, c, org)
	if err != nil {
		fmt.Fprintf(e.Stderr, "mavergreen-fleet: %v\n", err)
		return 1
	}
	// A cold cache costs a request per run: check the shared quota first.
	budget := func(need int) error {
		if left := ghapi.Remaining(c); !force && left >= 0 && need > left-quotaReserve {
			return errQuota{need, left}
		}
		return nil
	}
	runs, jobs, ferr := timing.FetchBudgeted(ctx, c, org, repos, since, cache, budget)
	var eq errQuota
	if errors.As(ferr, &eq) {
		fmt.Fprintf(e.Stderr, "mavergreen-fleet: %v\n", eq)
		return 1
	}
	unreadable := map[string]int{}
	for _, r := range runs {
		if r.JobsUnknown {
			unreadable[r.Repo]++
		}
	}
	stats := timing.Summarize(runs, jobs)
	var problems []string
	if ferr != nil {
		for _, l := range strings.Split(ferr.Error(), "\n") {
			if l != "" {
				problems = append(problems, l)
			}
		}
	}
	if asJSON {
		if stats == nil {
			stats = []timing.RepoStats{}
		}
		if problems == nil {
			problems = []string{}
		}
		err := render.JSON(e.Stdout, "timing", struct {
			WindowStart   string             `json:"window_start"`
			Repos         []timing.RepoStats `json:"repos"`
			UnreadableRun map[string]int     `json:"unreadable_runs"`
			CannotRead    []string           `json:"cannot_read"`
		}{since.UTC().Format("2006-01-02"), stats, unreadable, problems}, now)
		if err != nil {
			fmt.Fprintf(e.Stderr, "mavergreen-fleet: %v\n", err)
			return 1
		}
		return 0
	}
	if len(stats) == 0 {
		fmt.Fprintf(e.Stdout, "no jobs since %s in %d repos checked\n", since.UTC().Format("2006-01-02"), len(repos))
	} else {
		width, fit := textWidth(e)
		if !fit {
			width = 0 // no limit
		}
		io.WriteString(e.Stdout, timing.Table(stats, width, e.IsTTY))
	}
	names := make([]string, 0, len(unreadable))
	for r := range unreadable {
		names = append(names, r)
	}
	sort.Strings(names)
	for _, r := range names {
		fmt.Fprintf(e.Stdout, "cannot read jobs for %d runs in %s\n", unreadable[r], r)
	}
	for _, p := range problems {
		fmt.Fprintf(e.Stdout, "cannot read: %s\n", p)
	}
	if ferr != nil && len(runs) == 0 {
		return 1 // nothing rendered
	}
	return 0
}

func timingRun(ctx context.Context, e Env, c ghapi.Client, repo string, runID int64, since time.Time, cache *timing.Cache, asJSON bool, now time.Time) int {
	fail := func(err error) int {
		fmt.Fprintf(e.Stderr, "mavergreen-fleet: %v\n", err)
		return 1
	}
	var run timing.Run
	var jobs []timing.Job
	note := ""
	if runID == 0 {
		r, ok, err := timing.LatestRun(ctx, c, org, repo, "release.yml", since)
		if err != nil {
			return fail(err)
		}
		if !ok {
			note = fmt.Sprintf("no release.yml run in %s since %s; showing its latest run of any workflow", repo, since.UTC().Format("2006-01-02"))
			if r, ok, err = timing.LatestRun(ctx, c, org, repo, "", since); err != nil {
				return fail(err)
			}
		}
		if !ok {
			// Nothing to show is an answer, not an error: exit 0 either way.
			none := fmt.Sprintf("no runs in %s since %s", repo, since.UTC().Format("2006-01-02"))
			if asJSON {
				if err := render.JSON(e.Stdout, "timing", struct {
					Repo         string   `json:"repo"`
					Note         string   `json:"note"`
					Jobs         []any    `json:"jobs"`
					CriticalPath []string `json:"critical_path"`
				}{repo, none, []any{}, []string{}}, now); err != nil {
					return fail(err)
				}
				return 0
			}
			fmt.Fprintln(e.Stdout, none)
			return 0
		}
		runID = r.ID
	}
	run, jobs, err := timing.FetchRun(ctx, c, org, repo, runID, cache)
	if err != nil {
		return fail(err)
	}
	crit := timing.CriticalPath(jobs)
	if asJSON {
		type jobRow struct {
			Name      string    `json:"name"`
			Attempt   int       `json:"attempt"`
			Labels    []string  `json:"labels"`
			Created   time.Time `json:"created"`
			Started   time.Time `json:"started"`
			Completed time.Time `json:"completed"`
			WaitS     float64   `json:"wait_s"`
			RunS      float64   `json:"run_s"`
			Never     bool      `json:"never_started"`
			Result    string    `json:"conclusion"`
		}
		rows := []jobRow{}
		for _, j := range jobs {
			rows = append(rows, jobRow{j.Name, j.Attempt, append([]string{}, j.Labels...), j.Created, j.Started, j.Completed,
				timing.QueueWait(j).Seconds(), timing.RunTime(j).Seconds(), timing.NeverStarted(j), j.Conclusion})
		}
		if crit == nil {
			crit = []string{}
		}
		err := render.JSON(e.Stdout, "timing", struct {
			Repo         string   `json:"repo"`
			RunID        int64    `json:"run_id"`
			Note         string   `json:"note,omitempty"`
			Jobs         []jobRow `json:"jobs"`
			CriticalPath []string `json:"critical_path"`
		}{repo, runID, note, rows, crit}, now)
		if err != nil {
			return fail(err)
		}
		return 0
	}
	if note != "" {
		fmt.Fprintln(e.Stdout, note)
	}
	width, fit := textWidth(e)
	head := fmt.Sprintf("%s %s run %d (%s, attempt %d) created %s", repo, run.Workflow, run.ID,
		orDash(run.Conclusion), run.Attempt, run.Created.UTC().Format("2006-01-02 15:04Z"))
	lines := timing.GanttFull(jobs, width)
	if fit {
		head = timing.Truncate(head, width)
		lines = timing.Gantt(jobs, width)
	}
	fmt.Fprintln(e.Stdout, head)
	for _, l := range lines {
		fmt.Fprintln(e.Stdout, l)
	}
	return 0
}

func orDash(s string) string {
	if s == "" {
		return "in progress"
	}
	return s
}
