package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Mavergreen/fleet/internal/ghapi"
)

func testEnv() (Env, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	return Env{
		Stdout:  &out,
		Stderr:  &errb,
		Getenv:  func(string) string { return "" },
		Now:     time.Now,
		Version: "1.2.3",
	}, &out, &errb
}

func stubCmds(called *bool) []Command {
	run := func(ctx context.Context, e Env, args []string, asJSON bool) int { *called = true; return 0 }
	return []Command{
		{Name: "attention", Usage: "attention", Summary: "what needs a human", Run: run},
		{Name: "timing", Usage: "timing [repo [run-id]]", Summary: "where Actions time goes", Run: run},
	}
}

func TestBareInvocationPrintsHelp(t *testing.T) {
	for _, args := range [][]string{{}, {"--help"}, {"-h"}, {"help"}, {"--json"}} {
		e, out, errb := testEnv()
		called := false
		if code := run(args, e, stubCmds(&called)); code != 0 || called {
			t.Fatalf("%v: code=%d called=%v", args, code, called)
		}
		got := out.String()
		for _, want := range []string{
			"usage: mavergreen-fleet [--json] <command> [args]",
			"  attention  what needs a human",
			"  timing     where Actions time goes",
			"mavergreen-fleet <command> --help",
			"man mavergreen-fleet",
		} {
			if !strings.Contains(got, want) {
				t.Fatalf("%v: help lacks %q:\n%s", args, want, got)
			}
		}
		if errb.Len() != 0 {
			t.Fatalf("%v: stderr %q", args, errb.String())
		}
	}
}

func TestCommandHelpPrintsItsUsage(t *testing.T) {
	for _, flag := range []string{"--help", "-h"} {
		e, out, _ := testEnv()
		called := false
		code := run([]string{"timing", "ed25519", flag}, e, stubCmds(&called))
		if code != 0 || called {
			t.Fatalf("%s: code=%d called=%v", flag, code, called)
		}
		if got := out.String(); got != "usage: mavergreen-fleet [--json] timing [repo [run-id]]\nwhere Actions time goes\nSee: man mavergreen-fleet\n" {
			t.Fatalf("%s: got %q", flag, got)
		}
	}
}

func TestUnknownSubcommandExits2WithHelp(t *testing.T) {
	e, out, errb := testEnv()
	called := false
	code := run([]string{"bogus"}, e, stubCmds(&called))
	if code != 2 || called || out.Len() != 0 {
		t.Fatalf("code=%d called=%v stdout=%q", code, called, out.String())
	}
	got := errb.String()
	if !strings.HasPrefix(got, "mavergreen-fleet: unknown command \"bogus\"\n") || !strings.Contains(got, "  attention ") {
		t.Fatalf("stderr %q", got)
	}
}

func TestEveryCommandHasUsageAndSummary(t *testing.T) {
	for _, c := range commands() {
		if !strings.HasPrefix(c.Usage, c.Name) || c.Summary == "" {
			t.Fatalf("%s: usage %q summary %q", c.Name, c.Usage, c.Summary)
		}
	}
}

func TestVersionFlag(t *testing.T) {
	e, out, _ := testEnv()
	if code := Run([]string{"--version"}, e); code != 0 {
		t.Fatalf("code=%d", code)
	}
	if got := out.String(); got != "mavergreen-fleet 1.2.3\n" {
		t.Fatalf("got %q", got)
	}
}

func TestJSONFlagIsGlobal(t *testing.T) {
	e, _, _ := testEnv()
	var gotJSON bool
	var gotArgs []string
	cmds := []Command{{Name: "attention", Run: func(ctx context.Context, e Env, args []string, asJSON bool) int {
		gotJSON, gotArgs = asJSON, args
		return 0
	}}}
	run([]string{"--json", "attention", "x"}, e, cmds)
	if !gotJSON || len(gotArgs) != 1 || gotArgs[0] != "x" {
		t.Fatalf("json=%v args=%v", gotJSON, gotArgs)
	}
}

// useClient makes connect return c for the rest of the test.
func useClient(t *testing.T, c ghapi.Client) {
	t.Helper()
	old := newClient
	newClient = func(string, io.Writer) ghapi.Client { return c }
	t.Cleanup(func() { newClient = old })
}
