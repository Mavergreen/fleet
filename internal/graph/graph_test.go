package graph

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"reflect"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

func read(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	p := "testdata/" + name
	if *update {
		if err := os.WriteFile(p, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("%s differs:\n--- got\n%s--- want\n%s", name, got, want)
	}
}

// real is the real script's output (fixture); synthetic adds a cycle and
// the rust -> clang-22 edge, which the real family does not yet have.
func real(t *testing.T) Graph {
	g, err := Parse(read(t, "family-graph.out"), read(t, "family-graph.err"))
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func synthetic(t *testing.T) Graph {
	g, err := Parse(read(t, "family-graph-synthetic.out"), nil)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestParseEdgesDiamondsCycles(t *testing.T) {
	g := real(t)
	if len(g.Edges) != 22 || g.Edges[0] != (Edge{"claude-code", "drydock"}) {
		t.Errorf("edges: %d %v", len(g.Edges), g.Edges[0])
	}
	if len(g.Diamonds) != 3 || g.Diamonds[0] != "DIAMOND wezterm: clang-22 via make" {
		t.Errorf("diamonds: %v", g.Diamonds)
	}
	if len(g.Cycles) != 0 {
		t.Errorf("cycles: %v", g.Cycles)
	}
	s := synthetic(t)
	if !reflect.DeepEqual(s.Cycles, []string{"CYCLE a -> b -> a"}) {
		t.Errorf("cycles: %v", s.Cycles)
	}
	found := false
	for _, e := range s.Edges {
		found = found || e == (Edge{"rust", "clang-22"})
	}
	if !found {
		t.Error("rust -> clang-22 missing")
	}
}

type result struct {
	out, err string
	exit     int
	runErr   error
}

func fake(r result, got *[]string) func(context.Context, string, ...string) ([]byte, []byte, int, error) {
	return func(_ context.Context, name string, args ...string) ([]byte, []byte, int, error) {
		*got = append([]string{name}, args...)
		return []byte(r.out), []byte(r.err), r.exit, r.runErr
	}
}

func TestExit3KeepsGraphAndListsIncomplete(t *testing.T) {
	var argv []string
	g, err := Load(context.Background(), "/s", fake(result{string(read(t, "family-graph.out")), string(read(t, "family-graph.err")), 3, nil}, &argv))
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Edges) != 22 {
		t.Errorf("edges %d", len(g.Edges))
	}
	if len(g.Incomplete) != 24 || g.Incomplete[0] != "UNMAPPED ca-certs: withdrawn" {
		t.Errorf("incomplete %d %v", len(g.Incomplete), g.Incomplete)
	}
	var b bytes.Buffer
	Tree(&b, g)
	if !strings.Contains(b.String(), "cannot read: SKIP nodejs-24: no clone in DIR") {
		t.Errorf("tree lacks cannot-read:\n%s", b.String())
	}
}

func TestScriptFailureNamesWhatItNeeds(t *testing.T) {
	var argv []string
	for _, r := range []result{
		{err: "python3: not found\n", exit: 127},
		{err: "gh failed", exit: 2},
		{err: "bad", exit: 1},
		{runErr: errors.New("exec: sh: not found")},
	} {
		_, err := Load(context.Background(), "/s", fake(r, &argv))
		if err == nil || !strings.Contains(err.Error(), "python-315") || !strings.HasPrefix(err.Error(), "graph: family-graph.sh failed: ") {
			t.Errorf("%+v: %v", r, err)
			continue
		}
		if r.err != "" && !strings.Contains(err.Error(), strings.TrimSpace(r.err)) {
			t.Errorf("stderr missing: %v", err)
		}
	}
}

func TestFromDirPassedThrough(t *testing.T) {
	var argv []string
	if _, err := Load(context.Background(), "/s", fake(result{out: "a -> b\n"}, &argv), "--from-dir", "/clones"); err != nil {
		t.Fatal(err)
	}
	want := []string{"sh", "/s/family-graph.sh", "--from-dir", "/clones"}
	if !reflect.DeepEqual(argv, want) {
		t.Errorf("argv %v", argv)
	}
	if _, err := Load(context.Background(), "/s", fake(result{out: "a -> b\n"}, &argv)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(argv, want[:2]) {
		t.Errorf("argv %v", argv)
	}
}

func TestTreeRendering(t *testing.T) {
	var b bytes.Buffer
	Tree(&b, real(t))
	golden(t, "tree.golden", b.Bytes())
	b.Reset()
	Tree(&b, synthetic(t))
	golden(t, "tree-synthetic.golden", b.Bytes())
}

func TestTreeRepeatsSubtreeOnlyOnce(t *testing.T) {
	var b bytes.Buffer
	Tree(&b, synthetic(t))
	if !strings.Contains(b.String(), "(see above)") {
		t.Errorf("no (see above):\n%s", b.String())
	}
}

func TestDOTRendering(t *testing.T) {
	var b bytes.Buffer
	DOT(&b, synthetic(t))
	if !strings.Contains(b.String(), `"rust" -> "clang-22";`) {
		t.Errorf("edge missing:\n%s", b.String())
	}
	golden(t, "dot.golden", b.Bytes())
}

func TestMermaidRendering(t *testing.T) {
	var b bytes.Buffer
	Mermaid(&b, synthetic(t))
	golden(t, "mermaid.golden", b.Bytes())
}

func TestTimingsNotice(t *testing.T) {
	for name, render := range map[string]func(w *bytes.Buffer, g Graph){
		"tree":    func(w *bytes.Buffer, g Graph) { Tree(w, g) },
		"dot":     func(w *bytes.Buffer, g Graph) { DOT(w, g) },
		"mermaid": func(w *bytes.Buffer, g Graph) { Mermaid(w, g) },
	} {
		for _, g := range []Graph{real(t), {}} {
			var b bytes.Buffer
			render(&b, g)
			lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
			last := lines[len(lines)-1]
			if !strings.HasSuffix(last, TimingsNotice) {
				t.Errorf("%s last line %q", name, last)
			}
		}
	}
	if TimingsNotice != "propagation timings: not yet available (needs family-graph.sh to print pin paths; queued in shipyard)" {
		t.Error(TimingsNotice)
	}
}
