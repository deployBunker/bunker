package cli

// Fail-closed target binding (GAP-093, the correctness keystone).
//
// The hazard: `bunker use` writes active_server into the SHARED config file,
// and every command that omitted --server fell back to it. So whichever
// session last ran `bunker use` silently re-targeted every other session —
// measured in this repo's board row as a sibling session's write landing on a
// different server than the operator intended.
//
// The fix has two halves:
//
//  1. SessionScopedTarget: the binding resolver every MUTATING command goes
//     through. Order of precedence, most explicit wins:
//         --server flag  >  BUNKER_SESSION_TARGET env  >  REFUSE
//     There is NO fallback to the shared active_server. The refusal names both
//     remedies, so it teaches the fix rather than just failing.
//
//  2. ReadOnlyTarget: read-only commands may keep the convenience default, but
//     must PRINT which target they resolved, so a read is never mistaken for a
//     read of a different server.
//
// Both return the resolved name so callers report it uniformly.

import (
	"fmt"
	"os"
)

// SessionTargetEnvVar is the session-scoped binding. Exported so docs and the
// refusal message can name it and so tests can set it.
const SessionTargetEnvVar = "BUNKER_SESSION_TARGET"

// ErrNoTarget is the named refusal every mutating command produces when no
// explicit binding exists. It is a distinct type so scripts and tests can
// detect it (errors.As) without matching on text.
type ErrNoTarget struct {
	Command string
}

func (e *ErrNoTarget) Error() string {
	return fmt.Sprintf(
		"no target bound: pass --server/--agent or set %s (mutating commands never fall back to the shared 'bunker use' default — that default is how one session re-targets another)",
		SessionTargetEnvVar)
}

// SessionScopedTarget resolves the (server, agent) binding for a MUTATING
// command. serverName is the --server flag value ("" when unset). The shared
// config's active_server is deliberately NOT consulted: an implicit global is
// the bug this exists to kill.
//
// cfgActiveServer is passed in only so the refusal can say what WOULD have
// been used and why that is exactly the danger — it is never returned.
func SessionScopedTarget(serverName, cfgActiveServer string) (string, error) {
	if serverName != "" {
		return serverName, nil
	}
	if env := os.Getenv(SessionTargetEnvVar); env != "" {
		return env, nil
	}
	return "", &ErrNoTarget{}
}

// ReadOnlyTarget resolves the target for a read-only command: same precedence,
// then the shared default as a documented convenience — never a refusal, but
// the caller MUST print the result.
func ReadOnlyTarget(serverName, cfgActiveServer string) string {
	if serverName != "" {
		return serverName
	}
	if env := os.Getenv(SessionTargetEnvVar); env != "" {
		return env
	}
	return cfgActiveServer
}
