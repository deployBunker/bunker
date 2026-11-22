package cli

// DOGFOOD-015: a spawn must be OBSERVABLE while it runs and EXPLAINABLE when
// it fails.
//
// Measured defect (bunker-las-03, 2026-11-15): `bunker spawn --tools-at-spawn`
// printed "Creating agent..." and then nothing for five minutes, and died at
// the daemon's flat 300s request timeout with
//
//	spawn agent: deadline_exceeded: context deadline exceeded; agent X was NOT
//	found on re-list after 5 attempts — the spawn likely failed server-side
//
// — no stage, no elapsed time, no budget. A slow spawn and a hung spawn were
// indistinguishable, and the operator had no way to learn that
// --tools-at-spawn is the expensive flag.
//
// This file owns the two missing surfaces:
//
//   - spawnProgressWatch: the in-flight heartbeat. It polls the daemon's
//     read-only GetSpawnProgress RPC (the stage the manager is currently
//     EXECUTING, same vocabulary as the failure text) and prints elapsed time
//     whether or not the stage is known. Verified: not printed speculatively,
//     the watcher reports only what the daemon returned.
//   - spawnRun: the failure report. It composes stage + elapsed + budget +
//     how-to-raise into one message, and it NEVER invents a stage: when the
//     daemon reported none, the line says so in those words.
//
// The client deadline is derived from the daemon's reported spawn budget
// (config.SpawnClientTimeout), so this client can no longer be the thing that
// kills a spawn the daemon is still working on (the QA-BUNKER-58 ordering law).

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	"github.com/deployBunker/bunker/internal/config"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// spawnProgressInterval is how often the heartbeat polls the daemon while a
// spawn is in flight. 15s keeps a 20-minute spawn to ~80 cheap reads while
// still catching a stage change within one stage's duration; it is a var so
// tests can drive the loop at millisecond scale.
var spawnProgressInterval = 15 * time.Second

// spawnProgressFirstPollDelay is when the FIRST poll happens. PERF-022's
// acceptance: the client must show in-flight progress rather than a silent
// block, and a ~15-25s warm spawn under the OLD cadence risked finishing
// before the first 15s tick — a spawn could return with zero progress lines
// after the opening "Creating agent...". The first read starts shortly after
// the RPC, then the interval cadence takes over; it is a var so tests can
// drive the loop at millisecond scale.
var spawnProgressFirstPollDelay = 2 * time.Second

// spawnProgressPollWindow bounds ONE GetSpawnProgress call: a stalled daemon
// must never wedge the heartbeat (the spawn RPC keeps its own deadline).
const spawnProgressPollWindow = 5 * time.Second

// spawnProgressSkew is the tolerance used to attribute an in-flight entry to an
// auto-ID spawn (the client never learns its id before the RPC returns). The
// daemon's elapsed and the client's elapsed start within one RPC of each other;
// anything further apart is a different spawn and must not be claimed.
const spawnProgressSkew = 15 * time.Second

// spawnProgressWatch is the in-flight observer for ONE spawn.
type spawnProgressWatch struct {
	started time.Time
	out     func(string) // line sink (fmt.Println in production; captured in tests)

	mu          sync.Mutex
	stage       string // last stage the DAEMON reported ("" = none reported yet)
	unsupported bool   // daemon answered CodeUnimplemented (pre-DOGFOOD-015)
	token       string // operator token: every GetSpawnProgress request carries it
	polls       int
	beats       int // heartbeat lines printed

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// startSpawnProgressWatch launches the heartbeat. The returned watch is always
// non-nil; Stop() must be called exactly once when the RPC returns.
//
// token is the SAME operator token the spawn RPC rides on. PERF-022 (measured
// live on bunker-las-03, 2026-11-22): the heartbeat built its request WITHOUT
// the Authorization header, so an auth-enabled daemon DENIED every poll — the
// client heartbeated elapsed time only and never named the daemon stage
// (audit log: GetSpawnProgress … denied, "missing Authorization header" on
// every poll). The failure report's stage fallback ("stage not reported by
// daemon") was the same silent lie on every real deployment.
func startSpawnProgressWatch(client bunkerv1connect.BunkerdClient, agentID string, started time.Time, out func(string), token string) *spawnProgressWatch {
	w := &spawnProgressWatch{
		started: started,
		out:     out,
		token:   token,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	go w.run(client, agentID)
	return w
}

func (w *spawnProgressWatch) run(client bunkerv1connect.BunkerdClient, agentID string) {
	defer close(w.done)
	// The FIRST poll lands at spawnProgressFirstPollDelay (PERF-022: an
	// in-waiting client must print a progress line, not only the opening line),
	// then the regular cadence takes over.
	first := time.NewTimer(spawnProgressFirstPollDelay)
	defer first.Stop()
	select {
	case <-w.stop:
		return
	case <-first.C:
	}
	w.tick(client, agentID)
	ticker := time.NewTicker(spawnProgressInterval)
	defer ticker.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-ticker.C:
			w.tick(client, agentID)
		}
	}
}

