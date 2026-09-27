// Package fsmount is the bunker-fs FUSE binding: it hands the client
// (internal/fsclient) to the kernel as a filesystem.
//
// PLATFORM SEAM (BFS-010). Everything platform-specific lives in this package's
// per-OS files and nowhere else:
//
//   - fs_linux.go        (//go:build linux)  — the go-fuse node tree and Mount().
//     BFS-003 §2 chose go-fuse (pure Go, CGO_ENABLED=0) for Linux.
//   - fs_unsupported.go  (//go:build !linux) — MountAt() refuses with a named,
//     non-silent error naming the platform, rather than failing to compile or
//     pretending to mount. A Windows binding (decided: WinFsp driven from Go
//     through cgofuse, docs/evidence/BFS-010-windows-mint-decision.md) plugs in
//     here; nothing in this build implements it.
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
	// DefaultCacheMaxEntries is the cache directory's ENTRY bound. It is a
	// SEPARATE bound from the byte one because a byte bound alone does not bound
	// a directory (BFS-031: at a 1 KiB byte bound the directory reached
	// 30,689 B), and because a tree of tiny files reaches the entry bound while
	// the byte figure still reads comfortably low. BFS-038 landed the
	// enforcement; BFS-044 exposes it as `--cache-max-entries` and validates it.
	DefaultCacheMaxEntries        = fsclient.DefaultCacheMaxEntries
	DefaultCacheMaxInFlight       = fsclient.DefaultCacheMaxInFlight
	DefaultConcurrency            = fsclient.DefaultConcurrency
	DefaultWriteBufferMax   int64 = fsclient.DefaultCacheMaxBytes
	DefaultCacheMaxAge            = fsclient.DefaultCacheMaxAge
	DefaultPollInterval           = fsclient.DefaultPollInterval
	// DefaultInvalidateIdleTimeout is ZERO, and zero is not "unset" here: it is
	// the Invalidator's own documented sentinel for DERIVE the silence deadline
	// from the period the server's capability document declares — three missed
	// heartbeats of it, which is DefaultIdleTimeout when the document names
	// none (BFS-041 §8.1/§8.3). A non-zero value is the mount's own declared
	// bound. Either way the ARMED deadline is reported in the status document's
	// invalidation block, so the derived value is visible rather than inferred.
	DefaultInvalidateIdleTimeout time.Duration = 0
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
	// CacheMaxEntries is the cache directory's ENTRY bound — the second bound,
	// because a byte bound alone does not bound a directory (BFS-031). 0 means
	// DefaultCacheMaxEntries.
	CacheMaxEntries int
	// CacheMaxInFlight is how many staged (unpublished) blobs may hold bytes at
	// once, which is what makes the in-flight reservation a bound rather than a
	// hope (BFS-038). 0 means DefaultCacheMaxInFlight.
	CacheMaxInFlight int
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
	// InvalidateIdleTimeout is how long the pushed channel may be silent before
	// the mount declares it dead and falls back to the poll. ZERO — the
	// default — DERIVES it from the server's declared heartbeat period
	// (BFS-041 §8.1: three missed heartbeats), which is the documented contract
	// of the invalidator's own option rather than a silent fallback, and the
	// armed deadline is reported in the status document either way.
	InvalidateIdleTimeout time.Duration
	// OnConflict is refuse (default) or overwrite-if-unchanged.
	OnConflict string

	// Hot is the client-side hot-file policy (BFS-044). The zero value means
	// "the defaults"; a PARTIALLY populated policy is refused rather than
	// completed silently, so no knob can end up obeying a value the operator
	// did not ask for. Every field is validated in Normalize.
	Hot fsclient.HotPolicy

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

