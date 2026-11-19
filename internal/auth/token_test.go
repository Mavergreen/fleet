package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestTokenPrefersGHTOKEN(t *testing.T) {
	getenv := func(k string) string {
		if k == "GH_TOKEN" {
			return "envtok"
		}
		return ""
	}
	run := func(string, ...string) ([]byte, error) { t.Fatal("gh should not run"); return nil, nil }
	got, err := Token(getenv, run)
	if err != nil || got != "envtok" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestTokenFallsBackToGhAuthToken(t *testing.T) {
	run := func(name string, args ...string) ([]byte, error) {
		if name != "gh" || strings.Join(args, " ") != "auth token" {
			t.Fatalf("ran %s %v", name, args)
		}
		return []byte("abc\n"), nil
	}
	got, err := Token(func(string) string { return "" }, run)
	if err != nil || got != "abc" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestTokenErrorNamesBothSources(t *testing.T) {
	run := func(string, ...string) ([]byte, error) { return nil, errors.New("no gh") }
	_, err := Token(func(string) string { return "" }, run)
	if err == nil || !strings.Contains(err.Error(), "GH_TOKEN") || !strings.Contains(err.Error(), "gh auth login") {
		t.Fatalf("err=%v", err)
	}
}
