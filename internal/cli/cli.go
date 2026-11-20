// Package cli dispatches mavergreen-fleet subcommands.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/Mavergreen/fleet/internal/attention"
	"github.com/Mavergreen/fleet/internal/auth"
	"github.com/Mavergreen/fleet/internal/ghapi"
	"github.com/Mavergreen/fleet/internal/render"
	"github.com/Mavergreen/fleet/internal/retry"
	"github.com/Mavergreen/fleet/internal/shipyard"
)

const org = "Mavergreen"

// Env is everything a command needs from the outside world.
type Env struct {
	Stdout, Stderr io.Writer
	Getenv         func(string) string
	IsTTY          bool
	// Width is the terminal's width in columns (0 if unknown); only asked
	// when IsTTY. A seam so tests need no terminal.
	Width   func() int
	Now     func() time.Time
	Version string
}

// Command is a registered subcommand. Usage is its synopsis after
// "mavergreen-fleet [--json] ", starting with Name; Summary is one line.
type Command struct {
	Name    string
	Usage   string
	Summary string
	Run     func(ctx context.Context, e Env, args []string, asJSON bool) int
}

const (
	usagePrefix    = "usage: mavergreen-fleet [--json] "
	remediateUsage = "remediate [--dry-run]"
)

// commands is the production registry, in the order help lists them.
func commands() []Command {
	return []Command{
		{Name: "attention", Usage: "attention", Summary: "what needs a human, most urgent first", Run: runAttention},
		{Name: "remediate", Usage: remediateUsage, Summary: "retry failures known to be transient", Run: runRemediate},
		{Name: "timing", Usage: timingArgs, Summary: "where Actions time goes", Run: runTiming},
		{Name: "graph", Usage: graphArgs, Summary: "which repos build on which", Run: runGraph},
	}
}

// help writes the overview: every command's name and summary, aligned.
// Each command's full usage is under `<command> --help`.
func help(w io.Writer, cmds []Command) {
	width := 0
	for _, c := range cmds {
		if len(c.Name) > width {
			width = len(c.Name)
		}
	}
	fmt.Fprintf(w, "%s<command> [args]\n       mavergreen-fleet --version\n\nCommands:\n", usagePrefix)
	for _, c := range cmds {
		fmt.Fprintf(w, "  %-*s  %s\n", width, c.Name, c.Summary)
	}
	fmt.Fprint(w, "\nEvery command takes --json. Run mavergreen-fleet <command> --help for one command.\nSee: man mavergreen-fleet\n")
}

func isHelp(a string) bool { return a == "--help" || a == "-h" }

// Run executes the CLI with the production command set and returns the exit code.
func Run(args []string, e Env) int {
	return run(args, e, commands())
}

func run(args []string, e Env, cmds []Command) int {
	asJSON := false
	var rest []string
	for _, a := range args {
		switch a {
		case "--version":
			fmt.Fprintf(e.Stdout, "mavergreen-fleet %s\n", e.Version)
			return 0
		case "--json":
			asJSON = true
		default:
			rest = append(rest, a)
		}
	}
	if len(rest) == 0 || isHelp(rest[0]) || rest[0] == "help" {
		help(e.Stdout, cmds)
		return 0
	}
	name, rest := rest[0], rest[1:]
	for _, c := range cmds {
		if c.Name != name {
			continue
		}
		for _, a := range rest {
			if isHelp(a) {
				fmt.Fprintf(e.Stdout, "%s%s\n%s\nSee: man mavergreen-fleet\n", usagePrefix, c.Usage, c.Summary)
				return 0
			}
		}
		return c.Run(context.Background(), e, rest, asJSON)
	}
	fmt.Fprintf(e.Stderr, "mavergreen-fleet: unknown command %q\n", name)
	help(e.Stderr, cmds)
	return 2
}

// newClient builds the GitHub client; tests replace it.
var newClient = func(token string, notice io.Writer) ghapi.Client {
	return ghapi.New(token, ghapi.Options{Notice: notice})
}

// connect returns a client for the user's token.
func connect(e Env) (ghapi.Client, error) {
	token, err := auth.Token(e.Getenv, func(name string, args ...string) ([]byte, error) {
		return exec.Command(name, args...).Output()
	})
	if err != nil {
		return nil, err
	}
	return newClient(token, e.Stderr), nil
}

// scripts locates shipyard's scripts. Without them, a repo that declares
// retryable patterns is reported as unreadable rather than retried.
func scripts(e Env) string {
	p, err := shipyard.Scripts(e.Getenv, func(p string) bool {
		fi, err := os.Stat(p)
		return err == nil && fi.IsDir()
	})
	if err != nil {
		fmt.Fprintf(e.Stderr, "mavergreen-fleet: warning: %v\n", err)
		return ""
	}
	return p
}

