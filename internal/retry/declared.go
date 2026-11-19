package retry

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Mavergreen/fleet/internal/ghapi"
)

const declaredPath = ".mavergreen/deviations/retryable"

// genericClosers are lines every failed job emits; a signature matching one
// would make every failure of its job retryable.
var genericClosers = []string{
	"##[error]Process completed with exit code 1.",
	"##[error]Process completed with exit code 2.",
}

var declaredFiles = []string{"scope", "reason", "revisit", "signature"}

type contentEntry struct {
	Name, Type, Encoding, Content string
}

// LoadDeclared reads the retryable deviations a repo declares in its own tree.
// No such directory means no repo-specific patterns. Anything unreadable or
// invalid is an error, never a skipped pattern.
func LoadDeclared(ctx context.Context, c ghapi.Client, org, repo, scripts string) ([]Pattern, error) {
	base := fmt.Sprintf("/repos/%s/%s/contents/%s", org, repo, declaredPath)
	var entries []contentEntry
	if err := c.Get(ctx, base, &entries); err != nil {
		var he *ghapi.HTTPError
		if errors.As(err, &he) && he.Status == 404 {
			return nil, nil
		}
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}
	tmp, err := os.MkdirTemp("", "fleet-declared-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	type label struct {
		name, scope string
		p           Pattern
	}
	var labels []label
	for _, e := range entries {
		if e.Type != "dir" {
			return nil, fmt.Errorf("%s/%s: expected a directory, found %q", declaredPath, e.Name, e.Type)
		}
		if e.Name == "" || strings.ContainsAny(e.Name, "/\\") || e.Name == "." || e.Name == ".." {
			return nil, fmt.Errorf("%s: invalid label name %q", declaredPath, e.Name)
		}
		dir := filepath.Join(tmp, filepath.FromSlash(declaredPath), e.Name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		got := map[string]string{}
		for _, f := range declaredFiles {
			rel := path.Join(declaredPath, e.Name, f)
			var fe contentEntry
			if err := c.Get(ctx, "/repos/"+org+"/"+repo+"/contents/"+rel, &fe); err != nil {
				return nil, fmt.Errorf("label %s, file %s: %w", e.Name, f, err)
			}
			if fe.Type != "file" || fe.Encoding != "base64" {
				return nil, fmt.Errorf("label %s, file %s: not a base64 file", e.Name, f)
			}
			raw, err := base64.StdEncoding.DecodeString(strings.NewReplacer("\n", "", "\r", "").Replace(fe.Content))
			if err != nil {
				return nil, fmt.Errorf("label %s, file %s: %w", e.Name, f, err)
			}
			s := strings.TrimSuffix(string(raw), "\n")
			got[f] = s
			if err := os.WriteFile(filepath.Join(dir, f), raw, 0o644); err != nil {
				return nil, err
			}
		}
		wf, job, ok := strings.Cut(got["scope"], ":")
		if !ok || wf == "" || job == "" || strings.Contains(job, ":") {
			return nil, fmt.Errorf("label %s, file scope: %q is not <workflow file>:<job>", e.Name, got["scope"])
		}
		if got["signature"] == "" {
			return nil, fmt.Errorf("label %s, file signature: empty", e.Name)
		}
		re, err := regexp.CompilePOSIX(got["signature"])
		if err != nil {
			return nil, fmt.Errorf("label %s, file signature: %w", e.Name, err)
		}
		if re.MatchString("") {
			return nil, fmt.Errorf("label %s, file signature: %q is too broad (matches the empty string)", e.Name, got["signature"])
		}
		for _, closer := range genericClosers {
			if re.MatchString(closer) {
				return nil, fmt.Errorf("label %s, file signature: %q is too broad (matches %q, which every failed job emits)", e.Name, got["signature"], closer)
			}
		}
		labels = append(labels, label{e.Name, got["scope"], Pattern{
			ID: "retryable:" + e.Name, Workflow: wf, Job: job, LogRE: re, Evidence: got["reason"],
		}})
	}

	out, err := exec.CommandContext(ctx, "sh", filepath.Join(scripts, "deviations.sh"), tmp).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("deviations.sh: %w: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("deviations.sh: %w", err)
	}
	accepted := map[string]int{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "retryable" {
			accepted[f[1]]++
		}
	}
	var ps []Pattern
	for _, l := range labels {
		// One printed line per label: labels sharing a scope each need their own.
		if accepted[l.scope] == 0 {
			return nil, fmt.Errorf("deviations.sh did not accept %s (scope %s)", l.name, l.scope)
		}
		accepted[l.scope]--
		ps = append(ps, l.p)
	}
	return ps, nil
}
