package cli

// SURF-011: `bunker exec` deadline failures must surface an actionable
// hint naming --timeout and the elapsed budget, without changing the
// error's semantics (the command still fails non-zero). The flag default
// itself (execDefaultTimeoutSeconds) is pinned by TestExecFlagGrammar.

import (
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// TestExecDeadlineHintEndToEnd drives the REAL `bunker exec` command
// against a mock server whose ExecAgent handler fails, and asserts the
// decoration contract on the error the user actually sees: deadline-class
// errors name --timeout and the effective budget; other errors do not.
func TestExecDeadlineHintEndToEnd(t *testing.T) {
	rows := []struct {
		name       string
		execErr    error
		execResps  []*v1.ExecAgentResponse
		wantHint   bool
		wantBudget string // the budget the hint must echo; "" = no hint expected
	}{
		{
			name:       "deadline before any stream message surfaces the hint",
			execErr:    connect.NewError(connect.CodeDeadlineExceeded, errors.New("context deadline exceeded")),
			wantHint:   true,
			wantBudget: "1800",
		},
		{
			name:       "deadline after a stream message surfaces the hint",
			execErr:    connect.NewError(connect.CodeDeadlineExceeded, errors.New("context deadline exceeded")),
			execResps:  []*v1.ExecAgentResponse{{Output: &v1.ExecAgentResponse_Stdout{Stdout: []byte("partial")}}},
			wantHint:   true,
			wantBudget: "1800",
		},
		{
			name:     "normal stream error stays bare",
			execErr:  connect.NewError(connect.CodeInternal, errors.New("agent exploded")),
			wantHint: false,
		},
		{
			name:     "plain non-deadline error stays bare",
			execErr:  errors.New("something else failed"),
			wantHint: false,
		},
	}

	for _, tt := range rows {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			t.Setenv("HOME", tmpDir)
			t.Setenv(SessionTargetEnvVar, "default")

			server := newExecTestServer(t, &mockExecServer{
				execResponses: tt.execResps,
				execErr:       tt.execErr,
			})
			defer server.Close()
			writeExecTestConfig(t, tmpDir, server.URL)

			cmd := NewExecCommand()
			cmd.SetArgs([]string{"agent1", "--", "docker", "ps"})
			err := cmd.Execute()
			if err == nil {
				t.Fatal("expected exec to fail, got nil")
			}

			hasHint := strings.Contains(err.Error(), "--timeout") &&
				strings.Contains(err.Error(), "exec deadline exceeded after "+tt.wantBudget+"s")
			if tt.wantHint && !hasHint {
				t.Fatalf("deadline error missing actionable hint (--timeout, %ss budget): %v",
					tt.wantBudget, err)
			}
			if !tt.wantHint && hasHint {
				t.Fatalf("non-deadline error unexpectedly carries the hint: %v", err)
			}
		})
	}
}
