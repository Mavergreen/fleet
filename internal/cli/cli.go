// Package cli dispatches mavergreen-fleet subcommands.
package cli

import (
	"context"
	"fmt"
	"io"
	"time"
)

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
		{Name: "attention", Run: func(context.Context, Env, []string, bool) int { return 0 }},
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
