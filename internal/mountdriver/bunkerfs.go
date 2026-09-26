// Package mountdriver: the bunker-fs driver registration (BFS-008).
//
// bunker-fs is OPT-IN and is NOT the default: DefaultDriver stays "sshfs" until
// a battery on both DCs shows zero stalls (PRD-bunker-fs, the release rule this
// row is held to). Registering it here is what lets a caller ASK for it by name
// (`--driver bunkerfs`) without changing what a request that names no driver
// gets.
//
// The three mount-driver rules this driver must satisfy, and where each is met:
//
//  1. NO NEW SERVER-SIDE REQUIREMENT. The driver talks to the surface bunkerd
//     already serves (`internal/server/webdav`, BFS-004/BFS-006). Its only
//     mandatory interaction is a standards request: `OPTIONS`. Every X-Bunker-*
//     extension it uses is OPTIONAL and has a standard-protocol fallback
//     (X-Bunker-Op: snapshot degrades to PROPFIND Depth: 1; the watch channel
//     degrades to a declared revision poll), and HTTP/1.1 is fully supported —
//     nothing here needs h2 or h3.
//  2. NEVER DEFAULT. See above; nothing in this file touches DefaultDriver.
//  3. ITS OWN OPTIONS AND ITS OWN DURABILITY/FAILURE CLASSIFIER. The options are
//     declared below and implemented in internal/fsmount (this driver shares no
//     option with sshfs and inherits none of sshfs's retry logic); the classifier
//     is classifyBunkerFS, which reads bunker-fs's OWN verdicts (bind refusal,
//     named cause) rather than sshfs's stderr fragments.
package mountdriver

import (
	"errors"
	"io"
	"strings"
	"syscall"
)

// DriverBunkerFS is the driver identity carried in proto MountSpec.driver for a
// bunker-fs mount.
const DriverBunkerFS = "bunkerfs"

// BunkerFSOption describes one of this driver's own options. It exists so a
// caller (and the docs gate) can read the surface without importing
// internal/fsmount, and so "this driver carries its own options" is a fact with
// a name rather than a claim.
type BunkerFSOption struct {
	Name    string
	Default string
	Help    string
}

// BunkerFSOptions is the driver's own option set, verbatim from the client spec's
// chosen defaults (BFS-005 §9). No sshfs option appears here, and no option here
// is inherited by another driver.
var BunkerFSOptions = []BunkerFSOption{
	{"url", "", "WebDAV surface root (required)"},
	{"cache-max-size", "268435456", "hard cap on cache bytes on this client (0 = disabled)"},
	{"cache-max-entry-bytes", "67108864", "a single file larger than this is never cached"},
	{"cache-max-age", "1h", "backstop TTL; invalidation, not age, is the mechanism"},
	{"concurrency", "25", "maximum requests in flight — the measured lever"},
	{"max-conns-per-host", "0", "transport connection cap (0 = follow concurrency; 1 reproduces the serial arm)"},
	{"invalidation", "auto", "auto|push|poll"},
	{"poll-interval", "2s", "declared poll period"},
	{"on-conflict", "refuse", "refuse|overwrite-if-unchanged"},
	{"write-buffer-max-bytes", "268435456", "bound on one open write handle's local buffer"},
	{"allow-other", "false", "STRIPPED: accepted and ignored, the mountpoint stays private 0700"},
}

// bunkerFSPermanentFragments are the lowercased fragments of a bunker-fs bind
// refusal that retrying cannot fix: the credential was refused, the tree identity
// is no longer the bound one, or the local configuration is wrong.
var bunkerFSPermanentFragments = []string{
	"unauthenticated",
	"unauthorized",
	"forbidden",
	"workspace_invalid",
	"stale_identity",
	"stale_tree",
	"mountpoint must be private",
	"unknown capability document",
	"no fuse binding on this platform",
}

// bunkerFSTransientFragments are the transport shapes worth one retry: the far
// end is unreachable, slow, or reset the stream. They are bunker-fs's OWN named
// causes, not sshfs's stderr text.
var bunkerFSTransientFragments = []string{
	"unreachable_connect",
	"unreachable_deadline",
	"unreachable_reset",
	"connection refused",
	"connection reset",
	"deadline exceeded",
	"i/o timeout",
}

// classifyBunkerFS classifies a failed bunker-fs mount attempt. Permanent causes
// win: retrying an auth refusal or a re-created tree only adds noise, and a
// re-created tree additionally demands an explicit re-bind rather than a retry.
func classifyBunkerFS(output string, err error) (ClassifierClass, string) {
	lower := strings.ToLower(output)
	for _, frag := range bunkerFSPermanentFragments {
		if strings.Contains(lower, frag) {
			return ClassPermanent, frag
		}
	}
	for _, frag := range bunkerFSTransientFragments {
		if strings.Contains(lower, frag) {
			return ClassTransient, frag
		}
	}
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return ClassTransient, "unexpected EOF"
		}
		if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ETIMEDOUT) {
			return ClassTransient, err.Error()
		}
	}
	return ClassUnknown, ""
}

func init() {
	Register(Driver{
		Name:     DriverBunkerFS,
		Classify: classifyBunkerFS,
	})
}
