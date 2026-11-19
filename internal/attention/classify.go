// Package attention classifies PRs and issue threads into findings. Pure functions, no I/O.
package attention

import (
	"fmt"
	"time"

	"github.com/Mavergreen/fleet/internal/model"
)

const (
	stuckAfter = 24 * time.Hour
	retryAfter = time.Hour
)

type CheckState struct {
	State string // "green" | "red" | "pending"
	Since time.Time
}

type Status struct{ State, Description string }

type PR struct {
	Repo             string
	Number           int
	Author           string
	HeadRef          string
	URL              string
	Body             string
	CreatedAt        time.Time
	Checks           CheckState
	Mergeable        *bool
	Settled          *Status
	RetryRequestedAt *time.Time
	RunStartedSince  bool
}

type Thread struct {
	Repo          string
	Number        int
	URL           string
	Author        string
	AuthorIsBot   bool
	Title         string
	LastCommenter string
	LastActivity  time.Time
	IsPR          bool
}

// Age formats a duration the way findings do.
func Age(d time.Duration) string { return age(d) }

// age: <n>d at >= 48h, else <n>h, else <n>m.
func age(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
}

func item(repo string, n int) string { return fmt.Sprintf("%s#%d", repo, n) }

// ClassifyPR reports at most one finding per PR.
func ClassifyPR(pr PR, retryable bool, now time.Time) (model.Finding, bool) {
	f := model.Finding{Repo: pr.Repo, Item: item(pr.Repo, pr.Number), URL: pr.URL, Kind: model.KindStuckPR}

	if pr.RetryRequestedAt != nil && !pr.RunStartedSince && now.Sub(*pr.RetryRequestedAt) > retryAfter {
		f.Since = *pr.RetryRequestedAt
		f.Why = fmt.Sprintf("retry requested %s ago, no run started", age(now.Sub(f.Since)))
		f.Fix = "check Renovate picked up the retry"
		return f, true
	}

	switch pr.Checks.State {
	case "red":
		d := now.Sub(pr.Checks.Since)
		if d > stuckAfter && !retryable {
			f.Since = pr.Checks.Since
			f.Why = "red " + age(d) + ", not retryable"
			f.Fix = "diagnose the failure; declare a retryable deviation if it is a known flake"
			return f, true
		}
	case "green":
		d := now.Sub(pr.Checks.Since)
		if d > stuckAfter {
			f.Since = pr.Checks.Since
			f.Why = "green, unmerged " + age(d)
			if pr.Mergeable != nil && !*pr.Mergeable {
				f.Why += "; merge conflict"
			}
			if pr.Settled != nil && pr.Settled.State == "pending" {
				f.Why += "; ingredients-settled pending: " + pr.Settled.Description
			}
			f.Fix = "check auto-merge and branch protection (audit-repo-settings.sh)"
			return f, true
		}
	}
	return model.Finding{}, false
}

// NeedsReply reports an open human thread whose last word was not the maintainer's.
func NeedsReply(t Thread, maintainer string) (model.Finding, bool) {
	if t.AuthorIsBot || t.Title == "Dependency Dashboard" || t.LastCommenter == maintainer {
		return model.Finding{}, false
	}
	return model.Finding{
		Repo: t.Repo, Item: item(t.Repo, t.Number), URL: t.URL,
		Kind: model.KindNeedsReply, Since: t.LastActivity,
		Why: fmt.Sprintf("%s spoke last; no reply from %s", t.LastCommenter, maintainer),
		Fix: "reply (fleet never posts for you)",
	}, true
}
