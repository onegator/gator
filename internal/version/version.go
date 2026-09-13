// Package version carries build metadata injected by the linker.
package version

// Set via -ldflags "-X github.com/onegator/gator/internal/version.Version=..." at build time.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)
