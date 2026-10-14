package agent

// MON-MONITORING-USERDELHOME-001 regression: the destroy path's
// "home outlives its user" leak. On Debian/Ubuntu, `userdel -rf` exits
// NONZERO with "userdel: <user> mail spool not found" even when the user +
// home removal SUCCEEDED — destroyUserAbsentOutput's blanket "not found"
// match classified that success-with-warning as the benign absent-user
// class, and the non-force fallback branch then returned success / not_found
// WITHOUT re-probing the home. When the host kill-timer SIGKILLed userdel
// mid-run on a large home (up to 15G incl the rootless docker data), the
// passwd entry was gone but /home/bunker-<id> SURVIVED — and bunkerd logged
// "agent destroyed". Observed live 2026-10-14.
//
// Post-fix: (A) a mail-spool-warning failure is only classified as a
// completed delete when the HOST probe confirms it — the user record is gone
// AND the home directory is gone; (B) every absent-class success-shaped
// return refuses with StatusUserdelFailed + a recorded refusal when the
// home directory still exists.
//
// Every test drives Destroy through SEAMS (userdelRunner, lookupUser,
// agentHomeRoot) — no root, no real host users, no real userdel.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/registry"
	"github.com/deployBunker/bunker/internal/resource"
)

// udh001MailSpoolOutput is the verbatim Debian/Ubuntu success-with-warning
// shape from the defect row: exit nonzero, mail spool "not found", user and
// home actually removed.
func udh001MailSpoolOutput(uname string) []byte {
	return []byte(fmt.Sprintf("userdel: %s mail spool not found\n", uname))
}

// udh001Manager is the hermetic harness — the df63Manager shape: a temp
// durable registry at regPath ("" = disabled registry) + debug logger into
// buf. The caller then stubs only the seams the row under test needs.
func udh001Manager(t *testing.T, buf *bytes.Buffer, regPath string) *AgentManager {
	t.Helper()
	cfg := config.DefaultConfig()
	if regPath != "" {
		isolateRegistryAt(t, cfg, regPath)
	}
	var logger *slog.Logger
	if buf == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
	} else {
		logger = slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	tracker := resource.NewTracker(cfg.Agent.MaxAgents, logger)
	m := NewAgentManager(cfg, logger, tracker, nil, nil)
	m.listSystemAgents = func() ([]SystemAgent, error) { return nil, nil }
	t.Cleanup(func() { m.Stop() })
	return m
}

// udh001StubHost stages the host-probe seams: lookupUser resolves ONLY for
// userGone=false, and the home root is a temp dir holding the home
// directory when homeGone=false (created) or empty when homeGone=true.
func udh001StubHost(t *testing.T, username string, userGone, homeGone bool) {
	t.Helper()
	stubLookupUser(t, func(name string) (*user.User, error) {
		if !userGone && name == username {
			return &user.User{Username: name, Uid: "61001", Gid: "61001", HomeDir: "/home/" + name}, nil
		}
		return nil, user.UnknownUserError(name)
	})
	root := t.TempDir()
	if !homeGone {
		if err := os.MkdirAll(filepath.Join(root, username), 0o755); err != nil {
			t.Fatalf("stage surviving home: %v", err)
		}
	}
	pointHomeAt(t, root)
}

