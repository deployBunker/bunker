package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// NewExecCommand returns the `bunker exec` cobra command.
func NewExecCommand() *cobra.Command {
	var (
		serverName string
		agentID    string
		timeout    uint32
		rawMode    bool
		scriptPath string
		stdinPath  string
		base64Out  bool
		execCap    uint64
	)

	// execFlagSpecs is the single accepted-flag table for `bunker exec`:
	// the seven exec flags plus the root command's persistent flags. Every
	// declared exec flag must appear here (TestExecAcceptsEveryDeclaredFlag
	// walks the declared flags end-to-end) and every root persistent flag
	// must appear here (the cmd/bunker root-tree test walks
	// root.PersistentFlags() through the real `bunker exec` invocation).
	// The root persistent flags must be APPLIED HERE: with
	// DisableFlagParsing cobra never parses them for exec, so the root
	// PersistentPreRun transfer runs with an empty value and a peeled
	// --config would otherwise be accepted and silently ignored.
	execGrammar := flagGrammar{name: "exec", specs: map[string]flagGrammarSpec{
		"--server": {apply: func(v string) error { serverName = v; return nil }},
		"--timeout": {apply: func(v string) error {
			if n, err := strconv.ParseUint(v, 10, 32); err == nil {
				timeout = uint32(n)
			}
			return nil
		}},
		"--raw":    {apply: func(string) error { rawMode = true; return nil }, boolean: true},
		"--script": {apply: func(v string) error { scriptPath = v; return nil }},
		"--stdin":  {apply: func(v string) error { stdinPath = v; return nil }},
		"--base64": {apply: func(string) error { base64Out = true; return nil }, boolean: true},
		"--exec-cap": {apply: func(v string) error {
			if n, err := strconv.ParseUint(v, 10, 64); err == nil {
				execCap = n
			}
			return nil
		}},
		"--config":        {apply: func(v string) error { SetConfigPathOverride(v); return nil }},
		"--daemon-config": {apply: func(v string) error { SetDaemonConfigPathOverride(v); return nil }},
	}}

	cmd := &cobra.Command{
		Use:   "exec <agent-id> [flags] [--] <command> [args...]",
		Short: "Execute a command in an agent's environment",
		Long: `Execute a command inside an agent's isolated environment via the bunkerd server.

The command runs as the agent's user (bunker-<agent-id>) with the agent's
Docker socket and environment variables available.

Use -- to separate bunker flags from the command to execute, so that Docker
flags such as --rm, --format, -d, and --name are not intercepted by the CLI.

Use --raw to bypass shell interpretation and pass the command and arguments
directly to execve on the remote host. This is useful for commands with
quotes, parentheses, or pipes that would otherwise need shell escaping.

Use --script <file> to upload a local script and execute it inside the agent.

The global persistent flags --config and --daemon-config are accepted in
every position, including before the agent-id:

  bunker exec --config ./cfg.yaml abc12345 -- docker ps

Any other flag before the agent-id is rejected before anything is sent to
the server:

  exec takes no flags before <agent-id> (got "--flag")

Examples:
  bunker exec abc12345 docker ps
  bunker exec abc12345 -- docker run --rm hello-world
  bunker exec abc12345 -- ls -la /home
  bunker exec abc12345 --timeout 60 -- docker build -t myapp .
  bunker exec abc12345 --raw -- docker ps --format '{{.Names}}'
  bunker exec abc12345 --script ./migrate.sh
  bunker exec abc12345 --raw -- psql -c 'SELECT count(*) FROM pg_catalog.pg_tables'
  bunker exec --server prod --timeout 60 abc12345 -- docker ps
  bunker exec --raw abc12345 -- psql -c 'SELECT count(*) FROM pg_catalog.pg_tables'`,

		RunE: func(cmd *cobra.Command, args []string) error {
			// With DisableFlagParsing, --help is passed as an argument. Detect it
			// early so the help output prints instead of running the command.
			for _, a := range args {
				if a == "--help" || a == "-h" {
					return cmd.Help()
				}
			}
			if len(args) < 1 {
				return fmt.Errorf("requires at least 1 arg(s), only received %d", len(args))
			}

			// Peel flags that appear BEFORE the agent-id. Cobra Find strips
			// global flag pairs when locating the subcommand only for commands
			// with flag parsing enabled; exec disables parsing, so anything
			// typed before "exec" lands here in args (e.g.
			// "bunker --server prod exec abc123 -- ..." arrives as
			// ["--server", "prod", "abc123", "--", ...]). The shared
			// flagGrammar accepts the FULL flag set — the exec flags plus
			// the root command's persistent flags (--config/--daemon-config,
			// which exec must apply itself: DisableFlagParsing means cobra
			// never parses them and the root PersistentPreRun transfer
			// sees an empty value) — here and after the agent-id, each in
			// the space form (--server X) and the inline form (--server=X),
			// so the global position behaves like it does for
			// spawn/status/list/info/audit. Any other flag-like token is
			// rejected with an actionable error instead of becoming the
			// agent-id and dying as a server-side not_found.
			args, perr := execGrammar.peelHead(args)
			if perr != nil {
				return perr
			}
			if len(args) < 1 {
				return fmt.Errorf("agent-id required after flags")
			}

			// After cobra parsing, args contains everything after the subcommand
			// because DisableFlagParsing is true. We manually peel off the
			// leading -- if present, then extract bunker flags before the command.
			agentID = args[0]
			rest := args[1:]
			if len(rest) > 0 && rest[0] == "--" {
				rest = rest[1:]
			}
			// A script-only exec sends no command token (the server runs the
			// uploaded script), so only require a command when no script flag
			// was peeled above.
			if len(rest) == 0 && scriptPath == "" {
				return fmt.Errorf("command required after agent-id")
			}
			// Parse our own flags from the head of rest. Anything after the
			// command token is left untouched so Docker flags pass through.
			// The shared grammar's LENIENT peel stops at the first token
			// that is not an accepted flag (unknown or dangling), so
			// `docker run --rm` is never eaten — only the pre-agent-id
			// peel refuses.
			rest = execGrammar.peelLeading(rest)
			// The flag loop stops at the first non-flag token. If that token
			// is the "--" separator, skip it so it is not sent as the command.
			if len(rest) > 0 && rest[0] == "--" {
				rest = rest[1:]
			}
			if len(rest) == 0 && scriptPath == "" {
				return fmt.Errorf("command required after agent-id")
			}
			command := ""
			var commandArgs []string
			if len(rest) > 0 {
				command = rest[0]
				commandArgs = rest[1:]
			}

			// If a script is specified, read it locally and send it as script_content.
			var scriptContent string
			if scriptPath != "" {
				data, err := os.ReadFile(scriptPath)
				if err != nil {
					return fmt.Errorf("read script %s: %w", scriptPath, err)
				}
				scriptContent = string(data)
			}

			// 1. Load CLI config
			cfg, err := LoadCLIConfig()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			// 2. Determine server
			// Fail-closed binding (GAP-093): mutating commands never fall
			// back to the shared active_server default.
			resolved, berr := SessionScopedTarget(serverName, cfg.ActiveServer)
			if berr != nil {
				return berr
			}
			serverName = resolved

			entry, ok := cfg.Servers[serverName]
			if !ok {
				return fmt.Errorf("server %q not found in config", serverName)
			}

			// 3. Build request
			client := newBunkerdClient(entry)
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
			defer cancel()

			stdinPayload, err := readStdinPayload(stdinPath)
			if err != nil {
				return err
			}
			encoding := v1.ExecEncoding_EXEC_ENCODING_TEXT
			if base64Out {
				encoding = v1.ExecEncoding_EXEC_ENCODING_BASE64
			}
			req := connect.NewRequest(&v1.ExecAgentRequest{
				AgentId:          agentID,
				Command:          command,
				Args:             commandArgs,
				TimeoutSeconds:   timeout,
				Raw:              rawMode,
				ScriptContent:    scriptContent,
				StdinPayload:     stdinPayload,
				ResponseEncoding: encoding,
				ResponseCapBytes: execCap,
			})

			// Auth token
			token := resolveToken(entry)
			if token != "" {
				req.Header().Set("Authorization", "Bearer "+token)
			}

			// 4. Call RPC (streaming)
			stream, err := client.ExecAgent(ctx, req)
			if err != nil {
				return fmt.Errorf("exec agent: %w", err)
			}

			// 5. Stream output
			var exitCode int32
			for stream.Receive() {
				msg := stream.Msg()
				if msg.GetStdout() != nil {
					fmt.Print(string(msg.GetStdout()))
				}
				if msg.GetStderr() != nil {
					fmt.Fprint(cmd.ErrOrStderr(), string(msg.GetStderr()))
				}
				if msg.ExitCode != 0 {
					exitCode = msg.ExitCode
				}
			}
			if err := stream.Err(); err != nil {
				return fmt.Errorf("stream error: %w", err)
			}

			if exitCode != 0 {
				return &ExitError{Code: int(exitCode)}
			}
			return nil
		},
		DisableFlagParsing: true,
	}

	// We disabled flag parsing so that Docker flags can pass through. The flags
	// are still declared for help output and so that flag-aware tooling can see
	// them; runtime parsing is done manually in RunE.
	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (default: active server)")
	cmd.Flags().Uint32Var(&timeout, "timeout", 30, "Command timeout in seconds")
	cmd.Flags().BoolVar(&rawMode, "raw", false, "Bypass shell interpretation and pass command directly to execve")
	cmd.Flags().StringVar(&scriptPath, "script", "", "Upload and execute a local script file")
	cmd.Flags().StringVar(&stdinPath, "stdin", "", "Send this local file to the command's stdin ('-' reads bunker's own stdin)")
	cmd.Flags().BoolVar(&base64Out, "base64", false, "Base64-encode the response so binary bytes survive text-only transports")
	cmd.Flags().Uint64Var(&execCap, "exec-cap", 0, "Max response bytes per direction (server default 512MiB; may only lower, never raise)")

	return cmd
}

// readStdinPayload loads the --stdin payload: a file path, or "-" to pass
// bunker's own stdin through. Empty when no --stdin was given.
func readStdinPayload(path string) ([]byte, error) {
	switch path {
	case "":
		return nil, nil
	case "-":
		return io.ReadAll(os.Stdin)
	default:
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read --stdin %s: %w", path, err)
		}
		return b, nil
	}
}
