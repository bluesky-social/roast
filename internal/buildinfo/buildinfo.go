// Package buildinfo carries version metadata stamped at release build time
// via ldflags. In development builds all three vars default to sentinel
// values so callers can detect an unversioned binary.
package buildinfo

import "fmt"

// Version is the semver tag (e.g. "v0.4.2"). Set via
//
//	-ldflags "-X github.com/bluesky-social/roast/internal/buildinfo.Version=v0.1.0"
var Version = "dev"

// Commit is the git commit hash (short). Set via ldflags.
var Commit = "unknown"

// Date is the build date (RFC 3339). Set via ldflags.
var Date = "unknown"

// String returns a one-line human-readable version string.
// In development builds: "dev (commit unknown)"
// In release builds:     "v0.1.0 (abc1234, 2026-09-19)"
func String() string {
	if Version == "dev" {
		return fmt.Sprintf("dev (commit %s)", Commit)
	}
	return fmt.Sprintf("%s (%s, %s)", Version, Commit, Date)
}
