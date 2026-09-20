package cli

// DF-BUNKER-31: the `bunker exec` flag grammar. Both peelers (pre- and
// post-agent-id) accept the FULL flag set — the seven exec flags plus the
// root command's persistent flags — in BOTH the space form (--flag value)
// and the inline form (--flag=value). Unknown flags refuse LOCALLY (zero
// RPCs) in the pre-agent-id position only. A peeled --config /
// --daemon-config must be APPLIED, not just accepted: DisableFlagParsing
// means cobra never parses them for exec, so the peeler itself routes them
// to SetConfigPathOverride / SetDaemonConfigPathOverride.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	"github.com/spf13/pflag"
	yaml "go.yaml.in/yaml/v3"
)

// writeExecTestConfigAt writes a CLI config at an EXPLICIT path with a
// single server under the given name. --config rows use a DIFFERENT server
// name than the default config ("custom-default" vs "default"), so a
// successful exec proves the override was applied — the default config
// cannot resolve that name.
func writeExecTestConfigAt(t *testing.T, path, serverName, serverURL string) {
	t.Helper()
	cfg := &CLIConfig{
		Servers: map[string]ServerEntry{
			serverName: {
				Name:        serverName,
				URL:         serverURL,
				ConnectedAt: "2026-09-20T00:00:00Z",
			},
		},
		ActiveServer: serverName,
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write config %s: %v", path, err)
	}
}