func (w *spawnProgressWatch) tick(client bunkerv1connect.BunkerdClient, agentID string) {
	elapsed := time.Since(w.started)
	w.mu.Lock()
	unsupported := w.unsupported
	w.mu.Unlock()
	if unsupported {
		w.heartbeat(elapsed)
		return
	}
	stage, ok, unsupportedNow := w.poll(client, agentID, elapsed)
	w.mu.Lock()
	w.polls++
	if unsupportedNow {
		w.unsupported = true
	}
	if ok && stage != "" {
		w.stage = stage
	}
	w.mu.Unlock()
	w.emit(elapsed, stage, ok)
}

// emit prints one heartbeat line. A stage is named only when the daemon
// reported one; otherwise the line carries elapsed time alone — an elapsed
// clock is proof of life, an invented stage would be a lie.
func (w *spawnProgressWatch) emit(elapsed time.Duration, stage string, haveStage bool) {
	w.mu.Lock()
	w.beats++
	w.mu.Unlock()
	if haveStage && stage != "" {
		w.out(fmt.Sprintf("  still creating agent... %s (daemon stage: %s)", shortDuration(elapsed), stage))
		return
	}
	w.out(fmt.Sprintf("  still creating agent... %s", shortDuration(elapsed)))
}

// heartbeat prints an elapsed-only line (the daemon has no stage capability).
func (w *spawnProgressWatch) heartbeat(elapsed time.Duration) { w.emit(elapsed, "", false) }

// Beats reports how many heartbeat lines were printed. The caller uses it to
// decide whether a closing "created in …" line is warranted: a spawn that
// answered before the first heartbeat must print exactly what it printed
// before this feature existed.
func (w *spawnProgressWatch) Beats() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.beats
}

// poll asks the daemon what it is doing. The second return is "the daemon
// answered and this entry is ours"; the third is "this daemon does not have the
// capability" (CodeUnimplemented), which is not an error — a pre-DOGFOOD-015
// daemon simply gets an elapsed-only heartbeat.
func (w *spawnProgressWatch) poll(client bunkerv1connect.BunkerdClient, agentID string, elapsed time.Duration) (string, bool, bool) {
	pctx, cancel := context.WithTimeout(context.Background(), spawnProgressPollWindow)
	defer cancel()
	req := connect.NewRequest(&v1.GetSpawnProgressRequest{AgentId: agentID})
	if w.token != "" {
		// PERF-022: an auth-enabled daemon denied every header-less poll —
		// the client then reported "stage not reported" while the daemon was
		// happily mid-stage. Same token, same header form as the spawn RPC.
		req.Header().Set("Authorization", "Bearer "+w.token)
	}
	resp, err := client.GetSpawnProgress(pctx, req)
	if err != nil {
		if connect.CodeOf(err) == connect.CodeUnimplemented {
			return "", false, true
		}
		return "", false, false // transport hiccup: the next tick may answer
	}
	stage, ok := selectSpawnProgress(resp.Msg.GetSpawns(), agentID, elapsed)
	return stage, ok, false
}

// selectSpawnProgress picks THIS spawn's entry out of a GetSpawnProgress reply.
//
// With a known agent id the match is exact. An auto-ID spawn (the daemon chose
// the id) can only be attributed by elapsed time: exactly ONE in-flight entry
// whose daemon-side elapsed is within spawnProgressSkew of the client's own is
// attributed; zero or several are NOT guessed at, because naming another
// operator's spawn stage would be worse than naming none.
func selectSpawnProgress(entries []*v1.SpawnProgress, agentID string, elapsed time.Duration) (string, bool) {
	if agentID != "" {
		for _, e := range entries {
			if e.GetAgentId() == agentID {
				return e.GetStage(), true
			}
		}
		return "", false
	}
	var found *v1.SpawnProgress
	for _, e := range entries {
		d := time.Duration(e.GetElapsedSeconds())*time.Second - elapsed
		if d < -spawnProgressSkew || d > spawnProgressSkew {
			continue
		}
		if found != nil {
			return "", false // ambiguous: never claim one of several
		}
		found = e
	}
	if found == nil {
		return "", false
	}
	return found.GetStage(), true
}

// Stop ends the heartbeat and waits for its goroutine to exit. Safe to call
// once; a second call is a no-op that does not deadlock.
func (w *spawnProgressWatch) Stop() {
	w.stopOnce.Do(func() { close(w.stop) })
	<-w.done
}

// LastStage returns the most recent stage the daemon reported.
func (w *spawnProgressWatch) LastStage() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stage
}

