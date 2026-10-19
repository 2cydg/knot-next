package buildinfo

import "fmt"

// Set through -ldflags at release build time.
var (
	Version   = "0.1.0-dev"
	Commit    = "unknown"
	BuildTime = "unknown"
	Dirty     = "false"
)

func String() string {
	return fmt.Sprintf("knot %s (commit=%s, built=%s, dirty=%s)", Version, Commit, BuildTime, Dirty)
}
