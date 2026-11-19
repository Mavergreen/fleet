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
	Now            func() time.Time
	Version        string
}

// Command is a registered subcommand.
type Command struct {
	Name string
	Run  func(ctx context.Context, e Env, args []string, asJSON bool) int
}

const defaultCommand = "attention"

// commands is the production registry; later tasks add to it.
func commands() []Command {
	return []Command{
		{Name: "attention", Run: runAttention},
		{Name: "remediate", Run: runRemediate},
		{Name: "timing", Run: runTiming},
	}
}

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
	name := defaultCommand
	if len(rest) > 0 {
		name, rest = rest[0], rest[1:]
	}
	for _, c := range cmds {
		if c.Name == name {
			return c.Run(context.Background(), e, rest, asJSON)
		}
	}
	fmt.Fprintf(e.Stderr, "mavergreen-fleet: unknown command %q\nusage: mavergreen-fleet [--json] [--version] [command] [args]\n", name)
	return 2
}

// newClient builds the GitHub client; tests replace it.
var newClient = func(token string) ghapi.Client { return ghapi.New(token, ghapi.Options{}) }

// connect returns a client for the user's token.
func connect(e Env) (ghapi.Client, error) {
	token, err := auth.Token(e.Getenv, func(name string, args ...string) ([]byte, error) {
		return exec.Command(name, args...).Output()
	})
	if err != nil {
		return nil, err
	}
	return newClient(token), nil
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

func runAttention(ctx context.Context, e Env, _ []string, asJSON bool) int {
	c, err := connect(e)
	if err != nil {
		fmt.Fprintf(e.Stderr, "mavergreen-fleet: %v\n", err)
		return 1
	}
	now := e.Now()
	fs, checked, err := attention.Collect(ctx, c, org, "", retry.NewAttention(c, org, scripts(e), now), now)
	if err != nil {
		fmt.Fprintf(e.Stderr, "mavergreen-fleet: %v\n", err)
		return 1
	}
	width, _ := strconv.Atoi(e.Getenv("COLUMNS"))
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
			fmt.Fprintf(e.Stderr, "mavergreen-fleet: remediate: unknown argument %q\nusage: mavergreen-fleet [--json] remediate [--dry-run]\n", a)
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
	actions, err := retry.Plan(ctx, c, org, scripts(e), now)
	if err != nil {
		fmt.Fprintf(e.Stderr, "mavergreen-fleet: %v\n", err)
		return 1
	}
	results := make([]retry.Result, len(actions))
	for i, a := range actions {
		results[i] = retry.Apply(ctx, c, a, dry)
	}
	if err := printRemediate(e.Stdout, results, dry, asJSON, now); err != nil {
		fmt.Fprintf(e.Stderr, "mavergreen-fleet: %v\n", err)
		return 1
	}
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
func printRemediate(w io.Writer, results []retry.Result, dry, asJSON bool, now time.Time) error {
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
		_, err := fmt.Fprintln(w, "nothing to remediate: no red open PRs or latest main/scheduled runs")
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
