// Package auth finds the GitHub token. The token is never stored or printed.
package auth

import (
	"errors"
	"strings"
)

// Token returns GH_TOKEN if set, else the output of `gh auth token`.
func Token(getenv func(string) string, run func(name string, args ...string) ([]byte, error)) (string, error) {
	if t := strings.TrimSpace(getenv("GH_TOKEN")); t != "" {
		return t, nil
	}
	if out, err := run("gh", "auth", "token"); err == nil {
		if t := strings.TrimSpace(string(out)); t != "" {
			return t, nil
		}
	}
	return "", errors.New("no GitHub token: set GH_TOKEN, or run `gh auth login`")
}
