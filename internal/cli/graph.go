package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/Mavergreen/fleet/internal/auth"
	"github.com/Mavergreen/fleet/internal/graph"
	"github.com/Mavergreen/fleet/internal/render"
	"github.com/Mavergreen/fleet/internal/shipyard"
)

const graphUsage = "usage: mavergreen-fleet [--json] graph [--dot | --mermaid] [--from-dir DIR]"

const timingsJSON = "not yet available (needs family-graph.sh to print pin paths; queued in shipyard)"

// graphRunner builds the exec seam for family-graph.sh; tests replace it.
// The environment passes through, with GH_TOKEN added when it is not set.
var graphRunner = func(e Env) graph.Runner {
	return func(ctx context.Context, name string, args ...string) ([]byte, []byte, int, error) {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Env = os.Environ()
		if e.Getenv("GH_TOKEN") == "" {
			if tok, err := auth.Token(e.Getenv, func(n string, a ...string) ([]byte, error) {
				return exec.Command(n, a...).Output()
			}); err == nil {
				cmd.Env = append(cmd.Env, "GH_TOKEN="+tok)
			}
		}
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		err := cmd.Run()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return out.Bytes(), errb.Bytes(), ee.ExitCode(), nil
		}
		return out.Bytes(), errb.Bytes(), 0, err
	}
}

func runGraph(ctx context.Context, e Env, args []string, asJSON bool) int {
	format := "tree"
	var pass []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--dot":
			format = "dot"
		case a == "--mermaid":
			format = "mermaid"
		case a == "--from-dir" && i+1 < len(args):
			pass = append(pass, a, args[i+1])
			i++
		default:
			fmt.Fprintf(e.Stderr, "mavergreen-fleet: graph: unknown argument %q\n%s\n", a, graphUsage)
			return 2
		}
	}
	dir := ""
	if p := e.Getenv("SHIPYARD_SCRIPTS"); p != "" {
		if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
			fmt.Fprintf(e.Stderr, "mavergreen-fleet: graph: SHIPYARD_SCRIPTS=%s is not a directory\n", p)
			return 1
		}
		dir = p
	} else {
		var err error
		dir, err = shipyard.Scripts(e.Getenv, func(p string) bool {
			fi, err := os.Stat(p)
			return err == nil && fi.IsDir()
		})
		if err != nil {
			fmt.Fprintf(e.Stderr, "mavergreen-fleet: graph: %v\n", err)
			return 1
		}
	}
	g, err := graph.Load(ctx, strings.TrimRight(dir, "/"), graphRunner(e), pass...)
	if err != nil {
		fmt.Fprintf(e.Stderr, "mavergreen-fleet: %v\n", err)
		return 1
	}
	if asJSON {
		edges := g.Edges
		if edges == nil {
			edges = []graph.Edge{}
		}
		nn := func(s []string) []string {
			if s == nil {
				return []string{}
			}
			return s
		}
		err = render.JSON(e.Stdout, "graph", struct {
			Edges      []graph.Edge `json:"edges"`
			Diamonds   []string     `json:"diamonds"`
			Cycles     []string     `json:"cycles"`
			Incomplete []string     `json:"incomplete"`
			Timings    string       `json:"timings"`
		}{edges, nn(g.Diamonds), nn(g.Cycles), nn(g.Incomplete), timingsJSON}, e.Now())
		if err != nil {
			fmt.Fprintf(e.Stderr, "mavergreen-fleet: %v\n", err)
			return 1
		}
		return 0
	}
	switch format {
	case "dot":
		graph.DOT(e.Stdout, g)
	case "mermaid":
		graph.Mermaid(e.Stdout, g)
	default:
		graph.Tree(e.Stdout, g)
	}
	return 0
}
