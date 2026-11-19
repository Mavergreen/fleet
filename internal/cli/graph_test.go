package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Mavergreen/fleet/internal/graph"
)

func withRunner(t *testing.T, out, errs string, exit int, got *[]string) {
	t.Helper()
	old := graphRunner
	graphRunner = func(Env) graph.Runner {
		return func(_ context.Context, name string, args ...string) ([]byte, []byte, int, error) {
			*got = append([]string{name}, args...)
			return []byte(out), []byte(errs), exit, nil
		}
	}
	t.Cleanup(func() { graphRunner = old })
}

func graphEnv(t *testing.T) (Env, *strings.Builder, *strings.Builder, string) {
	e, _, _ := testEnv()
	var out, errb strings.Builder
	e.Stdout, e.Stderr = &out, &errb
	dir := t.TempDir()
	e.Getenv = func(k string) string {
		if k == "SHIPYARD_SCRIPTS" {
			return dir
		}
		return ""
	}
	return e, &out, &errb, dir
}

func TestGraphTreeAndFormats(t *testing.T) {
	var argv []string
	withRunner(t, "rust -> clang-22\n", "SKIP x: no clone in D\n", 3, &argv)
	e, out, _, dir := graphEnv(t)
	if code := runGraph(context.Background(), e, []string{"--from-dir", "/c"}, false); code != 0 {
		t.Fatalf("code %d", code)
	}
	if argv[1] != dir+"/family-graph.sh" || argv[3] != "/c" {
		t.Errorf("argv %v", argv)
	}
	if !strings.Contains(out.String(), "cannot read: SKIP x") {
		t.Errorf("%s", out)
	}
	e, out, _, _ = graphEnv(t)
	runGraph(context.Background(), e, []string{"--dot"}, false)
	if !strings.HasPrefix(out.String(), "digraph family {") {
		t.Errorf("%s", out)
	}
	e, out, _, _ = graphEnv(t)
	runGraph(context.Background(), e, []string{"--mermaid"}, false)
	if !strings.HasPrefix(out.String(), "graph LR") {
		t.Errorf("%s", out)
	}
}

func TestGraphJSON(t *testing.T) {
	var argv []string
	withRunner(t, "rust -> clang-22\nDIAMOND a: b via c\n", "UNMAPPED r: n\n", 3, &argv)
	e, out, _, _ := graphEnv(t)
	if code := runGraph(context.Background(), e, nil, true); code != 0 {
		t.Fatalf("code %d", code)
	}
	var v struct {
		Command                      string
		Edges                        []map[string]string
		Diamonds, Cycles, Incomplete []string
		Timings                      string
	}
	if err := json.Unmarshal([]byte(out.String()), &v); err != nil {
		t.Fatal(err)
	}
	if v.Command != "graph" || v.Edges[0]["consumer"] != "rust" || v.Edges[0]["ingredient"] != "clang-22" ||
		len(v.Diamonds) != 1 || v.Cycles == nil || len(v.Incomplete) != 1 ||
		v.Timings != "not yet available (needs family-graph.sh to print pin paths; queued in shipyard)" {
		t.Errorf("%+v\n%s", v, out)
	}
}

func TestGraphErrorExits1(t *testing.T) {
	var argv []string
	withRunner(t, "", "python3: not found", 127, &argv)
	e, _, errb, _ := graphEnv(t)
	if code := runGraph(context.Background(), e, nil, false); code != 1 || !strings.Contains(errb.String(), "python-315") {
		t.Errorf("code=%d %s", code, errb)
	}
}

func TestGraphBadShipyardScriptsNamed(t *testing.T) {
	var argv []string
	withRunner(t, "a -> b\n", "", 0, &argv)
	e, _, errb, _ := graphEnv(t)
	e.Getenv = func(k string) string {
		if k == "SHIPYARD_SCRIPTS" {
			return "/no/such/dir"
		}
		return ""
	}
	if code := runGraph(context.Background(), e, nil, false); code != 1 || !strings.Contains(errb.String(), "SHIPYARD_SCRIPTS") || !strings.Contains(errb.String(), "/no/such/dir") {
		t.Errorf("code=%d %s", code, errb)
	}
	if argv != nil {
		t.Error("ran anyway")
	}
}

func TestGraphUnknownArgument(t *testing.T) {
	e, _, errb, _ := graphEnv(t)
	if code := runGraph(context.Background(), e, []string{"--bogus"}, false); code != 2 || !strings.Contains(errb.String(), "usage:") {
		t.Errorf("code=%d %s", code, errb)
	}
}
