// Package retry decides whether a failed CI job failed for a transient,
// infrastructure reason. Only a positive match is retryable; anything else is
// left for a human.
package retry

import "regexp"

// Pattern recognises one kind of transient failure.
type Pattern struct {
	ID       string
	Evidence string // one sentence: where the evidence for this pattern came from
	// NeverStarted matches a job GitHub cancelled before any runner took it;
	// such a job has no log, so LogRE is ignored.
	NeverStarted bool
	// FinalError requires the matched line to be the job's final error: no
	// other ##[error] line and no more than finalTailLines lines may follow it
	// before the closing ##[error] line or the end of the log. Otherwise a
	// tolerated message could hide a later, real failure.
	FinalError bool
	LogRE      *regexp.Regexp
	// Workflow and Job are set only on declared patterns. They scope the
	// pattern to the job with exactly this workflow file name and job name.
	Workflow, Job string
}

// Builtin returns the Actions-wide patterns, which apply to every repo.
func Builtin() []Pattern {
	return []Pattern{
		{
			ID:           "never-started",
			Evidence:     "GitHub cancels jobs it never assigned a runner to; the job reports cancelled with no runner name and no started step.",
			NeverStarted: true,
		},
		{
			ID:         "runner-lost",
			Evidence:   "the exact runner-infrastructure error messages in GitHub's Actions documentation: the runner received a shutdown signal, or the hosted runner lost communication with the server.",
			FinalError: true,
			LogRE:      regexp.MustCompile(`(?m)##\[error\](?:The runner has received a shutdown signal\.|The hosted runner lost communication with the server\.|The hosted runner: .+ encountered an error while running your job\.)`),
		},
		{
			ID:         "network-fetch",
			Evidence:   "shipyard_curl (mavericks_fetch.sh) prints this line only after exhausting its retries on a transport-level curl exit or an HTTP 429/500/502/503/504, as seen in Mavergreen/rust run 42571198930 (the give-up wording and its HTTP 429/50x forms come from mavericks_fetch.sh).",
			FinalError: true,
			LogRE:      regexp.MustCompile(`(?m)shipyard_curl: \S+: (?:curl exit (?:6|7|18|28|35|52|55|56)|HTTP (?:429|50[0234])); giving up after \d+ tries`),
		},
	}
}
