package config

import "time"

// DF-BUNKER-81: the destroy home archive is bounded by the SIZE of the home,
// never by a fixed budget.
//
// The defect this file closes: an agent home whose bulk is RUNTIME STATE (the
// rootless docker data-root lives at $HOME/.local/share/docker — 442M of a
// 688M home on the dogfood host) took ~30s to tar+gzip, so every fixed budget
// expired mid-stream, exec.CommandContext SIGKILLed the gzip child, and the
// fail-closed archive gate correctly refused the delete. The destroy was
// UNFINISHABLE: five attempts, five refusals, and the agent still running.
//
// Two budgets are derived from the same figure, and they must stay ordered
// (client deadline LONGER than the daemon's archive budget, so the client
// never gives up while the daemon is still legitimately archiving):
//
//   - ArchiveBudgetForHomeSize — the context the daemon hands to its archive
//     `tar` (internal/agent archiveExecContextFn);
//   - DestroyRequestTimeoutForHomeSize — the `bunker destroy` client deadline
//     (internal/cli destroyRequestTimeout).
//
// Both are pure functions of a byte count so the derivation is unit-testable
// without a host, a daemon or a large home.

const (
	// ArchiveBudgetBase covers the fixed costs of one `tar czf` run: process
	// start, the recursive directory walk, and the final flush/close. It is
	// deliberately generous — it is the part of the budget that does not
	// shrink on a fast, small home.
	ArchiveBudgetBase = 2 * time.Minute

	// ArchiveBytesPerSecond is the CONSERVATIVE tar+gzip throughput (4 MiB/s)
	// the size-derived part of the budget assumes. Real gzip throughput on a
	// mixed workload (many small files, partly-compressible container layers)
	// is several times higher; a budget derived from a deliberately slow rate
	// is headroom, not a deadline the archive is expected to race.
	ArchiveBytesPerSecond = 4 << 20 // 4 MiB/s

	// ArchiveBudgetMin is the floor for every archive run, whatever the home
	// size. It exists so an UNKNOWN or unmeasurable size (the walk failed, an
	// empty home, a fresh host) never produces a tiny budget: the pre-fix
	// behaviour is what this constant exists to make unreachable.
	ArchiveBudgetMin = 10 * time.Minute

	// ArchiveBudgetMax caps a pathological home (or a bogus size figure)
	// without bounding it out of existence: 4h of tar+gzip at
	// ArchiveBytesPerSecond is ~56 GiB, far past any agent home this daemon
	// will see.
	ArchiveBudgetMax = 4 * time.Hour

	// DestroyRequestTimeoutMin is the floor for the `bunker destroy` client
	// deadline. It is the size-UNKNOWN default (a client that cannot resolve
	// the agent's footprint still gets a deadline that cannot race a normal
	// archive) and it is never shorter than the daemon's own per-request
	// budget (DefaultServerRequestTimeout, 300s).
	DestroyRequestTimeoutMin = 10 * time.Minute

	// DestroyRequestMargin is the headroom the client deadline keeps over the
	// daemon's archive budget. The client must outlive the daemon's own work:
	// the daemon refuses a destroy whose archive it could not complete
	// (home_retained, nothing deleted), and a client that expires FIRST turns
	// a slow-but-successful archive into a cancelled request.
	DestroyRequestMargin = 2 * time.Minute
)

// archiveBudgetSizeCap is the byte count at which the size-derived part of the
// budget would reach ArchiveBudgetMax. Sizes are clamped to it BEFORE the
// duration arithmetic, so a hostile or corrupt figure can neither overflow the
// duration nor exceed the cap.
func archiveBudgetSizeCap() int64 {
	return int64(ArchiveBudgetMax/time.Second) * ArchiveBytesPerSecond
}

// ArchiveBudgetForHomeSize returns the budget for archiving a home of
// homeBytes bytes: ArchiveBudgetBase plus homeBytes/ArchiveBytesPerSecond,
// clamped to [ArchiveBudgetMin, ArchiveBudgetMax]. A non-positive size means
// UNKNOWN and yields ArchiveBudgetMin (never a zero budget — a zero budget
// would reproduce the defect this function exists to fix).
func ArchiveBudgetForHomeSize(homeBytes int64) time.Duration {
	if homeBytes <= 0 {
		return ArchiveBudgetMin
	}
	if cap := archiveBudgetSizeCap(); homeBytes > cap {
		homeBytes = cap
	}
	budget := ArchiveBudgetBase + time.Duration(homeBytes/ArchiveBytesPerSecond)*time.Second
	if budget < ArchiveBudgetMin {
		return ArchiveBudgetMin
	}
	if budget > ArchiveBudgetMax {
		return ArchiveBudgetMax
	}
	return budget
}

// DestroyRequestTimeoutForHomeSize returns the client deadline for destroying
// an agent whose home holds homeBytes bytes: the daemon's archive budget plus
// DestroyRequestMargin, never below DestroyRequestTimeoutMin and never above
// the archive cap plus that same margin. Because the archive budget is
// clamped to [ArchiveBudgetMin, ArchiveBudgetMax], the returned deadline is
// always LONGER than the archive the daemon may legitimately be running.
func DestroyRequestTimeoutForHomeSize(homeBytes int64) time.Duration {
	deadline := ArchiveBudgetForHomeSize(homeBytes) + DestroyRequestMargin
	if max := ArchiveBudgetMax + DestroyRequestMargin; deadline > max {
		return max
	}
	if deadline < DestroyRequestTimeoutMin {
		return DestroyRequestTimeoutMin
	}
	return deadline
}
