package retry

import (
	"bytes"
	"strings"
)

// finalTailLines bounds how many lines may follow a FinalError match.
const finalTailLines = 5

// Job is the facts about one CI job that classification needs.
type Job struct {
	Repo, Workflow, Name, HeadBranch string
	RunID, JobID                     int64
	Attempt                          int
	Conclusion                       string
	RunnerName                       string
	AnyStepStarted                   bool
}

// Classify returns the first pattern, in the given order, that positively
// recognises the job's failure as transient. A missing log never matches a
// log pattern.
func Classify(j Job, log []byte, patterns []Pattern) (Pattern, bool) {
	for _, p := range patterns {
		if p.Workflow != "" || p.Job != "" {
			if p.Workflow != j.Workflow || p.Job != j.Name {
				continue
			}
		}
		if p.NeverStarted {
			if j.Conclusion == "cancelled" && j.RunnerName == "" && !j.AnyStepStarted {
				return p, true
			}
			continue
		}
		if p.LogRE != nil && len(log) > 0 && matchesLog(p, log) {
			return p, true
		}
	}
	return Pattern{}, false
}

func matchesLog(p Pattern, log []byte) bool {
	if p.LogRE == nil || len(log) == 0 {
		return false
	}
	if !p.FinalError {
		return p.LogRE.Match(log)
	}
	locs := p.LogRE.FindAllIndex(log, -1)
	if len(locs) == 0 {
		return false
	}
	rest := log[locs[len(locs)-1][1]:]
	if i := bytes.IndexByte(rest, '\n'); i >= 0 {
		rest = rest[i+1:]
	} else {
		return true
	}
	n, closed := 0, false
	for _, line := range strings.Split(string(rest), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.Contains(line, "##[error]") {
			if !isClosingError(line) {
				return false
			}
			closed = true
			continue
		}
		if closed {
			continue
		}
		if n++; n > finalTailLines {
			return false
		}
	}
	return true
}

// isClosingError reports whether an ##[error] line only closes the job.
func isClosingError(line string) bool {
	return strings.Contains(line, "##[error]Process completed with exit code") ||
		strings.Contains(line, "##[error]The operation was canceled.")
}
