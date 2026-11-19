package attention

import (
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

func ago(d time.Duration) time.Time { return now.Add(-d) }

func pr(state string, since time.Duration) PR {
	return PR{Repo: "r", Number: 7, URL: "u", Checks: CheckState{State: state, Since: ago(since)}}
}

func TestRedOver24hNotRetryableIsStuck(t *testing.T) {
	f, ok := ClassifyPR(pr("red", 25*time.Hour), false, now)
	if !ok || !strings.Contains(f.Why, "red 25h") || f.Fix == "" {
		t.Fatalf("got %+v %v", f, ok)
	}
	if f.Item != "r#7" || f.Kind != "stuck-pr" || !f.Since.Equal(ago(25*time.Hour)) {
		t.Fatalf("got %+v", f)
	}
}

func TestRedUnder24hIsNotStuck(t *testing.T) {
	if _, ok := ClassifyPR(pr("red", 23*time.Hour), false, now); ok {
		t.Fatal("unexpected finding")
	}
}

func TestRedRetryableIsNotStuck(t *testing.T) {
	if _, ok := ClassifyPR(pr("red", 25*time.Hour), true, now); ok {
		t.Fatal("unexpected finding")
	}
}

func TestPendingIsNeverStuck(t *testing.T) {
	if _, ok := ClassifyPR(pr("pending", 100*time.Hour), false, now); ok {
		t.Fatal("unexpected finding")
	}
}

func TestGreenUnmergedOver24hIsStuck(t *testing.T) {
	f, ok := ClassifyPR(pr("green", 25*time.Hour), false, now)
	if !ok || !strings.Contains(f.Why, "green, unmerged 25h") || f.Fix == "" {
		t.Fatalf("got %+v %v", f, ok)
	}
	if _, ok := ClassifyPR(pr("green", 23*time.Hour), false, now); ok {
		t.Fatal("under 24h should not be stuck")
	}
}

func TestGreenUnmergedSaysSettledPending(t *testing.T) {
	p := pr("green", 25*time.Hour)
	p.Settled = &Status{State: "pending", Description: "waiting on clang-22"}
	f, _ := ClassifyPR(p, false, now)
	if !strings.Contains(f.Why, "ingredients-settled pending: waiting on clang-22") {
		t.Fatalf("why = %q", f.Why)
	}
}

func TestGreenUnmergedSaysConflict(t *testing.T) {
	p := pr("green", 25*time.Hour)
	no := false
	p.Mergeable = &no
	f, _ := ClassifyPR(p, false, now)
	if !strings.Contains(f.Why, "merge conflict") {
		t.Fatalf("why = %q", f.Why)
	}
}

func TestRetryRequestedNoRunWithin1hIsStuck(t *testing.T) {
	p := pr("green", time.Hour)
	r := ago(61 * time.Minute)
	p.RetryRequestedAt = &r
	f, ok := ClassifyPR(p, false, now)
	if !ok || f.Fix == "" || !f.Since.Equal(r) || !strings.Contains(f.Why, "1h") {
		t.Fatalf("got %+v %v", f, ok)
	}
	p.RunStartedSince = true
	if _, ok := ClassifyPR(p, false, now); ok {
		t.Fatal("run started; not stuck")
	}
	p.RunStartedSince = false
	r2 := ago(59 * time.Minute)
	p.RetryRequestedAt = &r2
	if _, ok := ClassifyPR(p, false, now); ok {
		t.Fatal("under 1h; not stuck")
	}
}

func TestStaleRetryWinsOverGreenUnmerged(t *testing.T) {
	p := pr("green", 30*time.Hour)
	r := ago(2 * time.Hour)
	p.RetryRequestedAt = &r
	f, ok := ClassifyPR(p, false, now)
	if !ok || !f.Since.Equal(r) {
		t.Fatalf("got %+v", f)
	}
}

func th() Thread {
	return Thread{Repo: "r", Number: 3, URL: "u", Author: "Wowfunhappy", LastCommenter: "Wowfunhappy", LastActivity: ago(3 * time.Hour), Title: "Help"}
}

func TestNeedsReplyFromOutsider(t *testing.T) {
	f, ok := NeedsReply(th(), "schmonz")
	if !ok || f.Kind != "needs-reply" || f.Fix == "" || f.Why == "" || !f.Since.Equal(ago(3*time.Hour)) || f.Item != "r#3" {
		t.Fatalf("got %+v %v", f, ok)
	}
}

func TestNoReplyNeededWhenMaintainerSpokeLast(t *testing.T) {
	x := th()
	x.LastCommenter = "schmonz"
	if _, ok := NeedsReply(x, "schmonz"); ok {
		t.Fatal("unexpected finding")
	}
}

func TestBotsAndDependencyDashboardIgnored(t *testing.T) {
	x := th()
	x.AuthorIsBot = true
	if _, ok := NeedsReply(x, "schmonz"); ok {
		t.Fatal("bot")
	}
	y := th()
	y.Title = "Dependency Dashboard"
	if _, ok := NeedsReply(y, "schmonz"); ok {
		t.Fatal("dashboard")
	}
}

func TestAgeFormatBoundaries(t *testing.T) {
	for d, want := range map[time.Duration]string{
		59 * time.Minute: "59m", time.Hour: "1h", 47 * time.Hour: "47h", 48 * time.Hour: "2d",
	} {
		if got := age(d); got != want {
			t.Errorf("age(%v) = %s, want %s", d, got, want)
		}
	}
}