// TestExecFlagGrammar is the DF-BUNKER-31 table: every accepted flag, both
// forms, both positions, the applied-override wiring, and the refusal
// contracts. Success rows drive a real mock server and inspect the sent
// ExecAgentRequest; error rows assert the EXACT local error and that the
// server received zero requests.
func TestExecFlagGrammar(t *testing.T) {
	rows := []struct {
		name          string
		args          []string // {CONFIG} -> custom config path, {SCRIPT} -> temp script
		sessionEnv    string   // BUNKER_SESSION_TARGET value ("" = leave unset)
		scriptBody    string   // if set, written to a temp file replacing {SCRIPT}
		wantErr       string   // expected error substring; "" = expect success
		wantNoRequest bool     // expect the server to receive NO request
		wantCommand   string
		wantArgs      []string
		wantTimeout   uint32
		wantRaw       bool
		wantBase64    bool
		wantScript    string // expected ScriptContent; "" = expect empty
		wantStdinRel  string // expected StdinPayload; "{CONFIG}" = the custom config file's bytes
		wantExecCap   uint64
		wantCfgApp    bool // after Execute: configPathOverride must be the custom config path
		wantDaemonApp bool // after Execute: daemonConfigPathOverride must be the custom config path
	}{
		// -- Acceptance A: the dogfood bug — --config before the agent-id.
		{
			name:        "config space form before agent-id reaches server",
			args:        []string{"--config", "{CONFIG}", "agent1", "--", "docker", "ps"},
			sessionEnv:  "custom-default", // only resolvable via the custom config
			wantCommand: "docker",
			wantArgs:    []string{"ps"},
			wantTimeout: 30,
			wantCfgApp:  true,
		},
		{
			name:        "config inline form before agent-id reaches server",
			args:        []string{"--config={CONFIG}", "agent1", "--", "docker", "ps"},
			sessionEnv:  "custom-default",
			wantCommand: "docker",
			wantArgs:    []string{"ps"},
			wantTimeout: 30,
			wantCfgApp:  true,
		},
		{
			name:        "config after agent-id reaches server",
			args:        []string{"agent1", "--config", "{CONFIG}", "--", "docker", "ps"},
			sessionEnv:  "custom-default",
			wantCommand: "docker",
			wantArgs:    []string{"ps"},
			wantTimeout: 30,
			wantCfgApp:  true,
		},
		{
			name:          "daemon-config space form before agent-id is applied",
			args:          []string{"--daemon-config", "{CONFIG}", "agent1", "--", "docker", "ps"},
			sessionEnv:    "default",
			wantCommand:   "docker",
			wantArgs:      []string{"ps"},
			wantTimeout:   30,
			wantDaemonApp: true,
		},
		{
			name:          "daemon-config inline form before agent-id is applied",
			args:          []string{"--daemon-config={CONFIG}", "agent1", "--", "docker", "ps"},
			sessionEnv:    "default",
			wantCommand:   "docker",
			wantArgs:      []string{"ps"},
			wantTimeout:   30,
			wantDaemonApp: true,
		},

		// -- Remaining exec flags in the pre-agent-id position (the new
		//    grammar), each form verified and the value observed in the
		//    actual request.
		{
			name:         "stdin space form before agent-id",
			args:         []string{"--stdin", "{CONFIG}", "agent1", "--", "cat"},
			sessionEnv:   "default",
			wantCommand:  "cat",
			wantArgs:     []string{},
			wantTimeout:  30,
			wantStdinRel: "{CONFIG}",
		},
		{
			name:         "stdin inline form before agent-id",
			args:         []string{"--stdin={CONFIG}", "agent1", "--", "cat"},
			sessionEnv:   "default",
			wantCommand:  "cat",
			wantArgs:     []string{},
			wantTimeout:  30,
			wantStdinRel: "{CONFIG}",
		},
		{
			name:        "base64 space form before agent-id",
			args:        []string{"--base64", "agent1", "--", "docker", "ps"},
			sessionEnv:  "default",
			wantCommand: "docker",
			wantArgs:    []string{"ps"},
			wantTimeout: 30,
			wantBase64:  true,
		},
		{
			name:        "base64 inline form before agent-id",
			args:        []string{"--base64=true", "agent1", "--", "docker", "ps"},
			sessionEnv:  "default",
			wantCommand: "docker",
			wantArgs:    []string{"ps"},
			wantTimeout: 30,
			wantBase64:  true,
		},
		{
			name:        "exec-cap space form before agent-id",
			args:        []string{"--exec-cap", "1048576", "agent1", "--", "docker", "logs", "x"},
			sessionEnv:  "default",
			wantCommand: "docker",
			wantArgs:    []string{"logs", "x"},
			wantTimeout: 30,
			wantExecCap: 1048576,
		},
		{
			name:        "exec-cap inline form before agent-id",
			args:        []string{"--exec-cap=1048576", "agent1", "--", "docker", "logs", "x"},
			sessionEnv:  "default",
			wantCommand: "docker",
			wantArgs:    []string{"logs", "x"},
			wantTimeout: 30,
			wantExecCap: 1048576,
		},
		{
			name:        "server inline form before agent-id",
			args:        []string{"--server=default", "agent1", "--", "docker", "ps"},
			wantCommand: "docker",
			wantArgs:    []string{"ps"},
			wantTimeout: 30,
		},
		{
			name:        "timeout inline form before agent-id",
			args:        []string{"--timeout=120", "agent1", "--", "sleep", "1"},
			sessionEnv:  "default",
			wantCommand: "sleep",
			wantArgs:    []string{"1"},
			wantTimeout: 120,
		},
		{
			name:        "raw inline form before agent-id",
			args:        []string{"--raw=true", "agent1", "--", "docker", "ps"},
			sessionEnv:  "default",
			wantCommand: "docker",
			wantArgs:    []string{"ps"},
			wantTimeout: 30,
			wantRaw:     true,
		},
		{
			name:        "script inline form before agent-id",
			args:        []string{"--script={SCRIPT}", "agent1"},
			sessionEnv:  "default",
			scriptBody:  "#!/bin/sh\necho inline-script-grammar",
			wantCommand: "",
			wantArgs:    []string{},
			wantTimeout: 30,
			wantScript:  "#!/bin/sh\necho inline-script-grammar",
		},
		{
			name: "all seven exec flags before agent-id in space form",
			args: []string{
				"--server", "default", "--timeout", "61", "--raw",
				"--stdin", "{CONFIG}", "--base64", "--exec-cap", "4096",
				"--script", "{SCRIPT}", "agent1", "--", "echo", "hi",
			},
			scriptBody:   "#!/bin/sh\necho all-flags",
			wantCommand:  "echo",
			wantArgs:     []string{"hi"},
			wantTimeout:  61,
			wantRaw:      true,
			wantBase64:   true,
			wantExecCap:  4096,
			wantScript:   "#!/bin/sh\necho all-flags",
			wantStdinRel: "{CONFIG}",
		},
		{
			name: "persistent and exec flags mixed before agent-id",
			args: []string{
				"--config", "{CONFIG}", "--base64", "--exec-cap=2048",
				"agent1", "--timeout", "45", "--", "docker", "ps",
			},
			sessionEnv:  "custom-default",
			wantCommand: "docker",
			wantArgs:    []string{"ps"},
			wantTimeout: 45,
			wantBase64:  true,
			wantExecCap: 2048,
			wantCfgApp:  true,
		},

		// -- Requirement 4: "--" before the agent-id still terminates flag
		//    parsing; the next token is the agent-id even if flag-shaped.
		{
			name:        "separator before agent-id makes --config a plain token",
			args:        []string{"--", "--config", "agent1", "docker", "ps"},
			sessionEnv:  "default",
			wantCommand: "agent1",
			wantArgs:    []string{"docker", "ps"},
			wantTimeout: 30,
		},

		// -- Requirement 3: unknown flags refuse LOCALLY, naming the flag.
		{
			name:          "unknown flag before agent-id refuses naming it",
			args:          []string{"--bogus", "agent1", "--", "docker", "ps"},
			wantErr:       `exec takes no flags before <agent-id> (got "--bogus")`,
			wantNoRequest: true,
		},
		{
			name:          "prefix of a known flag is not silently accepted",
			args:          []string{"--conf", "{CONFIG}", "agent1", "--", "docker", "ps"},
			wantErr:       `exec takes no flags before <agent-id> (got "--conf")`,
			wantNoRequest: true,
		},
		{
			name:          "root-local --version is not an exec flag",
			args:          []string{"--version", "agent1", "--", "docker", "ps"},
			wantErr:       `exec takes no flags before <agent-id> (got "--version")`,
			wantNoRequest: true,
		},

		// -- Requirement 5 / acceptance E: value-taking flag with no value.
		{
			name:          "config without value needs an argument",
			args:          []string{"--config"},
			wantErr:       "flag needs an argument: --config",
			wantNoRequest: true,
		},
		{
			name:          "daemon-config without value needs an argument",
			args:          []string{"--daemon-config"},
			wantErr:       "flag needs an argument: --daemon-config",
			wantNoRequest: true,
		},
		{
			name:          "stdin without value needs an argument",
			args:          []string{"--stdin"},
			wantErr:       "flag needs an argument: --stdin",
			wantNoRequest: true,
		},
		{
			name:          "exec-cap without value needs an argument",
			args:          []string{"--exec-cap"},
			wantErr:       "flag needs an argument: --exec-cap",
			wantNoRequest: true,
		},
		{
			name:          "timeout without value needs an argument",
			args:          []string{"--timeout"},
			wantErr:       "flag needs an argument: --timeout",
			wantNoRequest: true,
		},
		{
			name:          "flags but no agent-id",
			args:          []string{"--config", "{CONFIG}"},
			wantErr:       "agent-id required after flags",
			wantNoRequest: true,
		},

		// -- Post-agent-id position: break-based grammar preserved (req 5).
		{
			name:        "docker run --rm never eaten",
			args:        []string{"agent1", "--", "docker", "run", "--rm", "hi"},
			sessionEnv:  "default",
			wantCommand: "docker",
			wantArgs:    []string{"run", "--rm", "hi"},
			wantTimeout: 30,
		},
		{
			name:        "unknown token after agent-id is the command",
			args:        []string{"agent1", "--", "--bogus", "x"},
			sessionEnv:  "default",
			wantCommand: "--bogus",
			wantArgs:    []string{"x"},
			wantTimeout: 30,
		},
		{
			name:        "dangling value-taking flag after agent-id is the command",
			args:        []string{"agent1", "--stdin"},
			sessionEnv:  "default",
			wantCommand: "--stdin",
			wantArgs:    []string{},
			wantTimeout: 30,
		},
	}

	for _, tt := range rows {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			t.Setenv("HOME", tmpDir)
			resetPathOverrides(t)

			var got *v1.ExecAgentRequest
			requests := 0
			server := newExecTestServer(t, &mockExecServer{
				execResponses: []*v1.ExecAgentResponse{
					{Output: &v1.ExecAgentResponse_Stdout{Stdout: []byte("ok")}},
					{ExitCode: 0},
				},
				captureReq: func(req *v1.ExecAgentRequest) {
					requests++
					got = req
				},
			})
			defer server.Close()

			// Default config at the default location; custom config at an
			// explicit path with a DIFFERENT active server so a success
			// proves --config was applied, not merely accepted.
			writeExecTestConfig(t, tmpDir, server.URL)
			cfgPath := filepath.Join(tmpDir, "custom.yaml")
			writeExecTestConfigAt(t, cfgPath, "custom-default", server.URL)

			args := append([]string(nil), tt.args...)
			if tt.sessionEnv != "" {
				t.Setenv(SessionTargetEnvVar, tt.sessionEnv)
			}
			if tt.scriptBody != "" {
				scriptFile := filepath.Join(tmpDir, "script.sh")
				if err := os.WriteFile(scriptFile, []byte(tt.scriptBody), 0o644); err != nil {
					t.Fatalf("write script: %v", err)
				}
				for i, a := range args {
					if strings.Contains(a, "{SCRIPT}") {
						args[i] = strings.ReplaceAll(a, "{SCRIPT}", scriptFile)
					}
				}
			}
			for i, a := range args {
				if strings.Contains(a, "{CONFIG}") {
					args[i] = strings.ReplaceAll(a, "{CONFIG}", cfgPath)
				}
			}

			cmd := NewExecCommand()
			cmd.SetArgs(args)
			err := cmd.Execute()

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got: %v", tt.wantErr, err)
				}
			} else if err != nil {
				t.Fatalf("exec command failed: %v", err)
			}

			if tt.wantNoRequest {
				if requests != 0 {
					t.Fatalf("server received %d request(s), want 0", requests)
				}
				return
			}
			if requests != 1 {
				t.Fatalf("server received %d request(s), want 1", requests)
			}
			if got == nil {
				t.Fatal("request not captured")
			}
			if got.Command != tt.wantCommand {
				t.Errorf("command = %q, want %q", got.Command, tt.wantCommand)
			}
			if len(got.Args) != len(tt.wantArgs) {
				t.Fatalf("args = %v, want %v", got.Args, tt.wantArgs)
			}
			for i, want := range tt.wantArgs {
				if got.Args[i] != want {
					t.Errorf("args[%d] = %q, want %q", i, got.Args[i], want)
				}
			}
			if got.TimeoutSeconds != tt.wantTimeout {
				t.Errorf("TimeoutSeconds = %d, want %d", got.TimeoutSeconds, tt.wantTimeout)
			}
			if got.Raw != tt.wantRaw {
				t.Errorf("Raw = %v, want %v", got.Raw, tt.wantRaw)
			}
			wantEncoding := v1.ExecEncoding_EXEC_ENCODING_TEXT
			if tt.wantBase64 {
				wantEncoding = v1.ExecEncoding_EXEC_ENCODING_BASE64
			}
			if got.ResponseEncoding != wantEncoding {
				t.Errorf("ResponseEncoding = %v, want %v", got.ResponseEncoding, wantEncoding)
			}
			if got.ResponseCapBytes != tt.wantExecCap {
				t.Errorf("ResponseCapBytes = %d, want %d", got.ResponseCapBytes, tt.wantExecCap)
			}
			if tt.wantScript != "" && got.ScriptContent != tt.wantScript {
				t.Errorf("ScriptContent = %q, want %q", got.ScriptContent, tt.wantScript)
			}
			if tt.wantStdinRel != "" {
				wantPayload, rerr := os.ReadFile(cfgPath)
				if rerr != nil {
					t.Fatalf("read custom config for stdin comparison: %v", rerr)
				}
				if string(got.StdinPayload) != string(wantPayload) {
					t.Errorf("StdinPayload = %d bytes, want the custom config file's %d bytes",
						len(got.StdinPayload), len(wantPayload))
				}
			}
			// Wiring proof: the peelers must APPLY the persistent flags via
			// the cli setters (read back before resetPathOverrides cleanup).
			if tt.wantCfgApp && configPathOverride != cfgPath {
				t.Errorf("configPathOverride = %q, want %q (peeled --config not applied)",
					configPathOverride, cfgPath)
			}
			if tt.wantDaemonApp && daemonConfigPathOverride != cfgPath {
				t.Errorf("daemonConfigPathOverride = %q, want %q (peeled --daemon-config not applied)",
					daemonConfigPathOverride, cfgPath)
			}
		})
	}
}

