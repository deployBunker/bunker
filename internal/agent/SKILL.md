# Package: `internal/agent`

## Public API

- `AgentManager` — lifecycle manager for isolated Linux agents.
- `NewAgentManager(cfg, logger, tracker, tunnelMgr, tailscaleMgr)` — constructor; builds a port allocator, tracker, and starts the TTL reaper.
- `(*AgentManager) Spawn(ctx, *SpawnAgentRequest) (*SpawnAgentResponse, error)` — creates a Linux user, generates an SSH keypair, writes `authorized_keys` and `.profile`, installs rootless Docker, and starts dockerd via systemd-run with cgroup limits. Invalid `--ttl` values are rejected here (step 1a, before port allocation) with `CodeInvalidArgument` — no silent fallback to the default.
- `(*AgentManager) Destroy(ctx, agentID, force) (*DestroyAgentResponse, error)` — stops dockerd, removes user slice limits, and removes the Linux user with `userdel -rf`. Unknown IDs return a clean `agent %q not found` `CodeNotFound` error; raw `userdel` output is confined to the server log, never the user-facing error (DOGFOOD-005). Removal is retried (20×500ms after `pkill -u -9` of lingering procs) so a slow dockerd shutdown can't lose the race and leak users (GAP-001/GAP-007).
- `ParseAgentTTL(s string) (time.Duration, error)` — TTL parser in `ttl.go` (DOGFOOD-003). Accepts the spec `\d+[hmd]` including day values (`7d` → 168h, which `time.ParseDuration` never supported); rejects invalid/negative/zero/overflowing input. Shared by Spawn validation AND the API-key TTL block so agent and key expiry always agree.
- `(*AgentManager) Stop()` — signals the TTL reaper goroutine to exit.
- SSH key auto-provisioning in `host_key.go` (UX-007):
  - `provisionHostSSHKey(ctx, username, authKeysFile, logger)` — scans the host's `/root/.ssh/id_{ed25519,rsa}.pub` and appends the public key to the agent's `~/.ssh/authorized_keys` during spawn, so the host can SSH into agents without a manual key exchange.
  - `findHostPublicKey()` — returns the first available host public key path and contents; no key → warning, not error.

Rootless helpers in `rootless.go`:
- `configureSubIDs(username)` — ensures `/etc/subuid` and `/etc/subgid` entries for rootless Docker.
- `installRootlessDocker(ctx, username, userHome, logger)` — downloads and installs Docker's rootless extras into the agent home.
- `ensureRootlesskitAppArmor(ctx, username, logger)` — writes an AppArmor profile for rootlesskit on Ubuntu 24.04+.
- `waitForUserManager(ctx, runtimeDir)` — waits for the systemd user manager bus socket before running the installer.

## Conventions

- Every agent runs as a dedicated `bunker-<agent-id>` Linux user.
- Agent IDs must match `^[a-z0-9-]{1,63}$`; empty IDs are replaced with a UUID short segment.
- Private SSH keys are persisted under `cfg.Agent.SSHDir` (`/etc/bunkerd/ssh` by default) for server-side use; the public key is written to the agent's `~/.ssh/authorized_keys` with an `environment="DOCKER_HOST=..."` prefix.
- Docker socket is created at `/run/bunker/<agent-id>/docker.sock` and chowned to the agent user.
- Resource limits come from `SpawnAgentRequest.Limits` or server defaults in `config.AgentConfig`.
- systemd-run uses `--system --uid=<UID> --gid=<GID> --property=PAMName=login` plus `CPUQuota`, `MemoryMax`, `LimitFSIZE`, `TasksMax`, and `LimitNOFILE` properties.
- The `XDG_RUNTIME_DIR` for rootlesskit is set to `/run/bunker/<agent-id>/run` so dockerd can start even when the standard `/run/user/<UID>` path is unavailable.

## Dependencies

