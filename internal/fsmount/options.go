// Package fsmount is the bunker-fs FUSE binding: it hands the client
// (internal/fsclient) to the kernel as a filesystem.
//
// PLATFORM SEAM (BFS-010). Everything platform-specific lives in this package's
// per-OS files and nowhere else:
//
//   - fs_linux.go        (//go:build linux)  — the go-fuse node tree and Mount().
//     BFS-003 §2 chose go-fuse (pure Go, CGO_ENABLED=0) for Linux.
//   - fs_unsupported.go  (//go:build !linux) — Mount() refuses with a named,
//     non-silent error pointing at BFS-010 (WinFsp driven from Go through
//     cgofuse), rather than failing to compile or pretending to mount.
//
// Everything in THIS file is OS-neutral on purpose — options, the mountpoint
// posture, the mount identity and the status document — so a future Windows
// driver reuses it verbatim and only swaps the binding. The client
// (internal/fsclient) contains no FUSE and no platform constant beyond the named
// errno table, which is what makes the same caching, invalidation, conflict and
// delegation behaviour available to the Windows driver unchanged.
package fsmount

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/deployBunker/bunker/internal/fsclient"
)

// The mount's own defaults. Each is the client spec's chosen default, restated
// here because the CLI reads them from this package rather than from a second
// copy of the numbers.
const (
	DefaultCacheMaxBytes      int64 = fsclient.DefaultCacheMaxBytes
	DefaultCacheMaxEntryBytes int64 = fsclient.DefaultCacheMaxEntryBytes
	DefaultConcurrency              = fsclient.DefaultConcurrency
	DefaultWriteBufferMax     int64 = fsclient.DefaultCacheMaxBytes
	DefaultCacheMaxAge              = fsclient.DefaultCacheMaxAge
	DefaultPollInterval             = fsclient.DefaultPollInterval
)

// Options configures one mount. Every field is this driver's OWN option set:
// the third mount-driver rule is that a driver carries its own options, its own
// durability policy and its own failure classifier rather than inheriting
// sshfs-shaped defaults.
type Options struct {
	// Mountpoint is the local directory to mount on. It is created mode 0700
	// and an existing wider mode is tightened — the mountpoint is PRIVATE.
	Mountpoint string
	// BaseURL is the WebDAV surface root, e.g. http://127.0.0.1:18481/dav.
	BaseURL string
	// Username/Password are optional HTTP Basic credentials.
	Username string
	Password string

	// CacheMaxBytes is the hard cap on cache bytes on disk (0 disables).
	CacheMaxBytes int64
	// CacheMaxEntryBytes caps one cached file.
	CacheMaxEntryBytes int64
	// CacheMaxAge is the backstop TTL.
	CacheMaxAge time.Duration
	// Concurrency is the maximum number of requests in flight. It is THE lever:
	// 25× concurrency measured 0.79 s against 38.47 s at MaxConnsPerHost=1.
	Concurrency int
	// MaxConnsPerHost overrides the transport's per-host connection cap, so the
	// concurrency lever can be measured exactly as the study measured it.
	MaxConnsPerHost int

	// CacheDir overrides the derived cache directory.
	CacheDir string

	// Invalidation is auto|push|poll.
	Invalidation string
	// PollInterval is the declared poll period.
	PollInterval time.Duration
	// OnConflict is refuse (default) or overwrite-if-unchanged.
	OnConflict string

	// AllowOther is the requested allow_other. It is STRIPPED, never honoured:
	// the field exists so the CLI can say it was stripped rather than
	// silently ignoring the flag.
	AllowOther bool

	// Snapshot, when false, skips the one-call node-tree snapshot and lets every
	// directory read fall back to the standard PROPFIND path. It exists as a
	// control arm for the measurement, not as a supported operating mode.
	Snapshot bool

	// OpTimeout / BindTimeout override the client deadlines (tests).
	OpTimeout   time.Duration
	BindTimeout time.Duration

	// Logf receives the mount's diagnostic lines (default: no output).
	Logf func(format string, args ...any)
}

// ErrPlatformUnsupported is returned by Mount on a platform this build has no
// binding for. It is a named refusal, not a silent no-op: BFS-010 is the row
// that delivers Windows (WinFsp via cgofuse).
var ErrPlatformUnsupported = errors.New("bunker-fs: no FUSE binding on this platform (BFS-010 delivers Windows via WinFsp/cgofuse)")

// ErrPrivateMountpoint is returned when the mountpoint cannot be made private.
var ErrPrivateMountpoint = errors.New("bunker-fs: mountpoint must be private (0700)")

