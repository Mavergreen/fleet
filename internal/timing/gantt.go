package timing

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// activeJobs are the jobs worth drawing, in start order.
func activeJobs(jobs []Job) []Job {
	var out []Job
	for _, j := range jobs {
		if !Skipped(j) {
			out = append(out, j)
		}
	}
	sort.SliceStable(out, func(a, b int) bool {
		x, y := out[a], out[b]
		if !x.Created.Equal(y.Created) {
			return x.Created.Before(y.Created)
		}
		if !x.Started.Equal(y.Started) {
			return x.Started.Before(y.Started)
		}
		return x.Name < y.Name
	})
	return out
}

// jobEnd is when the job stopped (or its last known moment).
func jobEnd(j Job) time.Time {
	switch {
	case !j.Completed.IsZero():
		return j.Completed
	case !j.Started.IsZero():
		return j.Started
	}
	return j.Created
}

// criticalIdx returns indexes into jobs: start from the job that completed
// last, then repeatedly the job that completed latest at or before the
// current job's Created. Earliest first.
func criticalIdx(jobs []Job) []int {
	last := -1
	for i, j := range jobs {
		if j.Completed.IsZero() {
			continue
		}
		if last < 0 || j.Completed.After(jobs[last].Completed) {
			last = i
		}
	}
	if last < 0 {
		return nil
	}
	seen := map[int]bool{last: true}
	path := []int{last}
	for cur := last; ; {
		next := -1
		for i, j := range jobs {
			if seen[i] || j.Completed.IsZero() || j.Completed.After(jobs[cur].Created) {
				continue
			}
			if next < 0 || j.Completed.After(jobs[next].Completed) {
				next = i
			}
		}
		if next < 0 {
			break
		}
		seen[next] = true
		path = append(path, next)
		cur = next
	}
	for a, b := 0, len(path)-1; a < b; a, b = a+1, b-1 {
		path[a], path[b] = path[b], path[a]
	}
	return path
}

// CriticalPath names the jobs on the critical path, earliest first.
func CriticalPath(jobs []Job) []string {
	jobs = activeJobs(jobs)
	var names []string
	for _, i := range criticalIdx(jobs) {
		names = append(names, jobs[i].Name)
	}
	return names
}

// Gantt draws one line per job on a shared time axis: ░ for queue wait, █
// for run, * before the name on the critical path, then a footer with the
// axis duration. Lines are at most width runes (width <= 0 means 80): long
// names are truncated.
func Gantt(jobs []Job, width int) []string { return gantt(jobs, width, true) }

// GanttFull is Gantt for output that is not a terminal: names are never
// truncated, and width only sizes the bars, so lines may be longer.
func GanttFull(jobs []Job, width int) []string { return gantt(jobs, width, false) }

func gantt(jobs []Job, width int, fit bool) []string {
	if width <= 0 {
		width = 80
	}
	jobs = activeJobs(jobs)
	if len(jobs) == 0 {
		return []string{"no jobs"}
	}
	start, end := jobs[0].Created, jobEnd(jobs[0])
	for _, j := range jobs {
		if j.Created.Before(start) {
			start = j.Created
		}
		if e := jobEnd(j); e.After(end) {
			end = e
		}
	}
	span := end.Sub(start)
	crit := map[int]bool{}
	for _, i := range criticalIdx(jobs) {
		crit[i] = true
	}

	names := make([]string, len(jobs))
	longest := 0
	for i, j := range jobs {
		names[i] = j.Name
		if j.Attempt > 1 {
			names[i] += fmt.Sprintf(" (attempt %d)", j.Attempt)
		}
		if n := runeLen(names[i]); n > longest {
			longest = n
		}
	}
	const minBar = 20
	nameW := longest
	if max := width - 2 - minBar; fit && nameW > max {
		nameW = max
	}
	if nameW < 4 {
		nameW = 4
	}
	barW := width - 2 - nameW
	if !fit && barW < minBar {
		barW = minBar
	}
	if barW < 1 {
		barW = 1
	}

	// cell maps t onto [0, barW]: clock skew (a job started before it was
	// created, or ended before it started) must not index off the bar.
	cell := func(t time.Time) int {
		if span <= 0 {
			return 0
		}
		return clamp(int(float64(t.Sub(start))/float64(span)*float64(barW)), 0, barW)
	}
	var lines []string
	for i, j := range jobs {
		bar := make([]rune, barW)
		for k := range bar {
			bar[k] = ' '
		}
		// Never-started jobs are all wait, until cancellation.
		s, e := j.Started, jobEnd(j)
		if NeverStarted(j) || s.IsZero() {
			s = e
		}
		a, b, c := cell(j.Created), cell(s), cell(e)
		if b < a {
			b = a
		}
		if s.After(j.Created) && b <= a && a < barW {
			b = a + 1
		}
		if c < b {
			c = b
		}
		if e.After(s) && c <= b {
			c = b + 1
		}
		a, b, c = clamp(a, 0, barW), clamp(b, 0, barW), clamp(c, 0, barW)
		for k := a; k < b; k++ {
			bar[k] = '░'
		}
		for k := b; k < c; k++ {
			bar[k] = '█'
		}
		mark := " "
		if crit[i] {
			mark = "*"
		}
		name := names[i]
		if fit {
			name = truncate(name, nameW)
		}
		name += strings.Repeat(" ", nameW-runeLen(name))
		lines = append(lines, strings.TrimRight(mark+name+" "+string(bar), " "))
	}
	footer := fmt.Sprintf("axis %s  ░ wait  █ run  * critical path", span.Round(time.Second))
	if fit {
		footer = truncate(footer, width)
	}
	return append(lines, footer)
}

func clamp(n, lo, hi int) int {
	switch {
	case n < lo:
		return lo
	case n > hi:
		return hi
	}
	return n
}
