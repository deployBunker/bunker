package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"github.com/deployBunker/bunker/internal/programalias"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// NewAliasCommand returns the `bunker alias` command group (GAP-066,
// docker-as-installer): list / set / delete the daemon-side program-alias
// registry that maps a program name onto a container image.
//
// All three verbs are master-credential operations — they sit on the Bunkerd
// service, so the master-only interceptor rejects an agent-scoped sub-key.
func NewAliasCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "alias",
		Short: "Manage docker-as-installer program aliases (name -> container image)",
		Long: `Manage program aliases: a durable mapping from a program name to a
container image, so an agent can RUN a program it never installed.

Why: an agent host has no compilers and no apt rights. Instead of installing
a tool into the agent, register it once:

  bunker alias set yq --image mikefarah/yq:4
  bunker run <agent-id> -- yq --version

The daemon resolves the name, pulls the image on first use (logging how long
the pull cost), and runs the program inside a fresh container. The container
is given the agent's own home at its own absolute path and nothing else: no
docker socket, no host path outside the home, and resource limits taken from
the agent's own spawn-time envelope.

The alias is also INSTALLED into the agent as a shim at ~/bin/<name>, so
anything running inside the agent — including a shell script — can call it:

  echo 'yq --version' | bunker exec --script <agent-id> -

When to use an alias vs apt:
  * alias  — a CLI tool or toolchain you want available to an agent but not
             baked into the host image (yq, jq, a Go/Rust/Node toolchain),
             especially anything you want to version per-invocation or
             upgrade by changing one registry entry.
  * apt    — host plumbing the agent's whole session depends on before any
             exec runs (shells, sshd, PAM artifacts, cron), or anything the
             daemon itself needs in order to spawn the agent at all.
A program alias never replaces a host package: it is per-invocation, it runs
in a container with the agent's home and no network by default, and it cannot
touch a host path outside that home.`,

		DisableFlagsInUseLine: true,
	}
	cmd.AddCommand(newAliasListCommand())
	cmd.AddCommand(newAliasSetCommand())
	cmd.AddCommand(newAliasDeleteCommand())
	return cmd
}

// aliasServerEntry resolves the target server for a MUTATING alias verb
// (set/delete): LoadCLIConfig then the fail-closed session-scoped resolution
// (never a silent fallback to the shared active_server default).
func aliasServerEntry(serverName string) (ServerEntry, error) {
	return aliasEntryFor(serverName, func(name, active string) (string, error) {
		return SessionScopedTarget(name, active)
	})
}

// aliasReadOnlyServerEntry resolves the target server for `bunker alias list`:
// a read-only verb falls back to the active server, exactly like `bunker list`
// (a read that cannot re-target another session does not need the fail-closed
// posture that mutating verbs do).
func aliasReadOnlyServerEntry(serverName string) (ServerEntry, error) {
	return aliasEntryFor(serverName, func(name, active string) (string, error) {
		return ReadOnlyTarget(name, active), nil
	})
}

// aliasEntryFor loads the CLI config and resolves the server alias through the
// caller's resolver, then returns the configured entry.
func aliasEntryFor(serverName string, resolve func(name, active string) (string, error)) (ServerEntry, error) {
	cfg, err := LoadCLIConfig()
	if err != nil {
		return ServerEntry{}, fmt.Errorf("load config: %w", err)
	}
	resolved, berr := resolve(serverName, cfg.ActiveServer)
	if berr != nil {
		return ServerEntry{}, berr
	}
	entry, ok := cfg.Servers[resolved]
	if !ok {
		return ServerEntry{}, fmt.Errorf("server %q not found in config", resolved)
	}
	return entry, nil
}

