// Package graph shows the family's ingredient graph, as computed by
// shipyard's family-graph.sh. fleet only runs the script and renders its
// stdout, stderr and exit code.
package graph

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
)

// TimingsNotice is the last line of every rendering.
const TimingsNotice = "propagation timings: not yet available (needs family-graph.sh to print pin paths; queued in shipyard)"

// Edge says Consumer uses Ingredient's product.
type Edge struct {
	Consumer   string `json:"consumer"`
	Ingredient string `json:"ingredient"`
}

// Graph is the parsed script output. Incomplete holds the script's stderr
// SKIP and UNMAPPED lines: what it could not read.
type Graph struct {
	Edges      []Edge
	Diamonds   []string
	Cycles     []string
	Incomplete []string
}

// Parse reads family-graph.sh's stdout and stderr.
func Parse(stdout, stderr []byte) (Graph, error) {
	var g Graph
	for _, l := range lines(stdout) {
		switch {
		case strings.HasPrefix(l, "DIAMOND "):
			g.Diamonds = append(g.Diamonds, l)
		case strings.HasPrefix(l, "CYCLE "):
			g.Cycles = append(g.Cycles, l)
		default:
			c, i, ok := strings.Cut(l, " -> ")
			if !ok || c == "" || i == "" {
				return Graph{}, fmt.Errorf("graph: unrecognised line from family-graph.sh: %q", l)
			}
			g.Edges = append(g.Edges, Edge{c, i})
		}
	}
	for _, l := range lines(stderr) {
		if strings.HasPrefix(l, "SKIP ") || strings.HasPrefix(l, "UNMAPPED ") {
			g.Incomplete = append(g.Incomplete, l)
		}
	}
	return g, nil
}

func lines(b []byte) []string {
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// Runner is the exec seam.
type Runner = func(ctx context.Context, name string, args ...string) (stdout, stderr []byte, exit int, err error)

// Load runs family-graph.sh from scripts, with extra args (such as
// --from-dir DIR) passed through. Exit 0 and 3 (partial) yield a graph.
func Load(ctx context.Context, scripts string, run Runner, args ...string) (Graph, error) {
	stdout, stderr, exit, err := run(ctx, "sh", append([]string{scripts + "/family-graph.sh"}, args...)...)
	if err != nil || (exit != 0 && exit != 3) {
		why := strings.TrimSpace(string(stderr))
		if err != nil {
			why = strings.TrimSpace(why + " " + err.Error())
		} else if why == "" {
			why = fmt.Sprintf("exit %d", exit)
		}
		return Graph{}, fmt.Errorf("graph: family-graph.sh failed: %s; it needs gh (and python3 before the flag day; on 10.9 the family's python-315 and gh products)", why)
	}
	return Parse(stdout, stderr)
}

func children(g Graph) map[string][]string {
	m := map[string][]string{}
	for _, e := range g.Edges {
		m[e.Consumer] = append(m[e.Consumer], e.Ingredient)
	}
	for _, v := range m {
		sort.Strings(v)
	}
	return m
}

func sortedKeys(m map[string][]string) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// Tree writes each consumer with its ingredients below it. A node's own
// ingredients are listed once per tree; later occurrences say "(see above)".
func Tree(w io.Writer, g Graph) {
	var b bytes.Buffer
	kids := children(g)
	if len(g.Edges) == 0 {
		b.WriteString("no ingredient edges\n")
	}
	expanded := map[string]bool{}
	var walk func(name, prefix string)
	walk = func(name, prefix string) {
		ks := kids[name]
		for i, k := range ks {
			branch, next := "├─ ", "│  "
			if i == len(ks)-1 {
				branch, next = "└─ ", "   "
			}
			if len(kids[k]) == 0 {
				fmt.Fprintf(&b, "%s%s%s\n", prefix, branch, k)
			} else if expanded[k] {
				fmt.Fprintf(&b, "%s%s%s (see above)\n", prefix, branch, k)
			} else {
				expanded[k] = true
				fmt.Fprintf(&b, "%s%s%s\n", prefix, branch, k)
				walk(k, prefix+next)
			}
		}
	}
	for _, c := range sortedKeys(kids) {
		if expanded[c] {
			fmt.Fprintf(&b, "%s (see above)\n", c)
			continue
		}
		expanded[c] = true
		fmt.Fprintln(&b, c)
		walk(c, "")
	}
	section := func(title string, ls []string) {
		if len(ls) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n%s:\n", title)
		for _, l := range ls {
			fmt.Fprintf(&b, "  %s\n", l)
		}
	}
	section("diamonds", g.Diamonds)
	section("cycles", g.Cycles)
	if len(g.Incomplete) > 0 {
		b.WriteString("\n")
		for _, l := range g.Incomplete {
			fmt.Fprintf(&b, "cannot read: %s\n", l)
		}
	}
	b.WriteString("\n" + TimingsNotice + "\n")
	w.Write(b.Bytes())
}

// DOT writes Graphviz source. The timings notice is a comment.
func DOT(w io.Writer, g Graph) {
	var b bytes.Buffer
	b.WriteString("digraph family {\n")
	for _, e := range sortedEdges(g) {
		fmt.Fprintf(&b, "  %q -> %q;\n", e.Consumer, e.Ingredient)
	}
	b.WriteString("}\n// " + TimingsNotice + "\n")
	w.Write(b.Bytes())
}

// Mermaid writes a left-to-right flowchart. Nodes get safe ids (n0, n1, ...)
// with the repo name as a quoted label, since names contain "-" and may
// collide with Mermaid keywords such as "end". The notice is a %% comment.
func Mermaid(w io.Writer, g Graph) {
	var b bytes.Buffer
	b.WriteString("graph LR\n")
	set := map[string]bool{}
	for _, e := range g.Edges {
		set[e.Consumer], set[e.Ingredient] = true, true
	}
	var names []string
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	id := map[string]string{}
	for i, n := range names {
		id[n] = fmt.Sprintf("n%d", i)
		fmt.Fprintf(&b, "  %s[%q]\n", id[n], n)
	}
	for _, e := range sortedEdges(g) {
		fmt.Fprintf(&b, "  %s --> %s\n", id[e.Consumer], id[e.Ingredient])
	}
	b.WriteString("%% " + TimingsNotice + "\n")
	w.Write(b.Bytes())
}

func sortedEdges(g Graph) []Edge {
	es := append([]Edge(nil), g.Edges...)
	sort.Slice(es, func(i, j int) bool {
		if es[i].Consumer != es[j].Consumer {
			return es[i].Consumer < es[j].Consumer
		}
		return es[i].Ingredient < es[j].Ingredient
	})
	return es
}
