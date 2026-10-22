package config

// ISO-001 config tests: the proc group the daemon holds on the real /proc
// mount. The env tier (BUNKERD_PROCVIS_PROC_GROUP) overrides the config
// file; an unnamed group is a resolution failure (never a silent
// group-less posture), and Config.Validate fails boot on a malformed
// CONFIGURED group while keeping the empty shape legitimate.

import (
	"strings"
	"testing"
)

func TestProcVisConfigProcGroupResolved(t *testing.T) {
	tests := []struct {
		name      string
		procGroup string
		env       string
		want      string
		wantErr   bool
	}{
		{name: "empty pair is an error", procGroup: "", env: "", wantErr: true},
		{name: "config value resolves", procGroup: "bunker-proc", env: "", want: "bunker-proc"},
		{name: "config value trimmed", procGroup: "  bunker-proc  ", env: "", want: "bunker-proc"},
		{name: "env overrides config", procGroup: "bunker-proc", env: "bunker-proc-2", want: "bunker-proc-2"},
		{
			name:      "whitespace-only env does NOT override",
			procGroup: "bunker-proc", env: "   ",
			want: "bunker-proc",
		},
		{
			name:      "whitespace-only config is an error, never a silent group-less mount",
			procGroup: "   ", env: "",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(ProcVisProcGroupEnv, tt.env)
			got, err := (ProcVisConfig{ProcGroup: tt.procGroup}).ProcGroupResolved()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ProcGroupResolved() = %q, want error", got)
				}
				if !strings.Contains(err.Error(), "agent.procvis_proc_group") {
					t.Errorf("refusal %q missing the config key", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("ProcGroupResolved(): %v", err)
			}
			if got != tt.want {
				t.Errorf("resolved = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestProcVisConfigValidate(t *testing.T) {
	t.Run("empty is legitimate (the opt-out shape)", func(t *testing.T) {
		t.Setenv(ProcVisProcGroupEnv, "")
		if err := (ProcVisConfig{ProcGroup: ""}).Validate(); err != nil {
			t.Fatalf("empty config refused: %v", err)
		}
	})
	t.Run("configured token validates clean", func(t *testing.T) {
		t.Setenv(ProcVisProcGroupEnv, "")
		if err := (ProcVisConfig{ProcGroup: "bunker-proc"}).Validate(); err != nil {
			t.Fatalf("valid group refused: %v", err)
		}
	})
	t.Run("configured multi-token name refuses at boot", func(t *testing.T) {
		t.Setenv(ProcVisProcGroupEnv, "")
		err := (ProcVisConfig{ProcGroup: "two tokens"}).Validate()
		if err == nil {
			t.Fatal("a multi-token group name passed validation")
		}
		if !strings.Contains(err.Error(), "agent.procvis_proc_group") {
			t.Errorf("refusal %q missing the config key", err.Error())
		}
	})
	t.Run("env-only configuration validates clean", func(t *testing.T) {
		t.Setenv(ProcVisProcGroupEnv, "bunker-proc")
		if err := (ProcVisConfig{ProcGroup: ""}).Validate(); err != nil {
			t.Fatalf("env-only config refused: %v", err)
		}
	})
}

func TestProcVisConfigProcVisEnabled(t *testing.T) {
	t.Run("empty pair disables", func(t *testing.T) {
		t.Setenv(ProcVisProcGroupEnv, "")
		if (ProcVisConfig{ProcGroup: ""}).ProcVisEnabled() {
			t.Fatal("empty pair reports enabled")
		}
	})
	t.Run("config value enables", func(t *testing.T) {
		t.Setenv(ProcVisProcGroupEnv, "")
		if !(ProcVisConfig{ProcGroup: "bunker-proc"}).ProcVisEnabled() {
			t.Fatal("config value did not enable")
		}
	})
	t.Run("env value enables", func(t *testing.T) {
		t.Setenv(ProcVisProcGroupEnv, "bunker-proc")
		if !(ProcVisConfig{ProcGroup: ""}).ProcVisEnabled() {
			t.Fatal("env value did not enable")
		}
	})
}

// TestValidate_ProcVisWiredIntoBootValidation pins the wiring: the Agent's
// ProcVis block is REACHED by Config.Validate (a malformed configured group
// refuses to start through the full Config.Validate surface, not only the
// narrow method).
func TestValidate_ProcVisWiredIntoBootValidation(t *testing.T) {
	t.Setenv(ProcVisProcGroupEnv, "")
	cfg := DefaultConfig()
	cfg.Agent.ProcVis.ProcGroup = "two tokens"
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Config.Validate accepted a malformed configured proc group")
	}
	if !strings.Contains(err.Error(), "agent.procvis_proc_group") {
		t.Errorf("refusal %q missing the config key", err.Error())
	}
	// The valid shapes boot clean.
	cfg2 := DefaultConfig()
	cfg2.Agent.ProcVis.ProcGroup = "bunker-proc"
	if err := cfg2.Validate(); err != nil {
		t.Errorf("valid proc group refused to boot: %v", err)
	}
	cfg3 := DefaultConfig()
	if err := cfg3.Validate(); err != nil {
		t.Errorf("default (unnamed) proc group refused to boot: %v", err)
	}
}