// spawnStageSource names where a reported stage came from, for the failure
// report's parenthetical.
const (
	stageSourceNone     = "stage not reported by daemon"
	stageSourceProgress = "daemon progress report"
	stageSourceFailure  = "daemon failure report"
)

// spawnFailureStageLines are the daemon's own stage-carrying failure wordings
// (internal/agent/spawn_failure.go): "spawn <id> failed at stage <stage>" and
// the rollback variant that appends the stage after "failed at stage". Matching
// the fixed wording — not a free-text search for a stage name — is what keeps
// this from attributing a stage the daemon never said.
var spawnFailureStageMarkers = []string{"failed at stage "}

// stageFromSpawnErrorText extracts a stage from the daemon's failure message.
// The stage token is the run of [a-z0-9-] right after "failed at stage ".
func stageFromSpawnErrorText(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	for _, marker := range spawnFailureStageMarkers {
		idx := strings.Index(msg, marker)
		if idx < 0 {
			continue
		}
		rest := msg[idx+len(marker):]
		end := 0
		for end < len(rest) {
			c := rest[end]
			if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
				end++
				continue
			}
			break
		}
		if end > 0 {
			return rest[:end]
		}
	}
	return ""
}

// spawnRun is the fact bag for one spawn attempt: what to call it, when it
// started, the budget that governs it, and the in-flight observer (which may be
// nil when the spawn never reached the RPC).
type spawnRun struct {
	agentID string
	started time.Time
	// budget is the daemon's spawn budget in force (0 = unknown: a daemon that
	// predates the ServerInfo capability, or a failed probe).
	budget time.Duration
	watch  *spawnProgressWatch
}

// subject names the spawn in a failure report: the requested id, or the fact
// that the daemon was choosing one.
func (r *spawnRun) subject() string {
	if r.agentID == "" {
		return "a new (auto-ID) agent"
	}
	return fmt.Sprintf("agent %q", r.agentID)
}

// failureReport composes the whole failure message for cause.
//
// The contract (DOGFOOD-015 acceptance): the message names (a) the stage the
// daemon reported, or says in as many words that none was reported, (b) the
// elapsed wall time, (c) the budget in force and how to raise it. It never
// upgrades a guess into a stage.
func (r *spawnRun) failureReport(cause error) string {
	elapsed := time.Since(r.started)

	stage := stageFromSpawnErrorText(cause)
	source := stageSourceFailure
	if stage == "" && r.watch != nil {
		if last := r.watch.LastStage(); last != "" {
			stage, source = last, stageSourceProgress
		}
	}
	stageLine := stageSourceNone
	if stage != "" {
		stageLine = fmt.Sprintf("%s (%s)", stage, source)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "spawn %s failed after %s\n", r.subject(), shortDuration(elapsed))
	fmt.Fprintf(&b, "  stage: %s\n", stageLine)
	fmt.Fprintf(&b, "  elapsed: %s\n", shortDuration(elapsed))
	if r.budget > 0 {
		fmt.Fprintf(&b, "  budget: daemon spawn budget %s (server.spawn_request_timeout), client deadline %s\n",
			shortDuration(r.budget), shortDuration(config.SpawnClientTimeout(r.budget)))
		if elapsed >= r.budget {
			fmt.Fprintf(&b, "          the daemon's own budget was exceeded — raise it and retry\n")
		}
	} else {
		fmt.Fprintf(&b, "  budget: not reported by the daemon (pre-DOGFOOD-015 build); client deadline %s\n",
			shortDuration(config.SpawnClientTimeout(0)))
	}
	fmt.Fprintf(&b, "  raise it: server.spawn_request_timeout: 45m in the daemon config (bunkerd), then restart bunkerd\n")
	fmt.Fprintf(&b, "  cause: %v", cause)
	return b.String()
}

// shortDuration renders a duration the way an operator reads a wall clock:
// whole seconds under a minute, then m/s, then h/m/s. Sub-second precision is
// noise in a spawn report (and would make the message untestable).
func shortDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm%02ds", int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60)
	}
}

// daemonSpawnBudget asks the daemon how long it allows ONE spawn
// (ServerInfo.spawn_request_timeout_seconds) and reports whether it answered.
//
// The second return is "the daemon reported a budget". false covers both a
// pre-DOGFOOD-015 daemon (field absent = 0) and a probe that failed: in both
// cases the caller falls back to the derived budget rather than to "no
// budget", and the failure report says the budget was not reported.
func daemonSpawnBudget(entry ServerEntry) (time.Duration, bool) {
	info, err := ConnectServerWithEntry(entry)
	if err != nil {
		return 0, false
	}
	secs := info.GetSpawnRequestTimeoutSeconds()
	if secs == 0 {
		return 0, false
	}
	return time.Duration(secs) * time.Second, true
}
