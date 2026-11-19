// Package cli dispatches mavergreen-fleet subcommands.
package cli

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"time"

	"github.com/Mavergreen/fleet/internal/attention"
	"github.com/Mavergreen/fleet/internal/auth"
	"github.com/Mavergreen/fleet/internal/ghapi"
	"github.com/Mavergreen/fleet/internal/render"
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

func runAttention(ctx context.Context, e Env, _ []string, asJSON bool) int {
	token, err := auth.Token(e.Getenv, func(name string, args ...string) ([]byte, error) {
		return exec.Command(name, args...).Output()
	})
	if err != nil {
		fmt.Fprintf(e.Stderr, "mavergreen-fleet: %v\n", err)
		return 1
	}
	now := e.Now()
	c := ghapi.New(token, ghapi.Options{})
	// A later task replaces NoRetry with the retry package's implementation.
	fs, checked, err := attention.Collect(ctx, c, org, "", attention.NoRetry{}, now)
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
