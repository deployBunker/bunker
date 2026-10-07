package config

import (
	"strings"
	"testing"
)

func TestEgressValidateModes(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		wantErr bool
	}{
		{"empty is the unset default", "", false},
		{"open", "open", false},
		{"allowlist", "allowlist", false},
		{"none", "none", false},
		{"unknown refuses", "deny-all", true},
		{"case sensitive", "Open", true},
		{"whitespace padded accepted", " none ", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := EgressConfig{Mode: tt.mode}
			err := e.Validate()
			if tt.wantErr && err == nil {
				t.Fatalf("Validate(%q) passed, want refusal", tt.mode)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Validate(%q): %v", tt.mode, err)
			}
			if tt.wantErr && err != nil && !strings.Contains(err.Error(), "agent.egress.mode") {
				t.Errorf("error %q does not name the config key", err)
			}
		})
	}
}

func TestEgressValidateAllowlist(t *testing.T) {
	tests := []struct {
		name      string
		allowlist []string
		wantErr   bool
	}{
		{"empty allowlist passes", nil, false},
		{"cidrs pass", []string{"10.0.0.0/8", "192.168.0.0/16"}, false},
		{"ips pass", []string{"93.184.216.34"}, false},
		{"hostnames pass", []string{"proxy.example.internal"}, false},
		{"junk entry refuses", []string{"not a host"}, true},
		{"ip typo refuses", []string{"10.0.0.999"}, true},
		{"bad cidr refuses", []string{"10.0.0.0/64"}, true},
		{"error names the index", []string{"10.0.0.0/8", "bogus!"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := EgressConfig{Mode: "allowlist", Allowlist: tt.allowlist}
			err := e.Validate()
			if tt.wantErr && err == nil {
				t.Fatalf("Validate(%v) passed, want refusal", tt.allowlist)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if tt.wantErr && err != nil && !strings.Contains(err.Error(), "allowlist") {
				t.Errorf("error %q does not name the allowlist key", err)
			}
			// The index-naming case must name agent.egress.allowlist[1].
			if tt.name == "error names the index" && err != nil && !strings.Contains(err.Error(), "allowlist[1]") {
				t.Errorf("error %q does not name the offending index", err)
			}
		})
	}
}

func TestResolveEgressModePrecedence(t *testing.T) {
	tests := []struct {
		name      string
		requested string
		cfg       EgressConfig
		want      string
		wantErr   bool
	}{
		{"default when both unset", "", EgressConfig{}, EgressModeOpen, false},
		{"config wins when no request", "", EgressConfig{Mode: "none"}, EgressModeNone, false},
		{"request wins over config", "open", EgressConfig{Mode: "none"}, EgressModeOpen, false},
		{"unknown request refuses", "bogus", EgressConfig{}, "", true},
		{"unknown config refuses", "", EgressConfig{Mode: "bogus"}, "", true},
		{"allowlist rides config", "", EgressConfig{Mode: "allowlist", Allowlist: []string{"10.0.0.0/8"}}, EgressModeAllowlist, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Config{}
			c.Agent.Egress = tt.cfg
			got, err := c.ResolveEgressMode(tt.requested)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ResolveEgressMode(%q) = %+v, want refusal", tt.requested, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveEgressMode(%q): %v", tt.requested, err)
			}
			if got.Mode != tt.want {
				t.Errorf("mode = %q, want %q", got.Mode, tt.want)
			}
			// The allowlist is carried ONLY for allowlist mode.
			if tt.want == EgressModeAllowlist && len(got.Allowlist) != 1 {
				t.Errorf("allowlist = %v, want the config list", got.Allowlist)
			}
			if tt.want != EgressModeAllowlist && len(got.Allowlist) != 0 {
				t.Errorf("mode %q must carry no allowlist, got %v", tt.want, got.Allowlist)
			}
		})
	}
}

func TestConfigValidateRejectsUnknownEgressMode(t *testing.T) {
	// Requirement 5: invalid mode rejected at config validation — the
	// daemon must refuse to START with a typoed agent.egress.mode.
	c := DefaultConfig()
	c.Agent.Egress = EgressConfig{Mode: "blocked"}
	if err := c.Validate(); err == nil {
		t.Fatal("Config.Validate() passed with an unknown egress mode, want refusal")
	} else if !strings.Contains(err.Error(), "agent.egress.mode") {
		t.Errorf("error %q does not name the key", err)
	}
	// And with a malformed allowlist entry.
	c2 := DefaultConfig()
	c2.Agent.Egress = EgressConfig{Mode: "allowlist", Allowlist: []string{"nope invalid"}}
	if err := c2.Validate(); err == nil {
		t.Fatal("Config.Validate() passed with a malformed allowlist entry, want refusal")
	}
	// The default config (untouched egress) keeps validating clean.
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatalf("DefaultConfig().Validate(): %v (the safe default must pass)", err)
	}
}
