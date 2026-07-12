package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// envFilePath returns the canonical path of the env file for an agent.
// The file lives under /run/bunker/<agent-id>/, writable by the bunker user.
func envFilePath(agentID string) string {
	return fmt.Sprintf("/run/bunker/%s/env", agentID)
}

// shQuoteSingle wraps s in POSIX single quotes, escaping embedded single
// quotes with the standard `'\”` sequence. This is safe for use inside
// `sh -c '...'` arguments.
func shQuoteSingle(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// NewEnvCommand returns the `bunker env` cobra command. The command has
// subcommands for managing an agent's env file (`/run/bunker/<id>/env`):
//
//	bunker env set   <agent-id> <KEY=VALUE>  -- write or replace a variable
//	bunker env get   <agent-id> <KEY>         -- print a single variable
//	bunker env list  <agent-id>               -- print all variables
//	bunker env unset <agent-id> <KEY>         -- remove a variable
//
// All subcommands work by issuing an ExecAgent RPC against the agent — the
// actual file mutation happens in a shell on the agent host.
func NewEnvCommand() *cobra.Command {
	var (
		serverName string
		timeout    uint32 = 30
	)

	cmd := &cobra.Command{
		Use:           "env <set|get|list|unset> <agent-id> [args...]",
		Short:         "Manage an agent's persistent environment variables",
		SilenceUsage:  true,
		SilenceErrors: true,
		Long: `Manage environment variables for an agent by reading and writing
/run/bunker/<agent-id>/env on the agent host.

The env file is automatically sourced at the start of every ` + "`bunker exec`" + `
and ` + "`bunker exec --script`" + ` invocation, and at the start of every detached
` + "`bunker run --detach`" + ` command. Variables set with ` + "`bunker env set`" + `
therefore persist across exec and run invocations until either ` + "`bunker env unset`" + `
or the agent itself is destroyed.

The env file format is plain KEY=VALUE lines (no 'export' prefix). Values
must not contain newlines.

Subcommands:

  set <agent-id> <KEY=VALUE>   write (replace if exists, append if new)
  get <agent-id> <KEY>         print the value of a single variable
  list <agent-id>              print all variables in KEY=VALUE form
  unset <agent-id> <KEY>       remove a variable

Examples:
  bunker env set abcd DATABASE_URL=postgres://db.local/app
  bunker env get abcd DATABASE_URL
  bunker env list abcd
  bunker env unset abcd DATABASE_URL`,

		RunE: func(cmd *cobra.Command, args []string) error {
			for _, a := range args {
				if a == "--help" || a == "-h" {
					return cmd.Help()
				}
			}

			// Manual flag parsing for --server / --timeout (DisableFlagParsing is true).
			rest, err := extractEnvFlags(args, &serverName, &timeout)
			if err != nil {
				return err
			}
			if len(rest) < 2 {
				return fmt.Errorf("usage: bunker env <set|get|list|unset> <agent-id> [args...]\nreceived %d arg(s)", len(rest))
			}

			subcommand := rest[0]
			agentID := rest[1]
			tail := rest[2:]

			// Validate the subcommand payload BEFORE we load any config so that
			// a typo doesn't require a working server to surface. This keeps
			// `bunker env set <agent> KEY=` etc. easy to validate locally.
			var (
				shellCmd string
				failOn   bool
			)
			switch subcommand {
			case "set":
				if len(tail) != 1 {
					return fmt.Errorf("env set requires exactly one KEY=VALUE argument")
				}
				key, value, err := parseEnvAssignment(tail[0])
				if err != nil {
					return err
				}
				shellCmd = buildEnvSetCommand(envFilePath(agentID), key, value)
				failOn = true
			case "get":
				if len(tail) != 1 {
					return fmt.Errorf("env get requires exactly one KEY argument")
				}
				if err := validateEnvKey(tail[0]); err != nil {
					return err
				}
				shellCmd = buildEnvGetCommand(envFilePath(agentID), tail[0])
				failOn = false
			case "list":
				if len(tail) != 0 {
					return fmt.Errorf("env list takes no extra arguments")
				}
				shellCmd = buildEnvListCommand(envFilePath(agentID))
				failOn = true
			case "unset":
				if len(tail) != 1 {
					return fmt.Errorf("env unset requires exactly one KEY argument")
				}
				if err := validateEnvKey(tail[0]); err != nil {
					return err
				}
				shellCmd = buildEnvUnsetCommand(envFilePath(agentID), tail[0])
				failOn = true
			default:
				return fmt.Errorf("unknown env subcommand %q (want: set, get, list, unset)", subcommand)
			}

			// Now load config — only after we know the subcommand is well-formed.
			cfg, err := LoadCLIConfig()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
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

			client := newBunkerdClient(entry)
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
			defer cancel()
			token := resolveToken(entry)

			return streamEnvExec(ctx, cmd, client, agentID, shellCmd, token, failOn)
		},
		DisableFlagParsing: true,
	}

	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (default: active server)")
	cmd.Flags().Uint32Var(&timeout, "timeout", 30, "Command timeout in seconds")
	return cmd
}

// extractEnvFlags peels --server and --timeout (and their = forms) off the
// front of args, returning the remaining positional args. Empty/leftover
// values for --server and --timeout are treated as errors.
func extractEnvFlags(args []string, serverName *string, timeout *uint32) ([]string, error) {
	rest := append([]string(nil), args...)
	i := 0
	for i < len(rest) {
		switch {
		case rest[i] == "--server":
			if i+1 >= len(rest) {
				return nil, fmt.Errorf("--server requires a value")
			}
			*serverName = rest[i+1]
			i += 2
			continue
		case strings.HasPrefix(rest[i], "--server="):
			*serverName = strings.TrimPrefix(rest[i], "--server=")
			i++
			continue
		case rest[i] == "--timeout":
			if i+1 >= len(rest) {
				return nil, fmt.Errorf("--timeout requires a value")
			}
			v, err := parseUint32(rest[i+1])
			if err != nil {
				return nil, fmt.Errorf("--timeout: %w", err)
			}
			*timeout = v
			i += 2
			continue
		case strings.HasPrefix(rest[i], "--timeout="):
			v, err := parseUint32(strings.TrimPrefix(rest[i], "--timeout="))
			if err != nil {
				return nil, fmt.Errorf("--timeout: %w", err)
			}
			*timeout = v
			i++
			continue
		}
		break
	}
	return rest[i:], nil
}

// parseEnvAssignment splits a single token of the form KEY=VALUE into (key,
// value). The key must be a valid POSIX env name (validateEnvKey enforces
// this); the value may be empty.
func parseEnvAssignment(token string) (key, value string, err error) {
	eq := strings.IndexByte(token, '=')
	if eq <= 0 {
		return "", "", fmt.Errorf("invalid env assignment %q (expected KEY=VALUE)", token)
	}
	key = token[:eq]
	value = token[eq+1:]
	// Disallow embedded newlines so values stay single-line in the env file.
	if strings.ContainsAny(value, "\n") {
		return "", "", fmt.Errorf("env value for %q must not contain newlines", key)
	}
	if err := validateEnvKey(key); err != nil {
		return "", "", err
	}
	return key, value, nil
}

// validateEnvKey enforces the standard POSIX env var name rules: leading
// non-digit, then alphanumerics or underscore. This is the only validation
// we trust on the agent side too (the server-side shell will execute the
// key as-is in our constructed shell snippets, so we rely on Go-side
// validation rather than runtime escaping).
func validateEnvKey(key string) error {
	if key == "" {
		return fmt.Errorf("env key is empty")
	}
	for i, r := range key {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return fmt.Errorf("invalid env key %q (must match [A-Za-z_][A-Za-z0-9_]*)", key)
		}
	}
	return nil
}

