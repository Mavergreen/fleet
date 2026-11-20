package retry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Mavergreen/fleet/internal/ghapi"
)

const declDir = "/repos/o/r/contents/.mavergreen/deviations/retryable"

type declFake struct{ routes map[string]any } // path -> JSON value, or error

func (f *declFake) Get(_ context.Context, p string, out any) error {
	v, ok := f.routes[p]
	if !ok {
		return &ghapi.HTTPError{Status: 404, Path: p}
	}
	if err, ok := v.(error); ok {
		return err
	}
	b, _ := json.Marshal(v)
	return json.Unmarshal(b, out)
}
func (f *declFake) GetPaged(context.Context, string, func(json.RawMessage) error) error {
	panic("unused")
}
func (f *declFake) GetRaw(context.Context, string) ([]byte, error) { panic("unused") }
func (f *declFake) Patch(context.Context, string, any, any) error  { panic("unused") }
func (f *declFake) Post(context.Context, string, any, any) error   { panic("unused") }

func addLabel(f *declFake, label string, files map[string]string) {
	list, _ := f.routes[declDir].([]map[string]string)
	list = append(list, map[string]string{"name": label, "type": "dir"})
	f.routes[declDir] = list
	var fl []map[string]string
	for n, c := range files {
		fl = append(fl, map[string]string{"name": n, "type": "file"})
		enc := base64.StdEncoding.EncodeToString([]byte(c))
		// GitHub wraps base64 at 60 columns with newlines.
		enc = enc[:len(enc)/2] + "\n" + enc[len(enc)/2:]
		f.routes[declDir+"/"+label+"/"+n] = map[string]string{"type": "file", "encoding": "base64", "content": enc}
	}
	f.routes[declDir+"/"+label] = fl
}

func goodFiles(sig string) map[string]string {
	return map[string]string{
		"scope":     "release.yml:on-mavericks\n",
		"reason":    "check 6 hangs on the mavericks VM\n",
		"revisit":   "2027-01-01\n",
		"signature": sig,
	}
}

func scriptsDir(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/fake-deviations.sh")
	if err != nil {
		t.Fatal(err)
	}
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "deviations.sh"), b, 0o755); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestNoMavergreenDirMeansNoPatterns(t *testing.T) {
	f := &declFake{routes: map[string]any{}}
	ps, err := LoadDeclared(context.Background(), f, "o", "r", scriptsDir(t))
	if err != nil || len(ps) != 0 {
		t.Fatalf("got %v, %v", ps, err)
	}
}

func TestListingErrorPropagates(t *testing.T) {
	f := &declFake{routes: map[string]any{declDir: &ghapi.HTTPError{Status: 500, Path: declDir}}}
	if _, err := LoadDeclared(context.Background(), f, "o", "r", scriptsDir(t)); err == nil {
		t.Fatal("want error")
	}
}

func TestLoadsClaudeCodeCheck6(t *testing.T) {
	f := &declFake{routes: map[string]any{}}
	addLabel(f, "e2e-check6-hang", goodFiles("check 6: (timed out|hang)\n"))
	ps, err := LoadDeclared(context.Background(), f, "o", "r", scriptsDir(t))
	if err != nil || len(ps) != 1 {
		t.Fatalf("got %v, %v", ps, err)
	}
	p := ps[0]
	if p.ID != "retryable:e2e-check6-hang" || p.Workflow != "release.yml" || p.Job != "on-mavericks" ||
		p.Evidence != "check 6 hangs on the mavericks VM" || p.LogRE == nil || !p.LogRE.MatchString("x check 6: hang") {
		t.Fatalf("bad pattern %+v", p)
	}
}

func TestBadSignatureIsAnError(t *testing.T) {
	for _, sig := range []string{"(unclosed\n", "\n", "", `\d+` + "\n"} {
		f := &declFake{routes: map[string]any{}}
		addLabel(f, "bad", goodFiles(sig))
		_, err := LoadDeclared(context.Background(), f, "o", "r", scriptsDir(t))
		if err == nil || !strings.Contains(err.Error(), "bad") || !strings.Contains(err.Error(), "signature") {
			t.Errorf("sig %q: err %v", sig, err)
		}
	}
}

