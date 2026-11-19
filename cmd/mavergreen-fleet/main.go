// Command mavergreen-fleet watches over the Mavergreen GitHub org.
package main

import (
	"os"
	"time"

	"github.com/Mavergreen/fleet/internal/cli"
)

// version is set with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	isTTY := false
	if fi, err := os.Stdout.Stat(); err == nil {
		isTTY = fi.Mode()&os.ModeCharDevice != 0
	}
	os.Exit(cli.Run(os.Args[1:], cli.Env{
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Getenv:  os.Getenv,
		IsTTY:   isTTY,
		Now:     time.Now,
		Version: version,
	}))
}
