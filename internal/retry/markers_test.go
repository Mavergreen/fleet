package retry

import (
	"os"
	"strings"
	"testing"
)

func TestTickRebaseBox(t *testing.T) {
	in := "intro\n- [ ] <!-- rebase-check -->If you want to rebase/retry this PR, check this box\nouttro\n"
	got, st := TickRebaseBox(in)
	want := "intro\n- [x] <!-- rebase-check -->If you want to rebase/retry this PR, check this box\nouttro\n"
	if st != BoxTicked || got != want {
		t.Fatalf("got %q, %v", got, st)
	}
}

func TestTickRebaseBoxRealBody(t *testing.T) {
	b, err := os.ReadFile("testdata/renovate-body.md")
	if err != nil {
		t.Fatal(err)
	}
	got, st := TickRebaseBox(string(b))
	if st != BoxTicked {
		t.Fatalf("state %v", st)
	}
	if strings.Replace(string(b), "[ ]", "[x]", 1) != got {
		t.Fatal("body changed beyond the checkbox")
	}
}

func TestTickRebaseBoxAbsent(t *testing.T) {
	in := "nothing to see\n- [ ] something else\n"
	got, st := TickRebaseBox(in)
	if st != BoxAbsent || got != in {
		t.Fatalf("got %q, %v", got, st)
	}
}

func TestTickRebaseBoxAlreadyTicked(t *testing.T) {
	for _, x := range []string{"x", "X"} {
		in := "a\n- [" + x + "] <!-- rebase-check -->retry\n"
		got, st := TickRebaseBox(in)
		if st != BoxAlreadyTicked || got != in {
			t.Fatalf("%s: got %q, %v", x, got, st)
		}
	}
}

func TestTickPreservesCRLF(t *testing.T) {
	in := "a\r\n\r\n- [ ] <!-- rebase-check -->retry\r\nb\r\n"
	got, st := TickRebaseBox(in)
	want := "a\r\n\r\n- [x] <!-- rebase-check -->retry\r\nb\r\n"
	if st != BoxTicked || got != want {
		t.Fatalf("got %q, %v", got, st)
	}
}

func TestCommentRoundTrip(t *testing.T) {
	u := UpdateKey("Update dependency clang-22 to v22.1.9-mavericks.1")
	c := Comment(1, Cap, "retryable:e2e-check6-hang", "https://github.com/o/r/actions/runs/1", u)
	if !strings.Contains(c, "fleet: retry 1/2, pattern `retryable:e2e-check6-hang`, run https://github.com/o/r/actions/runs/1") {
		t.Fatalf("visible line: %q", c)
	}
	if n := CountRetries([]string{c}, u); n != 1 {
		t.Fatalf("count %d", n)
	}
}

func TestCountIgnoresOtherUpdates(t *testing.T) {
	c := Comment(1, Cap, "p", "u", "Update a")
	if n := CountRetries([]string{c}, "Update b"); n != 0 {
		t.Fatalf("count %d", n)
	}
}

func TestNewVersionResetsCount(t *testing.T) {
	u1 := UpdateKey("Update dependency clang-22 to v22.1.9-mavericks.1")
	u2 := UpdateKey("Update dependency clang-22 to v22.1.9-mavericks.2")
	bodies := []string{Comment(1, Cap, "p", "u", u1), Comment(2, Cap, "p", "u", u1), Comment(1, Cap, "p", "u", u2)}
	if a, b := CountRetries(bodies, u1), CountRetries(bodies, u2); a != 2 || b != 1 {
		t.Fatalf("got %d, %d", a, b)
	}
}

func TestCountRetriesIgnoresMalformed(t *testing.T) {
	u := "Update x"
	good := Comment(1, Cap, "p", "u", u)
	bodies := []string{
		"<!-- fleet-retry v1 update=Update+x pattern=p -->",
		"<!-- fleet-retry v1 attempt=1 pattern=p -->",
		"<!-- fleet-retry v1 update=Update+x attempt=two pattern=p -->",
		"<!-- fleet-retry v2 update=Update+x attempt=1 pattern=p -->",
		"<!-- fleet-retry v1 update=Update+x attempt=-1 pattern=p -->",
		"<!-- fleet-retry v1 update=Update+x attempt=+1 pattern=p -->",
		"<!-- fleet-retry v1 update=Update+x attempt=0 pattern=p -->",
		"no marker at all",
		good + "\n" + good, // several markers in one body all count
	}
	if n := CountRetries(bodies, u); n != 2 {
		t.Fatalf("count %d", n)
	}
}

func TestCommentEscapesUpdate(t *testing.T) {
	u := "Update --> sneaky & more"
	c := Comment(1, Cap, "p", "u", u)
	if strings.Count(c, "-->") != 1 {
		t.Fatalf("marker not safe: %q", c)
	}
	if CountRetries([]string{c}, u) != 1 {
		t.Fatal("round trip failed")
	}
}

func TestUpdateKey(t *testing.T) {
	if got := UpdateKey("  Update \t dependency x\n to  v1 "); got != "Update dependency x to v1" {
		t.Fatalf("%q", got)
	}
}
