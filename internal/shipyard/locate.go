// Package shipyard locates the shipyard build scripts.
package shipyard

import "fmt"

const installed = "/usr/local/mavergreen/shipyard/share/cmake/MavericksShipyard/scripts"

// Scripts returns $SHIPYARD_SCRIPTS if set, else the installed location.
func Scripts(getenv func(string) string, isDir func(string) bool) (string, error) {
	if p := getenv("SHIPYARD_SCRIPTS"); p != "" && isDir(p) {
		return p, nil
	}
	if isDir(installed) {
		return installed, nil
	}
	return "", fmt.Errorf("shipyard scripts not found: set SHIPYARD_SCRIPTS or install shipyard at %s", installed)
}