// TestExecAcceptsEveryDeclaredFlag walks the flags the command actually
// DECLARES (help surface) and requires each one to be peelable from the
// pre-agent-id position end-to-end. A flag declared for help but missing
// from execFlagSpecs fails here: it would be rejected with "exec takes no
// flags before <agent-id>" despite being advertised.
func TestExecAcceptsEveryDeclaredFlag(t *testing.T) {
	cmd := NewExecCommand()
	var declared []string
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		declared = append(declared, f.Name)
	})
	if len(declared) == 0 {
		t.Fatal("exec declares no flags")
	}

	for _, name := range declared {
		t.Run(name, func(t *testing.T) {
			tmpDir := t.TempDir()
			t.Setenv("HOME", tmpDir)
			resetPathOverrides(t)

			requests := 0
			server := newExecTestServer(t, &mockExecServer{
				execResponses: []*v1.ExecAgentResponse{
					{Output: &v1.ExecAgentResponse_Stdout{Stdout: []byte("ok")}},
					{ExitCode: 0},
				},
				captureReq: func(req *v1.ExecAgentRequest) { requests++ },
			})
			defer server.Close()

			writeExecTestConfig(t, tmpDir, server.URL)
			cfgPath := filepath.Join(tmpDir, "custom.yaml")
			writeExecTestConfigAt(t, cfgPath, "custom-default", server.URL)

			var flagArgs []string
			switch name {
			case "server":
				flagArgs = []string{"--server", "default"}
			case "timeout":
				flagArgs = []string{"--timeout", "30"}
			case "raw", "base64":
				flagArgs = []string{"--" + name}
			case "script":
				scriptFile := filepath.Join(tmpDir, "walk.sh")
				if err := os.WriteFile(scriptFile, []byte("#!/bin/sh\nexit 0"), 0o644); err != nil {
					t.Fatalf("write script: %v", err)
				}
				flagArgs = []string{"--script", scriptFile}
			case "stdin":
				flagArgs = []string{"--stdin", "/dev/null"}
			case "exec-cap":
				flagArgs = []string{"--exec-cap", "1024"}
			case "config":
				flagArgs = []string{"--config", cfgPath}
			case "daemon-config":
				flagArgs = []string{"--daemon-config", cfgPath}
			default:
				t.Fatalf("exec declares flag %q but this walk does not know it: "+
					"add it here AND to execFlagSpecs in exec.go", name)
			}

			// --config rows can only succeed when the peeled override is
			// APPLIED: the target server name exists only in the custom file.
			if name == "config" {
				t.Setenv(SessionTargetEnvVar, "custom-default")
			} else {
				t.Setenv(SessionTargetEnvVar, "default")
			}

			cmd = NewExecCommand()
			cmd.SetArgs(append(flagArgs, "agent1", "--", "docker", "ps"))
			if err := cmd.Execute(); err != nil {
				t.Fatalf("declared flag --%s rejected before the agent-id: %v", name, err)
			}
			if requests != 1 {
				t.Fatalf("server received %d request(s), want 1", requests)
			}
		})
	}
}