// buildEnvSetCommand constructs a shell snippet that idempotently writes
// KEY=VALUE to file: replaces an existing line for KEY if present, otherwise
// appends. KEY and VALUE are embedded into the snippet via single-quoting
// so they're safe from shell metacharacter interpretation.
func buildEnvSetCommand(file, key, value string) string {
	// Use grep -F to avoid regex interpretation of the literal key; the
	// anchors ^<key>= ensure we only match the assignment line, not the same
	// key as a substring of another variable name.
	grepQ := fmt.Sprintf("grep -qF %s %s",
		shQuoteSingle(key+"="), shQuoteSingle(file))
	// sed -i replaces the whole line for KEY. We embed key and value via
	// single-quoted shell arguments to keep them literal in the sed
	// substitution. The sed delimiter is `|` so that `/` in values does not
	// require escaping.
	sedExpr := fmt.Sprintf("s|^%s=.*|%s=%s|", key, key, value)
	sedCmd := fmt.Sprintf("sed -i %s %s",
		shQuoteSingle(sedExpr), shQuoteSingle(file))
	// Append path uses printf to avoid echo's trailing-newline portability
	// quirks; %s=%s is safe because both sides were validated / single-
	// quoted above.
	appendCmd := fmt.Sprintf("printf '%%s=%%s\\n' %s %s >> %s",
		shQuoteSingle(key), shQuoteSingle(value), shQuoteSingle(file))
	return fmt.Sprintf("if [ -f %s ] && %s; then %s; else %s; fi",
		shQuoteSingle(file), grepQ, sedCmd, appendCmd)
}