func TestBadScopeIsAnError(t *testing.T) {
	for _, sc := range []string{"release.yml\n", "a:b:c\n", ":job\n", "wf:\n"} {
		f := &declFake{routes: map[string]any{}}
		fs := goodFiles("zzz one\n")
		fs["scope"] = sc
		addLabel(f, "bad", fs)
		if _, err := LoadDeclared(context.Background(), f, "o", "r", scriptsDir(t)); err == nil {
			t.Errorf("scope %q: want error", sc)
		}
	}
}

func TestMissingFileIsAnError(t *testing.T) {
	f := &declFake{routes: map[string]any{}}
	fs := goodFiles("zzz one\n")
	delete(fs, "revisit")
	addLabel(f, "lbl", fs)
	_, err := LoadDeclared(context.Background(), f, "o", "r", scriptsDir(t))
	if err == nil || !strings.Contains(err.Error(), "lbl") || !strings.Contains(err.Error(), "revisit") {
		t.Fatalf("err %v", err)
	}
}

func TestNonDirLabelIsAnError(t *testing.T) {
	f := &declFake{routes: map[string]any{declDir: []map[string]string{{"name": "stray", "type": "file"}}}}
	if _, err := LoadDeclared(context.Background(), f, "o", "r", scriptsDir(t)); err == nil {
		t.Fatal("want error")
	}
}

func TestDeviationsShFailureIsAnError(t *testing.T) {
	f := &declFake{routes: map[string]any{}}
	addLabel(f, "lbl", goodFiles("zzz one\n"))
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "deviations.sh"), []byte("#!/bin/sh\necho boom-detail >&2\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := LoadDeclared(context.Background(), f, "o", "r", d)
	if err == nil || !strings.Contains(err.Error(), "boom-detail") {
		t.Fatalf("want error carrying stderr, got %v", err)
	}
}

func TestDeviationsShOmittingALabelIsAnError(t *testing.T) {
	f := &declFake{routes: map[string]any{}}
	addLabel(f, "e2e-check6-hang", goodFiles("zzz one\n"))
	om := goodFiles("zzz two\n")
	om["scope"] = "other.yml:job\n" // distinct scope, so the scope-match rule can tell it is missing
	addLabel(f, "omit-me", om)
	_, err := LoadDeclared(context.Background(), f, "o", "r", scriptsDir(t))
	if err == nil || !strings.Contains(err.Error(), "deviations.sh did not accept omit-me") {
		t.Fatalf("err %v", err)
	}
}

func TestEmptyMatchingSignatureIsAnError(t *testing.T) {
	f := &declFake{routes: map[string]any{}}
	addLabel(f, "broad", goodFiles(".*\n"))
	_, err := LoadDeclared(context.Background(), f, "o", "r", scriptsDir(t))
	if err == nil || !strings.Contains(err.Error(), "broad") || !strings.Contains(err.Error(), "too broad") {
		t.Fatalf("err %v", err)
	}
}

func TestSignatureMatchingTheGenericCloserIsAnError(t *testing.T) {
	for _, sig := range []string{".\n", "exit code\n", "error\n"} {
		f := &declFake{routes: map[string]any{}}
		addLabel(f, "broad", goodFiles(sig))
		_, err := LoadDeclared(context.Background(), f, "o", "r", scriptsDir(t))
		if err == nil || !strings.Contains(err.Error(), "too broad") {
			t.Errorf("sig %q: err %v", sig, err)
		}
	}
}

func TestSharedScopeOmissionIsAnError(t *testing.T) {
	f := &declFake{routes: map[string]any{}}
	addLabel(f, "e2e-check6-hang", goodFiles("zzz one\n"))
	addLabel(f, "omit-me", goodFiles("zzz two\n")) // same scope as the first
	_, err := LoadDeclared(context.Background(), f, "o", "r", scriptsDir(t))
	if err == nil || !strings.Contains(err.Error(), "deviations.sh did not accept") {
		t.Fatalf("err %v", err)
	}
}

func TestMissingScriptsIsAnErrorOnlyWhenNeeded(t *testing.T) {
	f := &declFake{routes: map[string]any{}}
	if ps, err := LoadDeclared(context.Background(), f, "o", "r", ""); err != nil || len(ps) != 0 {
		t.Fatalf("a repo with no declarations needs no scripts: %v, %v", ps, err)
	}
	addLabel(f, "check-6", goodFiles("computer-use server never started\n"))
	if _, err := LoadDeclared(context.Background(), f, "o", "r", ""); err == nil || !strings.Contains(err.Error(), "shipyard scripts not found") {
		t.Fatalf("want a not-found error, got %v", err)
	}
}
