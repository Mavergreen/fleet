package shipyard

import (
	"strings"
	"testing"
)

const wantInstalled = "/usr/local/mavergreen/shipyard/share/cmake/MavericksShipyard/scripts"

func TestScriptsFromEnv(t *testing.T) {
	getenv := func(k string) string {
		if k == "SHIPYARD_SCRIPTS" {
			return "/x/scripts"
		}
		return ""
	}
	got, err := Scripts(getenv, func(p string) bool { return p == "/x/scripts" })
	if err != nil || got != "/x/scripts" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestScriptsInstalledPath(t *testing.T) {
	got, err := Scripts(func(string) string { return "" }, func(p string) bool { return p == wantInstalled })
	if err != nil || got != wantInstalled {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestScriptsErrorNamesBoth(t *testing.T) {
	_, err := Scripts(func(string) string { return "" }, func(string) bool { return false })
	if err == nil || !strings.Contains(err.Error(), "SHIPYARD_SCRIPTS") || !strings.Contains(err.Error(), wantInstalled) {
		t.Fatalf("err=%v", err)
	}
}