// buildEnvGetCommand constructs a shell snippet that prints the value of KEY
// from file, or exits 1 if the key is unset. Used by `bunker env get`.
func buildEnvGetCommand(file, key string) string {
	// awk is used so we can print only the value (after the first '=') and
	// exit cleanly. Anchoring the regex with ^<key>= prevents false matches
	// against similarly-named variables.
	return fmt.Sprintf("awk -F= -v k=%s '$1==k {sub(/^[^=]*=/, \"\"); print; exit 0} END{exit 1}' %s",
		shQuoteSingle(key), shQuoteSingle(file))
}

// buildEnvListCommand returns a shell snippet that prints all KEY=VALUE
// lines from file. An empty/missing file is not an error so that `bunker env
// list` on a fresh agent prints nothing and exits 0.
func buildEnvListCommand(file string) string {
	return fmt.Sprintf("[ -f %s ] && cat %s", shQuoteSingle(file), shQuoteSingle(file))
}

// buildEnvUnsetCommand constructs a shell snippet that removes every line
// beginning with KEY= in file. Idempotent — exits 0 whether or not KEY was
// present.
func buildEnvUnsetCommand(file, key string) string {
	sedExpr := fmt.Sprintf("/^%s=/d", key)
	return fmt.Sprintf("[ -f %s ] && sed -i %s %s",
		shQuoteSingle(file), shQuoteSingle(sedExpr), shQuoteSingle(file))
}

// streamEnvExec issues an ExecAgent streaming RPC with the given shell
// command and prints stdout to the cobra command's output, returning an
// error only for transport/protocol issues (or non-zero exit when
// failOnError is true). Token may be empty.
func streamEnvExec(
	ctx context.Context,
	cmd *cobra.Command,
	client bunkerv1connect.BunkerdClient,
	agentID, shellCmd, token string,
	failOnError bool,
) error {
	req := connect.NewRequest(&v1.ExecAgentRequest{
		AgentId:        agentID,
		Command:        "sh",
		Args:           []string{"-c", shellCmd},
		TimeoutSeconds: uint32(0), // Use server default; we set our own context above.
	})
	if token != "" {
		req.Header().Set("Authorization", "Bearer "+token)
	}

	stream, err := client.ExecAgent(ctx, req)
	if err != nil {
		return fmt.Errorf("exec agent: %w", err)
	}

	var exitCode int32
	for stream.Receive() {
		msg := stream.Msg()
		if msg.GetStdout() != nil {
			if _, err := cmd.OutOrStdout().Write(msg.GetStdout()); err != nil {
				return fmt.Errorf("write stdout: %w", err)
			}
		}
		if msg.GetStderr() != nil {
			if _, err := cmd.ErrOrStderr().Write(msg.GetStderr()); err != nil {
				return fmt.Errorf("write stderr: %w", err)
			}
		}
		if msg.ExitCode != 0 {
			exitCode = msg.ExitCode
		}
	}
	if err := stream.Err(); err != nil {
		return fmt.Errorf("stream error: %w", err)
	}

	// For `env set`/`env list`/`env unset`: any non-zero exit is a real failure.
	// For `env get`: an empty result with exit 1 means the key isn't set, which
	// we surface to the caller as "nothing printed" and exit 0 so the command
	// remains pipe-friendly.
	if exitCode != 0 && failOnError {
		return fmt.Errorf("exit code %d", exitCode)
	}
	return nil
}
