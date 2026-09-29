package agent

// BNK-DF-001 regression: destroy's compensating execs must run to completion
// even when the request context arrives ALREADY CANCELLED — the chi
// middleware.Timeout shape (request_timeout, default 300s): by the time the
// compensating execs run, the request ctx is done, and exec.CommandContext
// REFUSES to start a command on an already-done context. Pre-fix, `userdel
// -rf` never executed, the destroy still reported success, and the
// bunker-<id> user + home survived ssh-able. Post-fix every compensating
// stage draws its own context from the rollback budget (spawn_failure.go),
// so the userdel seam is invoked and completes under a dead request.
//
// The userdel evidence is a PATH stub recorder (the DF-BUNKER-33 pattern):
// running the real userdel is exactly what this task exists to prevent.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDestroy_CancelledRequestContextStillRunsUserdel hands Destroy a
// pre-cancelled request context and asserts the compensating chain still
// reaches (and completes) the userdel seam, with the destroy concluding
// destroyed. Both force modes are pinned: non-force is the CLI default and
// pre-fix it aborted with userdel_failed; force mode pre-fix "succeeded"
// while userdel had silently never run — only the stub recorder catches it.
func TestDestroy_CancelledRequestContextStillRunsUserdel(t *testing.T) {
	// Budgets shrunk to milliseconds (the spawn_failure_test.go pattern):
	// the steps must be LIVE — never already expired, which is the property
	// under test — but nothing here may wait out the production
	// 60s/15s/5s budgets. Every step still gets at least the reserved floor.
	restore := shrinkRollbackBudgets(t, 2*time.Second, time.Second, 500*time.Millisecond)
	defer restore()

	for _, tc := range []struct {
		name  string
		force bool
	}{
		{name: "non_force", force: false},
		{name: "force", force: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			m := newGateManager(t, &buf)
			const id = "dfb001-cancel"
			username := "bunker-" + id
			liveAgent(t, m, id)

			// The live-process gate passes and the user record resolves: the
			// destroy must reach userdel, not refuse.
			stubDestroyProcessProbe(t, func(string) ([]userProcess, uint32, bool, error) {
				return nil, 61001, true, nil
			})
			presentUserWithUID(t, username, "61001")

			// userdel recorder stub (DF-BUNKER-33 pattern): one line per
			// invocation, exit 0 — the seam "completed".
			userLog := filepath.Join(t.TempDir(), "userdel.log")
			stubDir := t.TempDir()
			script := "#!/bin/sh\nprintf '%s\\n' \"$0 $*\" >> \"" + userLog + "\"\nexit 0\n"
			if err := os.WriteFile(filepath.Join(stubDir, "userdel"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

			// No home to archive: keeps the archive precondition
			// (DF-BUNKER-33) out of this test's scope and the run hermetic.
			pointHomeAt(t, t.TempDir())

			// THE premise: the request ctx is already done when Destroy is
			// entered — the request timeout expired before the compensating
			// execs ran.
			requestCtx, cancel := context.WithCancel(context.Background())
			cancel()
			if requestCtx.Err() == nil {
				t.Fatal("test premise broken: the request context is not cancelled")
			}

			resp, err := m.Destroy(requestCtx, id, tc.force)
			if err != nil {
				t.Fatalf("Destroy under a pre-cancelled request ctx must still complete: %v", err)
			}
			if resp == nil || resp.Status != "destroyed" {
				t.Fatalf("status = %v, want destroyed", resp)
			}

			// The userdel seam was INVOKED and COMPLETED: the stub recorded
			// the -rf removal of this agent's user.
			calls := userdelCalls(t, userLog)
			if len(calls) == 0 {
				t.Fatal("userdel never ran under the pre-cancelled request context (the compensating exec no-op'd on a dead ctx)")
			}
			want := "-rf " + username
			found := false
			for _, call := range calls {
				if strings.Contains(call, want) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("userdel calls do not carry %q: %v", want, calls)
			}

			// The destroy ran to its end, not through the userdel-failure
			// early return: the durable planes are clean.
			if rec := m.tracker.Get(id); rec != nil {
				t.Error("tracker record survived a completed destroy")
			}
			if m.portAlloc != nil && m.portAlloc.Has(id) {
				t.Error("port range survived a completed destroy")
			}
		})
	}
}