// textWidth is how wide text output may be. On a terminal: COLUMNS when
// set, else the terminal's width; fit is true and output is truncated to
// width (0 means unknown: no truncation). Off a terminal nothing is
// truncated (fit is false), but width still sizes Gantt bars: COLUMNS when
// set, else 80.
func textWidth(e Env) (width int, fit bool) {
	cols, _ := strconv.Atoi(e.Getenv("COLUMNS"))
	if cols < 0 {
		cols = 0
	}
	if !e.IsTTY {
		if cols == 0 {
			cols = 80
		}
		return cols, false
	}
	if cols == 0 && e.Width != nil {
		cols = e.Width()
	}
	return cols, true
}

func runAttention(ctx context.Context, e Env, _ []string, asJSON bool) int {
	c, err := connect(e)
	if err != nil {
		fmt.Fprintf(e.Stderr, "mavergreen-fleet: %v\n", err)
		return 1
	}
	now := e.Now()
	// One memo for the invocation: Collect and remediate's planning read
	// many of the same endpoints.
	m := ghapi.NewMemo(c)
	fs, checked, err := attention.Collect(ctx, m, org, "", retry.NewAttention(m, org, scripts(e), now), now)
	if err != nil {
		fmt.Fprintf(e.Stderr, "mavergreen-fleet: %v\n", err)
		return 1
	}
	width, fit := textWidth(e)
	if !fit {
		width = 0
	}
	if err := render.Findings(e.Stdout, fs, checked, render.Options{TTY: e.IsTTY, Width: width, JSON: asJSON, Now: now}); err != nil {
		fmt.Fprintf(e.Stderr, "mavergreen-fleet: %v\n", err)
		return 1
	}
	return 0
}

func runRemediate(ctx context.Context, e Env, args []string, asJSON bool) int {
	dry := false
	for _, a := range args {
		if a != "--dry-run" {
			fmt.Fprintf(e.Stderr, "mavergreen-fleet: remediate: unknown argument %q\n%s%s\n", a, usagePrefix, remediateUsage)
			return 2
		}
		dry = true
	}
	c, err := connect(e)
	if err != nil {
		fmt.Fprintf(e.Stderr, "mavergreen-fleet: %v\n", err)
		return 1
	}
	now := e.Now()
	results, repos, err := retry.Remediate(ctx, c, org, scripts(e), now, dry)
	if err != nil {
		fmt.Fprintf(e.Stderr, "mavergreen-fleet: %v\n", err)
		return 1
	}
	if err := printRemediate(e.Stdout, results, repos, dry, asJSON, now); err != nil {
		fmt.Fprintf(e.Stderr, "mavergreen-fleet: %v\n", err)
		return 1
	}
	// Result.Err is a failed write, or a read Apply needed before writing:
	// exit 1. A Plan-time cannot-read is a report (Action.Error), exit 0.
	for _, r := range results {
		if r.Err != nil {
			return 1
		}
	}
	return 0
}

type remediateRow struct {
	Kind    retry.ActionKind `json:"kind"`
	Repo    string           `json:"repo"`
	PR      int              `json:"pr,omitempty"`
	RunID   int64            `json:"run_id,omitempty"`
	RunURL  string           `json:"run_url,omitempty"`
	Pattern string           `json:"pattern,omitempty"`
	Attempt int              `json:"attempt,omitempty"`
	Note    string           `json:"note"`
	Done    bool             `json:"done"`
	DryRun  bool             `json:"dry_run"`
	Error   string           `json:"error,omitempty"`
}

// printRemediate writes one line per result: <repo> <PR #n | run id>  <kind>  <note>.
func printRemediate(w io.Writer, results []retry.Result, repos int, dry, asJSON bool, now time.Time) error {
	if asJSON {
		rows := []remediateRow{}
		for _, r := range results {
			a := r.Action
			row := remediateRow{Kind: a.Kind, Repo: a.Repo, PR: a.PR, RunID: a.RunID, RunURL: a.RunURL,
				Pattern: a.PatternID, Attempt: a.Attempt, Note: r.Note, Done: r.Done, DryRun: dry}
			switch {
			case r.Err != nil:
				row.Error = r.Err.Error()
			case a.Error != "":
				row.Error = a.Error // a read error behind a report; not a failure
			}
			rows = append(rows, row)
		}
		return render.JSON(w, "remediate", struct {
			Actions []remediateRow `json:"actions"`
		}{rows}, now)
	}
	if len(results) == 0 {
		_, err := fmt.Fprintf(w, "nothing to remediate across %d repos\n", repos)
		return err
	}
	for _, r := range results {
		a := r.Action
		what := "-"
		switch {
		case a.PR != 0:
			what = fmt.Sprintf("PR #%d", a.PR)
		case a.RunID != 0:
			what = fmt.Sprintf("run %d", a.RunID)
		}
		line := fmt.Sprintf("%s %s  %s  %s", a.Repo, what, a.Kind, r.Note)
		if r.Err != nil {
			line += ": " + r.Err.Error()
		}
		if dry {
			line += " (dry run)"
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}