- `internal/config` — `Config.Agent` defaults and port range.
- `internal/resource` — `Tracker` for capacity and `PortAllocator` for per-agent port ranges.
- `internal/tunnel` — `TunnelManager` for Cloudflare TryCloudflare/named tunnels.
- `internal/tailscale` — `TailscaleManager` for per-agent tailnet IPs.
- `proto/bunker/v1` — generated request/response types.
- Standard library: `os/exec`, `os/user`, `crypto/rand`, `regexp`, `time`.
- No external non-stdlib dependencies.

## Test Patterns

- Tests are table-driven where possible; heavy integration paths are mocked with `exec.Command` and temp directories.
- `manager_test.go` uses a `fakeAgentManager` and helper fake useradd/userdel scripts placed on `PATH` to avoid requiring root.
- `manager_dockerd_test.go` tests `waitForDockerd` with a temp directory and a stub process running as the current user.
- `rootless_test.go` uses a temp copy of `/etc/subuid`/`/etc/subgid` via package-level vars to verify subID mapping without touching real system files.
- `concurrency_test.go` exercises the port allocator under goroutines to prove disjoint ranges.
- `ttl_test.go` uses a fast fake clock (`fakeTimeNow`) to verify TTL expiry reaping without wall-clock waits; the TTL parser itself is covered by a 28-case table test (valid `6h`/`90m`/`24h`/`7d`→168h; rejects banana/0/-1h/1.5h/empty/overflow).
- Root-gated suite (`go test -run 'TestSpawn|TestCgroup|TestConcurrency'` as root — spawns REAL users): coverage tests that spawn agents MUST `defer cleanupAgent` (GAP-001/007). Assertions tolerate OOM kills (Go `ExitCode()` returns -1 for signal deaths, not shell 137 — assert `WaitStatus`) and collapsed systemd `show` forms (`LimitNOFILE`/`LimitFSIZE` print a single value when soft==hard) (GAP-001, 1afa140).
- Avoid tests that require a real dockerd; instead assert systemd-run argument construction and file-system state.

## Pitfalls

1. **Rootless Docker requires lingering before install.** The systemd user manager must exist for `dockerd-rootless-setuptool.sh` to run `systemctl --user`. `installRootlessDocker` calls `loginctl enable-linger` and waits for `/run/user/<UID>/bus`; without this, install fails with `Unit docker.service not found`.
2. **dockerd-rootless.sh defaults `--detach-netns=true` which breaks rootlesskit v1.1.1.** The manager sets `DOCKERD_ROOTLESS_ROOTLESSKIT_DETACH_NETNS=false` and passes `DOCKERD_ROOTLESS_ROOTLESSKIT_NET=slirp4netns` for compatibility.
3. **Stale systemd units from incomplete destroys block re-spawn.** Before `systemd-run`, the manager runs `systemctl stop/disable/reset-failed` on the unit name and calls `stopDockerdDirect` as a fallback; otherwise `systemd-run --unit=` fails with "already loaded".
4. **Port allocator can be disabled.** If `PortRangeStart >= PortRangeEnd` or `PortRangePerAgent` is invalid, `NewAgentManager` logs a warning and leaves `portAlloc` nil; `Spawn` falls back to the full configured range.
5. **systemd-run `--system --uid` does not inherit the caller's environment.** Every needed env var (`PATH`, `HOME`, `USER`, `XDG_RUNTIME_DIR`, `DOCKER_HOST`, etc.) must be passed explicitly via `--setenv=`; otherwise rootlesskit/dockerd exit with missing variables.
6. **AppArmor profile must be loaded before running the installer.** `ensureRootlesskitAppArmor` is called before `installRootlessDocker` so unprivileged user namespaces are allowed on Ubuntu 24.04+.
7. **`userdel` can race a slow dockerd shutdown.** `cleanupAgent` first `pkill -u -9`s lingering agent processes, then retries `userdel -rf` up to 20×500ms; force-destroy tolerates a final `userdel` failure so the teardown path itself never panics. Root-suite CI wraps runs with a `/etc/passwd` + `/etc/bunkerd/ssh` + `/run/bunker` snapshot to catch leaks (scripts/root-suite.sh).
