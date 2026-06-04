// Package version holds build-time version information populated via ldflags.
package version

// Version is the semantic version string, set at build time.
var Version = "dev"

// Commit is the git commit SHA, set at build time.
var Commit = "unknown"

// Date is the build timestamp in RFC3339 format, set at build time.
var Date = "unknown"
