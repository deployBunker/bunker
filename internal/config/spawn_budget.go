package config

import "time"

// DOGFOOD-015: the spawn budget is derived from the COLD-HOST work a spawn
// actually performs, never from a flat constant.
//
// The defect this file closes: `bunker spawn --tools-at-spawn` against a cold
// host (bunker-las-03, 2026-11-15) was killed twice by the daemon's own flat
// 300s request timeout (server.request_timeout). Measured on that host:
//
//   - the pre-image-build stages (validate → useradd → isolation → keys →
//     rootless docker install → dockerd start → container cap) completed
//     inside 48s: a spawn WITHOUT --tools-at-spawn reached the slice-limits
//     verification in 48s (and then failed on that host's swap containment, an
//     unrelated defect — the point here is that nothing before the image build
//     is slow). The repo's long-standing note agrees: the ~93MB rootless
//     install runs 60-90s+ under load.
//   - --tools-at-spawn adds an image build (apt ripgrep + `go install
//     golang.org/x/tools/gopls@latest` compiled from a cold module cache) that
//     was STILL RUNNING when the 300s bound killed it: the daemon's own
//     failure read "spawn df015-probe3 failed at stage image-build: spawn was
//     cancelled by the server request timeout (request_timeout, default 300s)
//     ... docker build ... Canceled: context canceled". So one stage alone
//     needs more than five minutes on a cold host.
//
// So the budget is BASE + one allowance per measured cold stage. Every
// allowance is deliberately GENEROUS: this is headroom the spawn is not
// expected to race, not a deadline it must finish inside. Sizing it tightly
// would reproduce the defect, because the failure mode is a SIGKILL mid-stage
// (exec.CommandContext) that leaves the rollback to clean up after the fact.
//
// The spawn procedure is budgeted as a WHOLE rather than per request shape:
// the daemon's timeout middleware fires before the request body is read, so it
// cannot know whether a given spawn asked for --tools-at-spawn. Every spawn
// therefore gets the worst-case (tools included) budget, and every OTHER RPC
// keeps the ordinary server.request_timeout — see
// internal/server.requestTimeoutMiddleware.
const (
	// SpawnBudgetBase is the fixed cost of a spawn: validation, capacity, port
	// allocation, useradd, the isolation boundary, keygen, authorized_keys,
	// dockerd start + verify, the container cap, slice limits and the SSH
	// session probe. Measured on bunker-las-03: everything up to and including
	// dockerd start finished inside 48s, so this four-minute allowance is ~5x
	// the observed cost (the session probe and register follow, both seconds).
	SpawnBudgetBase = 4 * time.Minute

	// SpawnRootlessInstallBudget covers the rootless docker install on a host
	// where the agent home does not already carry it (~93MB download, unpack
	// and setup) — the observed 60-90s+ under load is already inside the 48s
	// figure above on an idle host. Five minutes is the headroom a cold,
	// loaded host needs for the download alone.
	SpawnRootlessInstallBudget = 5 * time.Minute

	// SpawnToolsImageBuildBudget covers the --tools-at-spawn image build: apt
	// ripgrep plus `go install ...gopls@latest` compiled from a cold module
	// cache inside the agent's own rootless builder. Measured (bunker-las-03,
	// 2026-11-15): a spawn that asked for the tool set was still inside this
	// stage when the daemon's old flat 300s bound killed it, so the stage alone
	// runs longer than five minutes on a cold host.
	SpawnToolsImageBuildBudget = 10 * time.Minute

	// SpawnBudgetMax caps the DERIVED allowance (see SpawnRequestTimeout). An
	// operator's own server.request_timeout longer than this still wins: a
	// spawn table must never be a SHORTER bound than the one every other RPC
	// gets. Beyond twenty minutes of work the answer is a faster host, not a
	// longer budget — a spawn at that point is holding a port range, a
	// half-created user and a tracker window open.
	SpawnBudgetMax = 20 * time.Minute

	// SpawnClientMargin is the headroom the CLIENT deadline keeps over the
	// daemon's own spawn budget. Same ordering law as the destroy archive
	// (archive_budget.go): the client must outlive the daemon's work, because
	// a client that expires first turns a slow-but-successful spawn into a
	// cancelled request — and the daemon's stages do NOT abort with it
	// (QA-BUNKER-58: measured 20-42s of work after the client gave up).
	SpawnClientMargin = 2 * time.Minute
)

// SpawnRequestTimeout returns the daemon-side budget for ONE SpawnAgent
// request: SpawnBudgetBase plus the rootless-install and image-build
// allowances, capped at SpawnBudgetMax, and never shorter than the ordinary
// per-request timeout it sits beside. A non-positive ordinary budget (an
// unset or hand-built config) still yields the full derived budget — never a
// zero timeout, which would kill every spawn at once.
func SpawnRequestTimeout(ordinary time.Duration) time.Duration {
	budget := SpawnBudgetBase + SpawnRootlessInstallBudget + SpawnToolsImageBuildBudget
	if budget > SpawnBudgetMax {
		budget = SpawnBudgetMax
	}
	if ordinary > budget {
		budget = ordinary
	}
	return budget
}

// SpawnClientTimeout returns the CLI deadline for a spawn against a daemon
// whose reported spawn budget is daemonBudget: that budget plus
// SpawnClientMargin. A non-positive daemonBudget (a daemon that predates the
// ServerInfo capability) falls back to the derived daemon budget, so the client
// is never shorter than the work the daemon may legitimately be doing.
func SpawnClientTimeout(daemonBudget time.Duration) time.Duration {
	if daemonBudget <= 0 {
		daemonBudget = SpawnRequestTimeout(DefaultServerRequestTimeout)
	}
	return daemonBudget + SpawnClientMargin
}

// EffectiveSpawnRequestTimeout resolves the daemon's spawn budget: the
// operator's server.spawn_request_timeout when set, otherwise the derived
// SpawnRequestTimeout(server.request_timeout). Config.Method (not a bare
// constant read) so the middleware, ServerInfo's capability report and any
// future caller can never disagree about the budget in force.
func (c *Config) EffectiveSpawnRequestTimeout() time.Duration {
	if c == nil {
		return SpawnRequestTimeout(DefaultServerRequestTimeout)
	}
	if c.Server.SpawnRequestTimeout > 0 {
		return c.Server.SpawnRequestTimeout
	}
	return SpawnRequestTimeout(c.Server.RequestTimeout)
}