// ErrPlatformUnsupported is returned by MountAt on a platform this build has no
// binding for. It is a named refusal, not a silent no-op.
//
// ONE PHRASE IN IT IS LOAD-BEARING: "no fuse binding on this platform" is the
// fragment internal/mountdriver classifies as a PERMANENT mount failure
// (bunkerFSpermanentFragments, internal/mountdriver/bunkerfs.go). Only
// "transient" is retried by the mount loop (internal/mountdriver/mountdriver.go:
// the FailureClassifier doc), so a reworded sentence would not be retried either —
// it would fall through to the classifier's `unknown` default, whose stated meaning
// is "the output carried no recognised signal". That is the difference this phrase
// buys: an ANSWER ("retrying cannot help") instead of an absence of one. Reword this
// sentence and that table has to change with it;
// internal/mountdriver/platform_refusal_test.go asserts the coupling and fails on
// exactly that mutation.
//
// The message states the situation and the way out; it does NOT promise a
// delivery. The Windows decision — WinFsp driven from Go through cgofuse, opt-in,
// sshfs stays the default until it lands — is recorded with its costs and
// verification plan in docs/evidence/BFS-010-windows-mint-decision.md and
// implements nothing.
var ErrPlatformUnsupported = errors.New("bunker-fs: no FUSE binding on this platform (use the default sshfs driver, or a stock WebDAV client, until a Windows binding lands)")

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
	// The ENTRY bound (BFS-031/BFS-044). Zero means unset and takes the
	// default; a negative value is refused rather than repaired, because
	// "negative" is not a way to say "no bound": the entry bound is always in
	// force, on the measured ground that a byte bound alone does not bound a
	// directory.
	if o.CacheMaxEntries < 0 {
		return fmt.Errorf("bunker-fs: --cache-max-entries must be >= 1 (got %d); 0 means the default %d. A byte bound alone does not bound a directory (BFS-031), so there is no value of this flag that turns the entry bound off", o.CacheMaxEntries, DefaultCacheMaxEntries)
	}
	if o.CacheMaxEntries == 0 {
		o.CacheMaxEntries = DefaultCacheMaxEntries
	}
	if o.CacheMaxInFlight < 0 {
		return fmt.Errorf("bunker-fs: --cache-max-inflight must be >= 1 (got %d); 0 means the default %d, and the width of the staged-refresh window is what makes its byte reservation a bound (BFS-038)", o.CacheMaxInFlight, DefaultCacheMaxInFlight)
	}
	if o.CacheMaxInFlight == 0 {
		o.CacheMaxInFlight = DefaultCacheMaxInFlight
	}
	if o.CacheMaxAge <= 0 {
		o.CacheMaxAge = DefaultCacheMaxAge
	}
	if o.Concurrency < 0 {
		return fmt.Errorf("bunker-fs: --concurrency must be >= 1 (got %d); 0 means the default %d, and a negative pool is not a pool", o.Concurrency, DefaultConcurrency)
	}
	if o.Concurrency == 0 {
		o.Concurrency = DefaultConcurrency
	}
	if o.PollInterval < 0 {
		return fmt.Errorf("bunker-fs: --poll-interval must be > 0 (got %s); 0 means the declared default %s, and a negative period would make the poll's own cadence a lie", o.PollInterval, DefaultPollInterval)
	}
	if o.PollInterval == 0 {
		o.PollInterval = DefaultPollInterval
	}
	if o.InvalidateIdleTimeout < 0 {
		return fmt.Errorf("bunker-fs: --invalidate-idle-timeout must be >= 0 (got %s); 0 means DERIVE the silence deadline from the period the server's capability document declares (BFS-041 §8.1: three missed heartbeats), and a negative deadline is not a bound", o.InvalidateIdleTimeout)
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
	// BFS-044: the hot-file policy. An ALL-ZERO policy means "no policy was
	// configured" and takes the defaults; anything else is validated strictly,
	// so a half-filled policy is refused instead of being completed with values
	// the operator never wrote. The validation is done AFTER the cache bounds
	// and the operation deadline are resolved, because the refusals are
	// relations against exactly those numbers (S-9, S-10, A.8, A.11).
	if o.Hot.IsZero() {
		o.Hot = fsclient.DefaultHotPolicy()
	}
	if err := o.Hot.Validate(o.PolicyEnv()); err != nil {
		return err
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

// ---------------------------------------------------------------------------
// BFS-044 — the effective configuration, readable at runtime.
//
// "A bound the owner cannot see is not a bound" (PRD §2.7), and the failure this
// project keeps finding is a figure that is reported one way and enforced
// another (BFS-031) or a counter that can never move (BFS-032). Both are
// prevented the same way here: the values the mount OBEYS are computed by the
// same code the mount uses and are written into the status document, so an
// operator reads what is in force rather than what they think they passed.
//
// These methods live in the OS-neutral file on purpose: the Windows driver will
// reuse this option set and this report verbatim, exactly as it reuses the
// client.
// ---------------------------------------------------------------------------

// PolicyEnv is the surrounding configuration the hot policy is validated and
// resolved against. Call it after Normalize: before then, the pool size, the op
// deadline and the cache bounds may still be zero, and a cross-check against a
// zero bound would be a check against nothing.
func (o Options) PolicyEnv() fsclient.HotPolicyEnv {
	return fsclient.HotPolicyEnv{
		Concurrency:        o.Concurrency,
		OpTimeout:          o.OpTimeout,
		CacheMaxBytes:      o.CacheMaxBytes,
		CacheMaxEntryBytes: o.CacheMaxEntryBytes,
	}
}

// EffectiveHotPolicy resolves the configured hot-file policy into the numbers
// the mount obeys, including the derived ones (P-4's clamp, the share's slot
// arithmetic, the reservation ceiling, the decay half-life). It is what the
// status document carries.
func (o Options) EffectiveHotPolicy() fsclient.HotPolicyEffective {
	return o.Hot.Effective(o.PolicyEnv())
}

// EffectiveConfig is the whole option set as resolved: the cache's byte AND
// entry bounds, the staged-refresh width, the pool size, the invalidation
// mechanism and its cadence, the declared silence deadline, and the entire
// hot-file policy. It exists so that "the effective values are readable at
// runtime" is one call rather than a scavenger hunt through the status
// document's other blocks.
func (o Options) EffectiveConfig() fsclient.EffectiveConfig {
	return fsclient.EffectiveConfig{
		CacheMaxBytes:             o.CacheMaxBytes,
		CacheMaxEntries:           o.CacheMaxEntries,
		CacheMaxEntryBytes:        o.CacheMaxEntryBytes,
		CacheMaxInFlight:          o.CacheMaxInFlight,
		CacheMaxAgeMS:             o.CacheMaxAge.Milliseconds(),
		Concurrency:               o.Concurrency,
		Invalidation:              o.Invalidation,
		PollIntervalMS:            o.PollInterval.Milliseconds(),
		InvalidationIdleTimeoutMS: o.InvalidateIdleTimeout.Milliseconds(),
		OnConflict:                o.OnConflict,
		Snapshot:                  o.Snapshot,
		Hot:                       o.EffectiveHotPolicy(),
	}
}