func TestMON_Monitoring_UserdelHome_MailSpoolWarning(t *testing.T) {
	const id = "udh001-x"
	username := "bunker-" + id
	mailSpoolOut := udh001MailSpoolOutput(username)

	tests := []struct {
		name        string
		force       bool
		userGone    bool
		homeGone    bool
		knownAgent  bool
		out         string // userdel output override; "" = the mail-spool shape
		wantStatus  string // response status
		wantErrSub  string // substring the error must carry ("" = must be nil)
		wantRefusal bool   // a durable refusal event must be recorded
	}{
		{
			// (A) success-with-warning + host confirms user AND home gone:
			// the destroy SUCCEEDS on the normal success path — no longer
			// misrouted into the absent/leak fallback.
			name:       "force/mailspool_warning_home_gone_is_destroyed",
			force:      true,
			userGone:   true,
			homeGone:   true,
			wantStatus: "destroyed",
		},
		{
			// Same classification on the non-force path (a known agent:
			// the host-probe-confirmed mail-spool shape never reaches the
			// not_found fallback).
			name:       "nonforce/mailspool_warning_home_gone_is_destroyed",
			force:      false,
			userGone:   true,
			homeGone:   true,
			knownAgent: true,
			wantStatus: "destroyed",
		},
		{
			// (B) the leak: mail-spool warning, user record gone, home
			// SURVIVED (the kill-timer SIGKILLed userdel mid-run). The
			// destroy must refuse — StatusUserdelFailed, error naming the
			// surviving path, refusal recorded — never success.
			name:        "nonforce/user_gone_home_survived_refuses",
			force:       false,
			userGone:    true,
			homeGone:    false,
			knownAgent:  true,
			wantStatus:  StatusUserdelFailed,
			wantErrSub:  "SURVIVING_HOME",
			wantRefusal: true,
		},
		{
			// The same refusal in force mode: the force fall-through must
			// not report "agent destroyed" over a surviving home.
			name:        "force/user_gone_home_survived_refuses",
			force:       true,
			userGone:    true,
			homeGone:    false,
			wantStatus:  StatusUserdelFailed,
			wantErrSub:  "SURVIVING_HOME",
			wantRefusal: true,
		},
		// The genuine absent-user wording keeps its historical path:
		// not_found for a never-seen ID, no retry, no refusal — the
		// conservative classifier is unchanged for this class.
		{
			name:       "nonforce/genuine_absent_user_still_not_found",
			force:      false,
			userGone:   true,
			homeGone:   true,
			out:        "userdel: user bunker-udh001-x does not exist\n",
			wantStatus: "not_found",
			wantErrSub: "not found",
		},
		{
			// A still-present user record is never a confirmed delete:
			// the mail-spool warning with the user resolvable and the
			// home gone falls to the not_found fallback (absent-class),
			// which is honest here because the home is gone.
			name:       "nonforce/user_still_present_home_gone_not_found",
			force:      false,
			userGone:   false,
			homeGone:   true,
			wantStatus: "not_found",
			wantErrSub: "not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			var regPath string
			if tt.knownAgent || tt.wantRefusal {
				// A temp registry so the durable record exists: the
				// idempotent-destroy rows need a known agent, the refusal
				// rows need a journal the refusal EVENT stays in after the
				// destroy path returns (the folded view is deleted).
				regPath = filepath.Join(t.TempDir(), "agents.jsonl")
			}
			m := udh001Manager(t, &buf, regPath)
			if regPath != "" {
				if err := m.registry.AppendSpawn(&registry.Record{AgentID: id, Status: StatusRunning}); err != nil {
					t.Fatalf("seed registry record: %v", err)
				}
			}
			udh001StubHost(t, username, tt.userGone, tt.homeGone)
			liveAgent(t, m, id)

			// The expected surviving-home path is computed AFTER the home
			// root points at this row's temp dir (a table literal would
			// capture the pre-stub /home root).
			wantErrSub := tt.wantErrSub
			if wantErrSub == "SURVIVING_HOME" {
				wantErrSub = filepath.Join(agentHomeRoot, username)
			}

			var calls int
			out := tt.out
			if out == "" {
				out = string(mailSpoolOut)
			}
			stubUserdelRunner(t, func(_ context.Context, uname string) ([]byte, error) {
				calls++
				rendered := out
				if strings.Contains(rendered, "%s") {
					rendered = fmt.Sprintf(rendered, uname)
				}
				return []byte(rendered), errors.New("exit status 1")
			})

			resp, derr := m.Destroy(context.Background(), id, tt.force)

			if tt.wantErrSub == "" {
				if derr != nil {
					t.Fatalf("Destroy() error = %v, want nil", derr)
				}
			} else if derr == nil || !strings.Contains(derr.Error(), wantErrSub) {
				t.Fatalf("Destroy() error = %v, want it to contain %q", derr, wantErrSub)
			}
			if resp == nil || resp.Status != tt.wantStatus {
				t.Fatalf("status = %v, want %q", resp, tt.wantStatus)
			}
			if calls != 1 {
				t.Errorf("userdel calls = %d, want exactly 1 (no retry on the mail-spool shape)", calls)
			}
			if wantErrSub == filepath.Join(agentHomeRoot, username) && !strings.Contains(derr.Error(), "surviving state:") {
				t.Errorf("refusal error missing the surviving-state evidence: %v", derr)
			}

			if tt.wantRefusal {
				evs := qa61RefusalEvents(t, regPath, id)
				if len(evs) != 1 {
					t.Fatalf("refusal events in journal = %d, want 1: %v", len(evs), evs)
				}
				refusal, _ := evs[0]["refusal"].(map[string]any)
				if got, _ := refusal["status"].(string); got != StatusUserdelFailed {
					t.Errorf("journal refusal status = %v, want %q", got, StatusUserdelFailed)
				}
				le, _ := refusal["last_error"].(string)
				if !strings.Contains(le, "still exists") || !strings.Contains(le, "surviving state:") {
					t.Errorf("journal refusal last_error missing the surviving-home evidence: %q", le)
				}
			} else if regPath != "" {
				if evs := qa61RefusalEvents(t, regPath, id); len(evs) != 0 {
					t.Errorf("a non-refused row recorded %d refusal events, want 0: %v", len(evs), evs)
				}
			}

			// A surviving home can never end in a success-shaped answer.
			if !tt.homeGone && (resp.Status == "destroyed" || resp.Status == "not_found") {
				t.Errorf("status %q returned while the home survived — the leak is back", resp.Status)
			}
		})
	}
}