// Normalize fills the defaults and validates the option set. It performs no I/O.
func (o *Options) Normalize() error {
	if strings.TrimSpace(o.Mountpoint) == "" {
		return errors.New("bunker-fs: a mountpoint is required")
	}
	if strings.TrimSpace(o.BaseURL) == "" {
		return errors.New("bunker-fs: an endpoint URL is required (--url)")
	}
	if o.CacheMaxBytes == 0 {
		// 0 from an unset field means "the default", not "disabled": the
		// disabled case is stated explicitly by --no-cache.
		o.CacheMaxBytes = DefaultCacheMaxBytes
	}
	if o.CacheMaxBytes < 0 {
		o.CacheMaxBytes = 0 // negative == disabled, same as --no-cache
	}
	if o.CacheMaxEntryBytes <= 0 {
		o.CacheMaxEntryBytes = o.CacheMaxBytes
		if o.CacheMaxEntryBytes == 0 || o.CacheMaxEntryBytes > DefaultCacheMaxEntryBytes {
			o.CacheMaxEntryBytes = DefaultCacheMaxEntryBytes
		}
	}
	if o.CacheMaxAge <= 0 {
		o.CacheMaxAge = DefaultCacheMaxAge
	}
	if o.Concurrency <= 0 {
		o.Concurrency = DefaultConcurrency
	}
	if o.PollInterval <= 0 {
		o.PollInterval = DefaultPollInterval
	}
	switch o.Invalidation {
	case "", "auto":
		o.Invalidation = "auto"
	case "push", "poll":
	default:
		return fmt.Errorf("bunker-fs: --invalidation must be auto, push or poll (got %q)", o.Invalidation)
	}
	switch o.OnConflict {
	case "":
		o.OnConflict = fsclient.OnConflictRefuse
	case fsclient.OnConflictRefuse, fsclient.OnConflictOverwriteIfUnchanged:
	default:
		return fmt.Errorf("bunker-fs: --on-conflict must be %s or %s (got %q)",
			fsclient.OnConflictRefuse, fsclient.OnConflictOverwriteIfUnchanged, o.OnConflict)
	}
	if o.OpTimeout <= 0 {
		o.OpTimeout = fsclient.DefaultOpTimeout
	}
	if o.BindTimeout <= 0 {
		o.BindTimeout = fsclient.DefaultBindTimeout
	}
	abs, err := filepath.Abs(o.Mountpoint)
	if err != nil {
		return fmt.Errorf("bunker-fs: resolve mountpoint: %w", err)
	}
	o.Mountpoint = abs
	return nil
}

// PrepareMountpoint creates the mountpoint private and returns whether an
// existing wider mode had to be tightened. `allow_other` is STRIPPED, not
// refused (the release rule): the caller is told the flag was ignored, and the
// mount proceeds, because a stripped request is a strictly safer mount than the
// one asked for — whereas a refusal would send the user to chmod 0777.
func PrepareMountpoint(dir string) (tightened bool, err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, fmt.Errorf("bunker-fs: create mountpoint %s: %w", dir, err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return false, fmt.Errorf("bunker-fs: stat mountpoint %s: %w", dir, err)
	}
	if !fi.IsDir() {
		return false, fmt.Errorf("bunker-fs: mountpoint %s is not a directory", dir)
	}
	if fi.Mode().Perm()&0o077 == 0 {
		return false, nil
	}
	if err := os.Chmod(dir, fi.Mode().Perm()&0o700|0o700); err != nil {
		return false, fmt.Errorf("%w: chmod %s: %v", ErrPrivateMountpoint, dir, err)
	}
	return true, nil
}

// MountID returns the stable identity of one endpoint's mount.
func MountID(baseURL string) string { return fsclient.MountID(baseURL) }

// MountDir returns the resolved cache directory for one mount: the explicit
// override when set, otherwise $XDG_CACHE_HOME/bunker/fs/<mount-id>.
func MountDir(o Options) (string, error) {
	if o.CacheDir != "" {
		return o.CacheDir, nil
	}
	return fsclient.MountDir(o.BaseURL)
}

// AllowedOtherStripped is the sentence the CLI prints when --allow-other was
// requested, so the stripping is a visible fact rather than a silent one.
func AllowedOtherStripped() string {
	return "bunker-fs: --allow-other is STRIPPED (the mountpoint stays private 0700); " +
		"the flag is ignored rather than refused, because a private mount is strictly safer than the one requested"
}
