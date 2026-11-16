// DOGFOOD-015: per-procedure request budgets.
//
// The daemon used to install ONE flat middleware.Timeout(server.request_timeout)
// for every RPC (300s by default). That bound is right for the ordinary
// request shape — a status read, a list, an exec start — but it is wrong for
// SpawnAgent, whose cold-host stages legitimately exceed it (a ~93MB rootless
// docker install plus a --tools-at-spawn image build that compiles gopls from
// an empty module cache). Measured on bunker-las-03 (2026-11-15): two
// `bunker spawn --tools-at-spawn` runs were killed at the flat 300s bound with
// no stage reported to the caller.
//
// This file keeps the ordinary bound intact and gives the spawn procedure the
// derived budget (config.SpawnRequestTimeout), selected per REQUEST — the
// daemon's timeout middleware fires before the request body is read, so the
// selector keys on the procedure path, never on the request contents.
package server

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// requestTimeoutMiddleware applies ordinary to every request and spawn to the
// Bunkerd SpawnAgent procedure. Both budgets are installed as ordinary chi
// timeout middleware; only the selection differs, so the cancellation
// semantics (a cancelled request context, no goroutine left behind) are
// exactly the ones the daemon already relied on.
func requestTimeoutMiddleware(ordinary, spawn time.Duration) func(http.Handler) http.Handler {
	ordinaryMW := middleware.Timeout(ordinary)
	spawnMW := middleware.Timeout(spawn)
	return func(next http.Handler) http.Handler {
		ordinaryHandler := ordinaryMW(next)
		spawnHandler := spawnMW(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isSpawnAgentRequest(r.URL.Path) {
				spawnHandler.ServeHTTP(w, r)
				return
			}
			ordinaryHandler.ServeHTTP(w, r)
		})
	}
}

// isSpawnAgentRequest reports whether path addresses the Bunkerd SpawnAgent
// procedure. connect-go serves a procedure at <baseURL><Service>/<Method>, so
// an exact match on the generated procedure constant is the whole test: a
// substring match would also catch a future procedure whose name merely
// contains "SpawnAgent".
func isSpawnAgentRequest(path string) bool {
	return path == bunkerv1connect.BunkerdSpawnAgentProcedure
}
