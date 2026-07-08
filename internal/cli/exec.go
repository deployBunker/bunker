package cli

import (
	"context"
	"fmt"
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
	)

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

Examples:
  bunker exec abc12345 docker ps
  bunker exec abc12345 -- docker run --rm hello-world
  bunker exec abc12345 -- ls -la /home
  bunker exec abc12345 --timeout 60 -- docker build -t myapp .
  bunker exec abc12345 --raw -- docker ps --format '{{.Names}}'
  bunker exec abc12345 --script ./migrate.sh
  bunker exec abc12345 --raw -- psql -c 'SELECT count(*) FROM pg_catalog.pg_tables'`,

		RunE: func(cmd *cobra.Command, args []string) error {
			// With DisableFlagParsing, --help is passed as an argument. Detect it
			// early so the help output prints instead of running the command.
			for _, a := range args {
				if a == "--help" || a == "-h" {
					return cmd.Help()
				}
			}
			if len(args) < 2 {
				return fmt.Errorf("requires at least 2 arg(s), only received %d", len(args))
			}

			// After cobra parsing, args contains everything after the subcommand
			// because DisableFlagParsing is true. We manually peel off the
			// leading -- if present, then extract bunker flags before the command.
			agentID = args[0]
			rest := args[1:]
			if len(rest) > 0 && rest[0] == "--" {
				rest = rest[1:]
			}
			if len(rest) == 0 {
				return fmt.Errorf("command required after agent-id")
			}
			// Parse our own flags from the head of rest. Anything after the
			// command token is left untouched so Docker flags pass through.
			i := 0
			for i < len(rest) {
				switch rest[i] {
				case "--server":
					if i+1 < len(rest) {
						serverName = rest[i+1]
						i += 2
						continue
					}
				case "--timeout":
					if i+1 < len(rest) {
						if v, err := strconv.ParseUint(rest[i+1], 10, 32); err == nil {
							timeout = uint32(v)
						}
						i += 2
						continue
					}
				case "--raw":
					rawMode = true
					i += 1
					continue
				case "--script":
					if i+1 < len(rest) {
						scriptPath = rest[i+1]
						i += 2
						continue
					}
				}
				break
			}
			rest = rest[i:]
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
			if serverName == "" {
				serverName = cfg.ActiveServer
			}
			if serverName == "" {
				return fmt.Errorf("no active server; run 'bunker connect' first")
			}

			entry, ok := cfg.Servers[serverName]
			if !ok {
				return fmt.Errorf("server %q not found in config", serverName)
			}

			// 3. Build request
			client := newBunkerdClient(entry)
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
			defer cancel()

			req := connect.NewRequest(&v1.ExecAgentRequest{
				AgentId:        agentID,
				Command:        command,
				Args:           commandArgs,
				TimeoutSeconds: timeout,
				Raw:            rawMode,
				ScriptContent:  scriptContent,
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
				return fmt.Errorf("exit code %d", exitCode)
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

	return cmd
}
