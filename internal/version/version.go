// Package version holds build-time version information injected via -ldflags.
package version

var (
	// Version is the semantic version of the build.
	Version = "dev"
	// Commit is the source revision of the build.
	Commit = "unknown"
	// BuildTime is the UTC build timestamp.
	BuildTime = "unknown"
)
