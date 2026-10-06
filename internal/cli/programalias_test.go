package cli

import (
	"strings"
	"testing"
)

func TestParseAliasMountFlags(t *testing.T) {
	cases := []struct {
		name    string
		specs   []string
		want    []string // rendered host[:container][:ro]
		wantErr bool
	}{
		{name: "host only", specs: []string{"/home/bunker-x/data"}, want: []string{"/home/bunker-x/data"}},
		{name: "host and container", specs: []string{"/home/bunker-x/data:/work"}, want: []string{"/home/bunker-x/data:/work"}},
		{name: "host ro", specs: []string{"/home/bunker-x/data:ro"}, want: []string{"/home/bunker-x/data:ro"}},
		{name: "host container ro", specs: []string{"/home/bunker-x/data:/work:ro"}, want: []string{"/home/bunker-x/data:/work:ro"}},
		{name: "empty container ro", specs: []string{"/home/bunker-x/data::ro"}, want: []string{"/home/bunker-x/data:ro"}},
		{name: "repeatable", specs: []string{"/a:/a", "/b:/b:ro"}, want: []string{"/a:/a", "/b:/b:ro"}},
		{name: "empty", specs: []string{""}, wantErr: true},
		{name: "too many parts", specs: []string{"/a:/b:ro:extra"}, wantErr: true},
		{name: "bad trailing component", specs: []string{"/a:/b:rw"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseAliasMountFlags(tc.specs)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseAliasMountFlags(%v) = %+v, want error", tc.specs, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseAliasMountFlags(%v): %v", tc.specs, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d (%+v)", len(got), len(tc.want), got)
			}
			for i, want := range tc.want {
				rendered := got[i].Host
				if got[i].Container != "" {
					rendered += ":" + got[i].Container
				}
				if got[i].ReadOnly {
					rendered += ":ro"
				}
				if rendered != want {
					t.Errorf("mount[%d] = %q, want %q", i, rendered, want)
				}
			}
		})
	}
}

// TestAliasCommandSurface pins the CLI shape an operator scripts against.
func TestAliasCommandSurface(t *testing.T) {
	cmd := NewAliasCommand()
	if cmd.Use != "alias" {
		t.Fatalf("Use = %q", cmd.Use)
	}
	names := map[string]bool{}
	for _, sub := range cmd.Commands() {
		names[sub.Name()] = true
	}
	for _, want := range []string{"list", "set", "delete"} {
		if !names[want] {
			t.Errorf("alias is missing the %q subcommand (have %v)", want, names)
		}
	}
	// The flag surface the docs promise.
	setCmd, _, err := cmd.Find([]string{"set"})
	if err != nil {
		t.Fatalf("find set: %v", err)
	}
	for _, flag := range []string{"image", "entrypoint", "mount", "network", "description", "server"} {
		if setCmd.Flags().Lookup(flag) == nil {
			t.Errorf("alias set is missing --%s", flag)
		}
	}
}

// TestAliasSetFailsFastOnBadInput proves a bad name/image/mount is refused
// LOCALLY — before any config load or RPC — naming the offending token.
func TestAliasSetFailsFastOnBadInput(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "bad name",
			args: []string{"set", "bad name", "--image", "mikefarah/yq:4"},
			want: "must start with a letter or digit",
		},
		{
			name: "path as name",
			args: []string{"set", "/usr/bin/yq", "--image", "mikefarah/yq:4"},
			want: "must start with a letter or digit",
		},
		{
			name: "missing image",
			args: []string{"set", "yq"},
			want: "--image is required",
		},
		{
			name: "bad image",
			args: []string{"set", "yq", "--image", "yq; rm -rf /"},
			want: "not a valid OCI reference",
		},
		{
			name: "relative mount",
			args: []string{"set", "yq", "--image", "mikefarah/yq:4", "--mount", "relative/path"},
			want: "must be absolute",
		},
		{
			name: "non-home mount",
			args: []string{"set", "yq", "--image", "mikefarah/yq:4", "--mount", "/etc"},
			want: "must be inside an agent home",
		},
		{
			name: "docker socket mount",
			args: []string{"set", "yq", "--image", "mikefarah/yq:4", "--mount", "/run/bunker/abc123"},
			want: "must not expose a docker socket",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := NewAliasCommand()
			cmd.SetArgs(tc.args)
			cmd.SetOut(&strings.Builder{})
			cmd.SetErr(&strings.Builder{})
			err := cmd.Execute()
			if err == nil {
				t.Fatalf("alias %v succeeded, want a local refusal", tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name the problem (%q)", err, tc.want)
			}
		})
	}
}
