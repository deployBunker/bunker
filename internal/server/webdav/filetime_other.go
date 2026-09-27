//go:build !linux

package webdav

import "os"

// ctimeUnixNano has no change time to report where the file-info system type
// does not expose one, so the identity an observation records on this platform
// is (size, mtime) — a weaker identity, and one that cannot see an edit which
// preserves both. That gap is real and is stated rather than papered over: the
// invalidation channel is a metadata observer, and on these platforms it misses
// the same-size, mtime-restored edit that the write path's content-hash
// re-validation (BFS-004 §6.1 step 5) refuses on its own evidence.
//
// bunkerd's served target is Linux (the release's platforms are linux/amd64 and
// linux/arm64); this file exists so the surface still BUILDS for the Windows and
// survey targets BFS-010's cross-GOOS guard requires.
func ctimeUnixNano(os.FileInfo) int64 { return 0 }
