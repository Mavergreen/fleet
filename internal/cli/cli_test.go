package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
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

func TestNoSubcommandRunsAttention(t *testing.T) {
	e, _, _ := testEnv()
	called := false
	cmds := []Command{{Name: "attention", Run: func(ctx context.Context, e Env, args []string, asJSON bool) int {
		called = true
		return 0
	}}}
	if code := run([]string{}, e, cmds); code != 0 || !called {
		t.Fatalf("code=%d called=%v", code, called)
	}
}

func TestUnknownSubcommandExits2(t *testing.T) {
	e, _, errb := testEnv()
	code := run([]string{"bogus"}, e, nil)
	if code != 2 || !strings.Contains(errb.String(), "usage:") {
		t.Fatalf("code=%d stderr=%q", code, errb.String())
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