// TestMON_Monitoring_UserdelHome_DestroyDeleteConfirmedOnHost_Unit pins the
// host probe's own contract: BOTH the user record and the home must be gone;
// either one surviving (or an unobservable state) never confirms a delete.
func TestMON_Monitoring_UserdelHome_DestroyDeleteConfirmedOnHost_Unit(t *testing.T) {
	const username = "bunker-udh001-unit"
	tests := []struct {
		name     string
		userGone bool
		homeGone bool
		want     bool
	}{
		{"user_gone_home_gone_confirms", true, true, true},
		{"user_gone_home_survives_refuses", true, false, false},
		{"user_present_home_gone_refuses", false, true, false},
		{"user_present_home_survives_refuses", false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			udh001StubHost(t, username, tt.userGone, tt.homeGone)
			if got := destroyDeleteConfirmedOnHost(username); got != tt.want {
				t.Errorf("destroyDeleteConfirmedOnHost = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestMON_Monitoring_UserdelHome_MailSpoolWording_Narrow pins the wording
// classifier: the mail-spool shape matches, the genuine absent-user
// wordings stay OUT of it (they belong to destroyUserAbsentOutput), and
// non-matching output does not match.
func TestMON_Monitoring_UserdelHome_MailSpoolWording_Narrow(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want bool
	}{
		{"verbatim_mail_spool_shape", "userdel: bunker-x mail spool not found\n", true},
		{"capitalized", "userdel: user bunker-x mail spool not found\n", true},
		{"genuine_absent_user", "userdel: user bunker-x does not exist\n", false},
		{"no_such_user", "userdel: no such user bunker-x\n", false},
		{"busy_home", "userdel: user bunker-x is currently used by process 1234\n", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := destroyMailSpoolWarningOutput([]byte(tt.out)); got != tt.want {
				t.Errorf("destroyMailSpoolWarningOutput(%q) = %v, want %v", tt.out, got, tt.want)
			}
			// The mail-spool shape must ALSO match the conservative absent
			// classifier when the host probe is not consulted — that is the
			// historical behavior the refusal guard (B) now backstops.
			if strings.Contains(strings.ToLower(tt.out), "mail spool") && strings.Contains(strings.ToLower(tt.out), "not found") {
				if !destroyUserAbsentOutput([]byte(tt.out)) {
					t.Errorf("mail-spool shape lost its conservative classifier coverage")
				}
			}
		})
	}
}
