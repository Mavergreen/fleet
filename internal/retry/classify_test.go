package retry

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "logs", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func startedJob() Job {
	return Job{Repo: "Mavergreen/rust", Workflow: "release.yml", Name: "build-native",
		HeadBranch: "main", RunID: 1, JobID: 2, Attempt: 1, Conclusion: "failure",
		RunnerName: "GitHub Actions 12", AnyStepStarted: true}
}

func wantID(t *testing.T, j Job, log []byte, id string) {
	t.Helper()
	p, ok := Classify(j, log, Builtin())
	if !ok || p.ID != id {
		t.Fatalf("Classify = %q, %v; want %q", p.ID, ok, id)
	}
	if p.Evidence == "" {
		t.Fatalf("pattern %s has no evidence", p.ID)
	}
}

func TestBuiltinNeverStartedMatches(t *testing.T) {
	j := Job{Conclusion: "cancelled", RunnerName: "", AnyStepStarted: false}
	wantID(t, j, nil, "never-started")
}

func TestNeverStartedRequiresAllThree(t *testing.T) {
	for _, j := range []Job{
		{Conclusion: "failure"},
		{Conclusion: "cancelled", RunnerName: "r1"},
		{Conclusion: "cancelled", AnyStepStarted: true},
	} {
		if p, ok := Classify(j, nil, Builtin()); ok {
			t.Fatalf("%+v matched %s", j, p.ID)
		}
	}
}

func TestBuiltinRunnerLostMatches(t *testing.T) {
	wantID(t, startedJob(), fixture(t, "runner-lost.txt"), "runner-lost")
	wantID(t, startedJob(), fixture(t, "runner-shutdown.txt"), "runner-lost")
}

func TestBuiltinNetworkFetchMatches(t *testing.T) {
	wantID(t, startedJob(), fixture(t, "network-fetch.txt"), "network-fetch")
	wantID(t, startedJob(), fixture(t, "network-fetch-http.txt"), "network-fetch")
}

func TestNetworkRetryLineAloneIsNotRetryable(t *testing.T) {
	log := []byte("x\t2026-11-09T06:09:13.4Z shipyard_curl: https://h/x: curl exit 56; retry 1 of 5 in 2s\n")
	if p, ok := Classify(startedJob(), log, Builtin()); ok {
		t.Fatalf("matched %s", p.ID)
	}
}

func TestCompileErrorIsNotRetryable(t *testing.T) {
	if p, ok := Classify(startedJob(), fixture(t, "build-error.txt"), Builtin()); ok {
		t.Fatalf("compile error matched %s", p.ID)
	}
}

func TestClassifyMissingLogIsNotRetryable(t *testing.T) {
	if p, ok := Classify(startedJob(), nil, Builtin()); ok {
		t.Fatalf("nil log matched %s", p.ID)
	}
}

func TestDeclaredPatternScopedToWorkflowAndJob(t *testing.T) {
	declared := []Pattern{{ID: "flaky-thing", Evidence: "maintainer-declared", Workflow: "release.yml",
		Job: "on-mavericks", LogRE: regexp.MustCompile(`flaky thing happened`)}}
	log := []byte("t 2026-01-01T00:00:00Z flaky thing happened\n")
	on := startedJob()
	on.Name = "on-mavericks"
	if p, ok := Classify(on, log, declared); !ok || p.ID != "flaky-thing" {
		t.Fatalf("in-scope job: %q, %v", p.ID, ok)
	}
	build := startedJob()
	build.Name = "build"
	if p, ok := Classify(build, log, declared); ok {
		t.Fatalf("job build matched %s", p.ID)
	}
	other := on
	other.Workflow = "ci.yml"
	if p, ok := Classify(other, log, declared); ok {
		t.Fatalf("workflow ci.yml matched %s", p.ID)
	}
}

func TestClassifyReturnsFirstMatch(t *testing.T) {
	a := Pattern{ID: "a", LogRE: regexp.MustCompile(`boom`)}
	b := Pattern{ID: "b", LogRE: regexp.MustCompile(`boom`)}
	p, ok := Classify(startedJob(), []byte("boom"), []Pattern{a, b})
	if !ok || p.ID != "a" {
		t.Fatalf("got %q", p.ID)
	}
}

func TestToleratedGiveUpThenBuildErrorIsNotRetryable(t *testing.T) {
	var giveUp string
	for _, l := range strings.Split(string(fixture(t, "network-fetch.txt")), "\n") {
		if strings.Contains(l, "giving up after") {
			giveUp = l
			break
		}
	}
	if giveUp == "" {
		t.Fatal("no give-up line in fixture")
	}
	if _, ok := Classify(startedJob(), []byte(giveUp+"\n##[error]Process completed with exit code 1.\n"), Builtin()); !ok {
		t.Fatal("precondition: the give-up line alone must match network-fetch")
	}
	log := append([]byte(giveUp+"\n"), fixture(t, "build-error.txt")...)
	if p, ok := Classify(startedJob(), log, Builtin()); ok {
		t.Fatalf("tolerated give-up before a build error matched %s", p.ID)
	}
}

// Known limit of finalTailLines: a short failure after a tolerated give-up is still retried.
func TestShortFailureAfterGiveUpIsRetried_DocumentedLimit(t *testing.T) {
	log := []byte("c\t2026-11-08T08:44:25Z shipyard_curl: https://h/x: curl exit 56; giving up after 5 tries\n" +
		"c\t2026-11-08T08:44:26Z make: *** Error 2\n" +
		"c\t2026-11-08T08:44:26Z ##[error]Process completed with exit code 2.\n")
	wantID(t, startedJob(), log, "network-fetch")
}

func TestOtherErrorAfterGiveUpIsNotRetryable(t *testing.T) {
	log := append(fixture(t, "network-fetch-http.txt"), []byte("c\t2026-11-08T08:45:00Z ##[error]Something else broke\n")...)
	if p, ok := Classify(startedJob(), log, Builtin()); ok {
		t.Fatalf("matched %s", p.ID)
	}
}

func TestRunnerLostTextWithoutErrorPrefixNotMatched(t *testing.T) {
	log := []byte("t 2026-11-13T01:14:41Z The hosted runner lost communication with the server.\nt 2026-11-13T01:14:42Z The runner has received a shutdown signal.\n")
	if p, ok := Classify(startedJob(), log, Builtin()); ok {
		t.Fatalf("matched %s", p.ID)
	}
}

func TestTestServer503WithoutGiveUpNotMatched(t *testing.T) {
	log := []byte("t 2026-11-09T06:09:13Z FAIL: test server returned HTTP 503\nt 2026-11-09T06:09:14Z shipyard_curl: https://h/x: HTTP 501; giving up after 5 tries\n")
	if p, ok := Classify(startedJob(), log, Builtin()); ok {
		t.Fatalf("matched %s", p.ID)
	}
}
