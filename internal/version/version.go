// Package version holds build-time metadata for bunker and bunkerd.
//
// The values are overridden at link time via -ldflags, e.g.:
//
//	go build -ldflags "-X github.com/deployBunker/bunker/internal/version.Version=v0.2.0 \
//	                   -X github.com/deployBunker/bunker/internal/version.Commit=$(git rev-parse --short HEAD) \
//	                   -X github.com/deployBunker/bunker/internal/version.BuildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
//
// The Makefile build targets apply these flags automatically (VERSION is
// configurable via the VERSION variable, defaulting to 0.1.3).
//
// When ldflags values are absent (e.g. `go install pkg@version`, the README's
// primary install path, or a bare `go build ./cmd/bunker`), init() falls back
// to the VCS metadata embedded by the Go toolchain (debug.ReadBuildInfo):
// vcs.revision supplies Commit and vcs.time supplies BuildDate. For module
// installs without VCS metadata (module-proxy zips), the module version
// (e.g. v0.1.3) is used as the Commit fallback so the field is never a bare
// "unknown" for a tagged release (GAP-042).
package version

import "runtime/debug"

var (
	// Version is the semantic version of the build.
	Version = "0.1.3"

	// Commit is the git short SHA the binaries were built from.
	// "unknown" when neither injected via ldflags nor derivable from
	// the embedded build info.
	Commit = "unknown"

	// BuildDate is the UTC build timestamp (RFC3339).
	// "unknown" when neither injected via ldflags nor derivable from
	// the embedded build info.
	BuildDate = "unknown"
)

func init() {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	commit, buildDate := deriveFromBuildInfo(bi.Settings, bi.Main.Version)
	if Commit == "unknown" && commit != "" {
		Commit = commit
	}
	if BuildDate == "unknown" && buildDate != "" {
		BuildDate = buildDate
	}
}

// deriveFromBuildInfo extracts a short commit and build timestamp from the
// settings the Go toolchain embeds into the binary. It returns empty strings
// for fields the build did not record.
func deriveFromBuildInfo(settings []debug.BuildSetting, mainVersion string) (commit, buildDate string) {
	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			if s.Value != "" {
				commit = shortRev(s.Value)
			}
		case "vcs.time":
			if s.Value != "" {
				buildDate = s.Value
			}
		}
	}
	// Module installs from the module proxy have no VCS settings; the
	// module version (v0.1.3) is the only provenance the binary carries.
	if commit == "" && mainVersion != "" && mainVersion != "(devel)" {
		commit = mainVersion
	}
	return commit, buildDate
}

// shortRev shortens a full VCS revision to git's default short form (7
// chars), matching what `git rev-parse --short HEAD` produces in the
// Makefile ldflags.
func shortRev(rev string) string {
	if len(rev) > 7 {
		return rev[:7]
	}
	return rev
}