// newAliasListCommand builds `bunker alias list`.
func newAliasListCommand() *cobra.Command {
	var serverName string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List registered program aliases",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			entry, err := aliasReadOnlyServerEntry(serverName)
			if err != nil {
				return err
			}
			client := newBunkerdClient(entry)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			req := connect.NewRequest(&v1.ListProgramAliasesRequest{})
			if token := resolveToken(entry); token != "" {
				req.Header().Set("Authorization", "Bearer "+token)
			}
			resp, err := client.ListProgramAliases(ctx, req)
			if err != nil {
				return fmt.Errorf("list program aliases: %w", err)
			}
			out := cmd.OutOrStdout()
			aliases := resp.Msg.GetAliases()
			if len(aliases) == 0 {
				_, _ = fmt.Fprintln(out, "no program aliases registered")
				return nil
			}
			for _, a := range aliases {
				_, _ = fmt.Fprintf(out, "%s	image=%s", a.GetName(), a.GetImage())
				if ep := a.GetEntrypoint(); len(ep) > 0 {
					_, _ = fmt.Fprintf(out, "	entrypoint=%s", strings.Join(ep, " "))
				}
				for _, m := range a.GetMounts() {
					_, _ = fmt.Fprintf(out, "	mount=%s", m.GetHost())
					if m.GetContainer() != "" {
						_, _ = fmt.Fprintf(out, ":%s", m.GetContainer())
					}
					if m.GetReadOnly() {
						_, _ = fmt.Fprint(out, ":ro")
					}
				}
				if a.GetNetwork() {
					_, _ = fmt.Fprint(out, "	network=on")
				}
				if a.GetDescription() != "" {
					_, _ = fmt.Fprintf(out, "	# %s", a.GetDescription())
				}
				_, _ = fmt.Fprintln(out)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (required unless BUNKER_SESSION_TARGET is set; mutating commands never fall back to the shared active default)")
	return cmd
}

// newAliasSetCommand builds `bunker alias set <name>`.
func newAliasSetCommand() *cobra.Command {
	var (
		serverName  string
		image       string
		entrypoint  []string
		mounts      []string
		network     bool
		description string
	)
	cmd := &cobra.Command{
		Use:   "set <name> --image <ref>",
		Short: "Register or update a program alias (name -> container image)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if strings.TrimSpace(image) == "" {
				return fmt.Errorf("--image is required (e.g. --image mikefarah/yq:4)")
			}
			// Fail fast on the grammar the daemon enforces, so an operator
			// sees a bad name or image before a round trip.
			if err := programalias.ValidateName(name); err != nil {
				return err
			}
			if err := programalias.ValidateImage(image); err != nil {
				return err
			}
			parsedMounts, err := parseAliasMountFlags(mounts)
			if err != nil {
				return err
			}
			for _, m := range parsedMounts {
				if err := programalias.ValidateMountShape(m); err != nil {
					return err
				}
			}
			// Entrypoint tokens arrive as repeated flags; comma-separated
			// values are accepted too so `--entrypoint sh,-lc` works.
			ep := make([]string, 0, len(entrypoint))
			for _, tok := range entrypoint {
				for _, part := range strings.Split(tok, ",") {
					if part != "" {
						ep = append(ep, part)
					}
				}
			}

			entry, err := aliasServerEntry(serverName)
			if err != nil {
				return err
			}
			client := newBunkerdClient(entry)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			wire := &v1.ProgramAlias{
				Name:        name,
				Image:       image,
				Entrypoint:  ep,
				Network:     network,
				Description: description,
			}
			for _, m := range parsedMounts {
				wire.Mounts = append(wire.Mounts, &v1.ProgramAliasMount{
					Host:      m.Host,
					Container: m.Container,
					ReadOnly:  m.ReadOnly,
				})
			}
			req := connect.NewRequest(&v1.PutProgramAliasRequest{Alias: wire})
			if token := resolveToken(entry); token != "" {
				req.Header().Set("Authorization", "Bearer "+token)
			}
			resp, err := client.PutProgramAlias(ctx, req)
			if err != nil {
				return fmt.Errorf("register program alias: %w", err)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s: %s -> %s\n", resp.Msg.GetStatus(), name, image)
			return nil
		},
	}
	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (required unless BUNKER_SESSION_TARGET is set; mutating commands never fall back to the shared active default)")
	cmd.Flags().StringVar(&image, "image", "", "Container image the program runs out of (required)")
	cmd.Flags().StringArrayVar(&entrypoint, "entrypoint", nil, "Docker's --entrypoint: the program the container enters through (repeatable or comma-separated; extra elements are its leading args); empty = the image's own ENTRYPOINT/CMD (usual for a tool image like mikefarah/yq)")
	cmd.Flags().StringArrayVar(&mounts, "mount", nil, "Extra bind mount host[:container][:ro] (repeatable). host MUST be inside the target agent's home directory (/home/bunker-<id>/...); anything else is refused")
	cmd.Flags().BoolVar(&network, "network", false, "Give the container network access (default: --network none)")
	cmd.Flags().StringVar(&description, "description", "", "Operator-facing note shown by `bunker alias list`")
	return cmd
}

// newAliasDeleteCommand builds `bunker alias delete <name>`.
func newAliasDeleteCommand() *cobra.Command {
	var serverName string
	cmd := &cobra.Command{
		Use:   "delete <name>",
		Short: "Remove a program alias",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			entry, err := aliasServerEntry(serverName)
			if err != nil {
				return err
			}
			client := newBunkerdClient(entry)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			req := connect.NewRequest(&v1.DeleteProgramAliasRequest{Name: name})
			if token := resolveToken(entry); token != "" {
				req.Header().Set("Authorization", "Bearer "+token)
			}
			resp, err := client.DeleteProgramAlias(ctx, req)
			if err != nil {
				return fmt.Errorf("delete program alias: %w", err)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s: deleted=%t\n", resp.Msg.GetName(), resp.Msg.GetDeleted())
			return nil
		},
	}
	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (required unless BUNKER_SESSION_TARGET is set; mutating commands never fall back to the shared active default)")
	return cmd
}

// parseAliasMountFlags parses repeatable `--mount host[:container][:ro]`
// values. Paths are absolute, so a ':' can only be a separator; `ro` as the
// last component is the read-only marker.
func parseAliasMountFlags(specs []string) ([]programalias.Mount, error) {
	out := make([]programalias.Mount, 0, len(specs))
	for _, spec := range specs {
		spec = strings.TrimSpace(spec)
		if spec == "" {
			return nil, fmt.Errorf("--mount: empty value")
		}
		parts := strings.Split(spec, ":")
		if len(parts) > 3 {
			return nil, fmt.Errorf("--mount %q: expected host[:container][:ro]", spec)
		}
		m := programalias.Mount{Host: parts[0]}
		rest := parts[1:]
		if len(rest) == 2 && rest[0] == "" {
			// `host::ro` — container defaults to host.
			rest = rest[1:]
		}
		switch len(rest) {
		case 0:
		case 1:
			if rest[0] == "ro" {
				m.ReadOnly = true
			} else if rest[0] != "" {
				m.Container = rest[0]
			}
		case 2:
			m.Container = rest[0]
			if rest[1] != "ro" {
				return nil, fmt.Errorf("--mount %q: trailing component must be 'ro'", spec)
			}
			m.ReadOnly = true
		}
		out = append(out, m)
	}
	return out, nil
}
