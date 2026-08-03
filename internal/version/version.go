// Package version holds build-time metadata for bunker and bunkerd.
//
// The values are overridden at link time via -ldflags, e.g.:
//
//	go build -ldflags "-X github.com/deployBunker/bunker/internal/version.Version=v0.2.0 \
//	                   -X github.com/deployBunker/bunker/internal/version.Commit=$(git rev-parse --short HEAD) \
//	                   -X github.com/deployBunker/bunker/internal/version.BuildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
//
// The Makefile build targets apply these flags automatically (VERSION is
// configurable via the VERSION variable, defaulting to 0.1.0).
package version

var (
	// Version is the semantic version of the build.
	Version = "0.1.0"

	// Commit is the git short SHA the binaries were built from.
	// "unknown" when not injected via ldflags.
	Commit = "unknown"

	// BuildDate is the UTC build timestamp (RFC3339).
	// "unknown" when not injected via ldflags.
	BuildDate = "unknown"
)
