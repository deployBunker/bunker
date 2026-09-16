# Package: `internal/hostsetup`

Idempotent, testable host provisioning for the agent isolation boundary
(GAP-075). Design and guarantees: `specs/agent-tmp-isolation.md`.

## Public API

- `Options` — every host path, identity and limit the provisioners use. The zero
  value is valid: `WithDefaults()` fills empty fields with the documented
  defaults and applies the optional `Root` sandbox prefix, so tests can point the
  whole package at a temp tree instead of a real host. `WithDefaults` is
  idempotent (paths already inside `Root` are returned unchanged), because every
  entry point calls it. `DaemonBinary` (default `DefaultDaemonBinary` =
  `/usr/local/bin/bunkerd`, the same path `internal/systemd` writes into the
  unit ExecStart) and `DaemonVersionRunner` (the version-probe seam, parallel to
  `Runner`) feed the daemon version-skew check (`daemonversion.go`).
- `Runner` — the command seam (`func(ctx, name, args ...string) ([]byte, error)`).
  Production uses `DefaultRunner`; tests inject a recorder that answers the
  probes (`getent`, `id`, `mountpoint`, `findmnt`) from simulated state and
  records every argv, so host state is never touched and the exact command line
  can be asserted.
- `Report`/`Change` — what a provisioner considered or did. `Mutations()` returns
  only the changes that modified host state (pure `ok`/`skip` rows excluded),
  which is what makes idempotency assertable: a second apply must have none.

Shared scratch (`scratch.go`):

- `Options.ScratchDir(agentID)`, `ScratchMountArgs`, `ScratchMountOptions`,
  `ScratchRemountArgs` — pure builders for the per-agent bounded tmpfs.
- `EnsureSharedScratch(ctx)` — agent group (`groupadd --system`) + root
  directory (root-owned, group `bunker-agents`, setgid and NOT group/world
  writable, mode 2750), re-asserted and STAT-ED BACK on every call
  (`verifyScratchRoot`): a still-writable exchange root is a hard error, because
  an agent could otherwise create an uncapped plain entry beside its capped
  tmpfs.
- `EnsureAgentGroup(ctx)` / `EnsureAgentGroupMembership(ctx, username)` — create
  the isolation group and add one agent to it. Exported because the fail-closed
  precondition REQUIRES the membership: a host without the group, or an agent
  without the membership, has every session DENIED (never a silent shared
  `/tmp`). Membership is granted for every agent **independently of
  `ScratchEnabled`**.
- `EnsureAgentScratch(ctx, agentID, username, uid, gid)` — group membership,
  mountpoint, size-capped tmpfs mount, ownership, mode 2770. **Fail-closed:** a
  failed mount removes the mountpoint before returning the error, so a broken
  provision never leaves an unbounded group-writable directory.
- `RemoveAgentScratch(ctx, agentID)`, `AgentScratchStatus(ctx, agentID)`.

Private `/tmp` (`tmpnamespace.go`):

- `NamespaceConf(instanceRoot)` / `NamespaceConfRule(instanceRoot)` — the
  `namespace.d` drop-in (`/tmp <root>/ user:noinit root`) and the exact rule
  string the helper verifies. The fourth field is a **user list only** (exact
  names, resolved with `getpwnam`), so it is defense in depth; it is never
  load-bearing.
- `NamespacePAMAgentPattern` (`bunker-*`) / `NamespacePAMClassifierLine()` /
  `NamespacePAMVerifyLine(helperPath, group)` / `NamespacePAMModuleLine` /
  `NamespacePAMBlock(helperPath, group)` — the managed sshd session block, three
  adjacent MODULES:

  1. `[success=2 auth_err=ignore default=die] pam_succeed_if.so quiet user !~ bunker-*`
     — the OUTER guard, keyed on the reserved username pattern (not group state);
  2. `[success=ignore default=die] pam_exec.so quiet <helper> verify <group>` —
     the agent-only fail-closed precondition (see `pamguard.go`);
  3. `required pam_namespace.so` — bare: no `ignore_config_error`, no
     `ignore_instance_parent_mode`.

  A non-agent name is jumped over ALL THREE modules; an agent name must pass the
  verifier; anything unclassifiable is denied. The jump length counts modules, so
  the trio is always written and repaired as one adjacent unit
  (`pamBlockIntact`).
- `PamGuardScript()` / `PamGuardManifest(script)` / `PamGuardScriptHash(script)`
  (`pamguard.go`) — render the root-owned POSIX-sh precondition helper and its
  sha256 manifest. The helper re-checks its whole TRUST CHAIN (the helper
  directory, the manifest, then its own bytes against that manifest — owner and
  group/world-write on every link), the `PAM_USER` unambiguousness, the group +
  membership, the exact drop-in rule, the whole namespace config set, the
  instance parent and the module, and gives itself a fixed `PATH`.
- `guardPatternTokens` / `missingPatternTokens(path)` and the
  `GuardModuleSupportsPattern` status field — the installer probes the
  classifier module for the glob test (`fnmatch`/`noglob`, Linux-PAM ≥ 1.6)
  before writing the block: without it `user !~ <pattern>` is an unknown
  attribute and `default=die` would deny every session on the host, so `--apply`
  refuses (nothing written) and `--status` says
  `classifier pattern ok: false` with the missing tokens.
- `ValidateAgentGroup` / `ValidateSystemPath` / `Options.validateTmpNamespaceInputs`
  — reject any configurable group/path that could inject a PAM line, a shell
  word or a second rule, BEFORE anything is written.
- `EnsureTmpNamespace(ctx)` / `RemoveTmpNamespace(ctx)` — install/uninstall the
  agent group, the instance parent (mode 0000), the helper + manifest, the
  drop-in and the PAM block (with a one-time backup of `/etc/pam.d/sshd`).
  Install writes the helper BEFORE the block that invokes it; `Remove` strips the
  block FIRST, restores that file byte-for-byte, deletes the helper/manifest, and
  reports any operator-owned `pam_namespace` line it left alone. Refuses to
  activate the block when `pam_namespace.so`, `pam_succeed_if.so` **or**
  `pam_exec.so` is missing.
- `TmpNamespaceStatus()` — module/classifier (including the glob-test probe)
  /pam_exec/drop-in rule + owner + mode/helper trust chain (directory → manifest
  → helper: presence, owner, mode, hash)/PAM-block group/agent group/instance
  parent presence + owner + mode, plus `Active`, which requires ALL of them and a
  block naming the configured group. It observes exactly the static properties
  the RUNTIME helper enforces, so `Active` cannot report isolated for a host on
  which every agent session is denied (or where an agent could rewrite the
  boundary). Ownership is only observable as root — the same gate the helper
  applies (`if [ "$(id -u)" = 0 ]`) — while modes are always checked.
- `EnsureAgentTmpInstance` / `RemoveAgentTmpInstance` — pre-create an agent's
  instance directory (0700, owned by the agent).

Host `/tmp` cap (`tmpcap.go`):

- `MergeTmpMountOptions` / `RenderTmpMountDropIn` / `RenderTmpMountOptions` — pure
  rendering of the `tmp.mount` drop-in (`[Mount] Options=…,size=<cap>`).
- `EnsureHostTmpCap(ctx, apply)` / `RemoveHostTmpCap(ctx, apply)` — write the
  drop-in, `systemctl daemon-reload`, and apply the cap to a live tmpfs `/tmp`
  with a NON-destructive `mount -o remount,size=`. Never touches `/etc/fstab`
  (asserted by `TestHostTmpCap_NeverTouchesFstab`).
- `HostTmpStatus(ctx)` — fstype, live options/size, drop-in state.

Daemon version skew (`daemonversion.go`, INT-DEMO-001):

- The host half of GAP-075 fails CLOSED: the sshd PAM precondition denies every
  `bunker-*` session unless the agent is in the isolation group, and that
  membership is granted at SPAWN time by the daemon's isolation-provision stage.
  A daemon older than the stage spawns agents WITHOUT the grant, so after a
  host-provision every SSH session into a healthy agent is denied (bare ssh exit
  254) while the agent is reported running — the live demo host lockout of
  2026-09-16. The skew check makes installing the hardening over such a daemon
  loud instead of silent.
- `MinDaemonVersion` (`0.1.4`) — the first release carrying the grant; the
  constant's comment names WHY. `Version` is the compared field (not `Commit`,
  which is an unordered SHA, and not `Built`, which stamps the build machine):
  it is injected by Makefile ldflags on release builds and falls back to the
  module version for `go install`, so one comparison covers both binary kinds.
  `versionAtLeast` compares dot-separated numerics (leading `v` ignored);
  "unknown"/unparseable versions are NOT comparable and fail safe.
- `ProbeDaemonVersion(ctx)` — runs `<DaemonBinary> --version` under
  `DaemonProbeTimeout` (5s) and parses the `bunkerd`/`commit:`/`built:` block
  (`ParseDaemonVersionOutput`). It NEVER fails the installer; it returns
  `DaemonSkewOK` (equal/newer), `DaemonSkewSkewed` (older), or
  `DaemonSkewUnknown` (binary absent / not executable / timed out /
  unparseable) with a diagnostic error. A host may be provisioned before the
  daemon exists, so UNKNOWN warns and proceeds — it never refuses.
- `CheckDaemonSkew(ctx, allow, warn)` — the installer decision: SKEWED returns
  the refusal (one actionable multi-line message: installed version/commit/
  built, the required minimum, the operator-visible failure mode — bare exit
  254 on exec/mount/cp against running agents — and BOTH remediations: upgrade
  the daemon, or `bunker host-provision --uninstall --apply` back to a shared
  /tmp; never hand-delete only the PAM drop-in, the remaining pam_exec
  precondition fails closed); `allow` (`--allow-daemon-skew`) proceeds with a
  loud WARNING; UNKNOWN prints the WARNING and proceeds; OK is silent.
- `Apply` gates on the check BEFORE anything is planned or written; the
  UNINSTALL path is deliberately never gated — returning the host to a shared
  /tmp must always remain possible (and never probes the daemon).
- `Status` carries `DaemonSkew` + `DaemonSkewBuild` (`--status` renders
  `installed daemon (version skew)` /
  `daemon vs minimum: OK|SKEWED|UNKNOWN (required daemon >= 0.1.4 — installed
  <path>: version X, commit Y, built Z)`); the CLI `--status --json` payload
  carries the same facts under `daemon_skew`.

Orchestration (`status.go`):

- `Status(ctx)` → `Status` with `Isolated()` — the single verdict the boundary
  exists for (three modules + drop-in rule/owner/mode + helper trust chain +
  intact PAM block + agent group + a block naming that group + instance parent
  presence/owner/mode). Ownership checks are gated the way the runtime helper
  applies them (`if [ "$(id -u)" = 0 ]`): as non-root they are reported as
  unverifiable rather than passing, while modes are always checked.
  `ScratchRootModeOK` reports the exchange root's mode
  separately: a wide scratch root does not deny sessions, it silently widens the
  exchange tree, so it is flagged instead of folded into the isolation verdict.
- `Apply(ctx, apply)` — daemon-skew check (INT-DEMO-001, `daemonversion.go`:
  refuse an installed daemon older than `MinDaemonVersion` before anything is
  planned or written) + scratch + namespace + host-/tmp cap in one call; with
  `apply=false` it renders the plan and mutates nothing.

## Conventions

- Every function takes an explicit `apply` or is read-only: nothing is installed
  by observing a struct.
- All writes go through `writeFileIdempotent` (compare, write to a temp file in
  the same directory, rename), because a torn `/etc/pam.d/sshd` would break every
  login on the host.
- Only files Bunker owns are written: `/etc/security/namespace.d/50-bunker-agents.conf`
  (a directory the module reads in addition to the distro's `namespace.conf`),
  `/usr/lib/bunker/pam-tmp-guard` + `.sha256`, the tmp.mount drop-in, and an
  append-only marked block in `/etc/pam.d/sshd`. Ownership of a PAM block is
  decided by Bunker's MARKER or by Bunker's own classifier line — never by the
  mere presence of a module name — so an operator's bare/optioned
  `pam_namespace` rule and a foreign `pam_succeed_if` rule are preserved byte for
  byte across install, repair and uninstall.
- Any value that is embedded into PAM/config/helper content (group name, instance
  root, helper paths, tool `PATH`) is validated first and rejected if it could
  inject a line, a shell word or a rule; nothing is written when validation
  fails.
- `Root` exists so a test can exercise the REAL default layout
  (`/srv/bunker-share`, `/etc/security/…`, `/etc/pam.d/sshd`) inside a temp tree.
- `os.Chmod` is used for the mode-0000 instance parent (no Go-vs-shell octal
  ambiguity, and it works even when the command runner is a fake). The scratch
  ROOT is enforced BOTH ways: the shell `chmod 2750` (the operator-facing octal
  string, which Go's `FileMode` cannot express — `0o2750` as a `FileMode` sets
  bit 0o2000, NOT `os.ModeSetgid`, so the setgid bit would be silently dropped)
  and an in-process `os.Chmod(dir, os.ModeSetgid|0o750)`, so the mode is real
  even under a fake runner and can be stat-ed back.

## Dependencies

- Standard library only. Called by `internal/agent` (spawn/destroy) and
  `internal/cli` (`bunker host-provision`); constants are consumed by
  `internal/config` so config defaults, installer and daemon can never disagree.

## Test Patterns

- `fakes_test.go` provides `recorder` (probe answers + recorded argv) and
  `exitError` (an `*exec.ExitError`, which the probes treat as a "no" answer).
- Table-driven tests assert EXACT argv (`ScratchMountArgs`, `ScratchRemountArgs`)
  and the exact drop-in/namespace.conf bodies.
- Idempotency is asserted with `Report.Mutations()` on a second run; fail-closed
  with "mount fails → no directory, no ownership change"; dry runs with "no
  mutating command issued".
- A temp tree created by `Root` may be left mode 0000 (the instance parent), so
  tests that inspect inside it relax it again in `t.Cleanup` (only root can
  traverse a 0000 directory).
- `scoping_test.go` holds the PAM control-field model (`evalPAMStack`) and the
  pins on the rendered block; `pamguard_test.go` EXECUTES the real rendered
  helper (`/bin/sh <helper> verify <group>`) inside a sandbox host whose
  `getent`/`id`/`stat` are stubs driven by `T_*` environment variables (the
  `stat` stub + `T_FAKE_UID` make the helper's root-only ownership checks
  reachable without being root), and feeds the exit code back through the
  control-field model. Adding a new broken-boundary scenario means adding ONE row
  to `boundaryCases()`: the assertion is "session DENIED and `pam_namespace`
  unreached" for the fail-closed matrix AND, when the breakage is statically
  observable, "`--status` reports NOT isolated" for
  `TestStatusInactiveForEveryStaticallyObservableBrokenBoundary`.
- `pamownership_test.go` pins PAM line ownership with a fixture that carries a
  foreign bare rule, a foreign optioned rule and a foreign `pam_succeed_if` rule
  both BEFORE and AFTER the managed block, and compares the uninstalled file
  byte-for-byte.
- `battery_test.go` (dc45cdb) pins the two GAP-075 semantics the LIVE E2E
  battery (`e2e-full-battery.sh`) initially got wrong: (1) the summary block
  must be executed for real under the battery's `set -euo pipefail` and its
  exit code must equal the FAIL count — a bare `exit 0` used to keep the CI E2E
  step green while section 15 reported failures and never printed VERIFY-PASS;
  (2) the per-agent scratch cap must be read as BYTES from `statfs` (validated
  digits before any arithmetic) and proved to be its own tmpfs mount — the old
  script compared the human `size=16384k` mount OPTION against the byte count
  (never equal) and then aborted the `-le` arithmetic ("integer expression
  expected"), silently skipping the over-cap ENOSPC proof. The battery's
  capacity checks are therefore AUTHORITATIVE: over-cap behaviour is asserted
  with real bytes, not human strings.
- `ciwiring_test.go` (06865bc) statically pins WHICH binaries the CI regression
  suite exercises. On run 34778344738 the self-hosted runner resolved the bare
  `bunker`/`bunkerd` invocations of `regression-tests.sh` through PATH to the
  stale `/usr/local` host baseline — a build predating the GAP-075 private-/tmp
  + PAM boundary setup — so the Regression suite job failed 31 PASS / 2 FAIL
  (`exec whoami returns agent username`, `exec propagates exit code`) while the
  run-level status stayed green (`continue-on-error: true`) and the E2E battery
  never ran. The workflow now exports the just-built workspace binaries FIRST
  on PATH and asserts `command -v` resolves to `${{ github.workspace }}/…`
  before running the suite; the test reads `.github/workflows/ci.yml` and
  fails in `go test ./...` on any push that drops the PATH export, the
  resolution proof, or repoints the battery at `/usr/local`.

## Pitfalls

1. **A group-writable directory is not a bounded directory.** The per-agent
   scratch bound comes from the tmpfs `size=` only; `mount` failure must therefore
   remove the mountpoint rather than leave a plain directory in place.
2. **`mountpoint -q` exits non-zero for "not a mountpoint".** Treat only an
   `*exec.ExitError` as a "no" answer; a missing binary is a real failure.
3. **Do not restart `tmp.mount` to apply a size change.** It would delete every
   file open in `/tmp`; the live change is a remount, which preserves contents.
4. **`pam_namespace` needs its instance parent at mode 0000**, and a 0000
   directory cannot be traversed by its owner — create children first, restrict
   last (and relax it again in tests).
5. **`namespace.conf` cannot scope by group.** Its fourth field lists user names
   only (exact names, resolved with `getpwnam`), and agents are dynamic, so
   scoping lives in the PAM block. Never "fix" host-wide coverage by mounting
   `pam_namespace.so` unconditionally: that polyinstantiates every non-root user
   on the host.
6. **The classifier's jump length is 2, so adjacency is correctness.** If the
   verifier or the module line is edited away or reordered, the jump would skip
   an unrelated module; `pamBlockIntact` therefore requires the marker plus all
   three module lines in order, and the installer rewrites the block whenever it
   is not canonical.
7. **Group state must never decide who is an agent.** The first revision scoped
   the module with `pam_succeed_if user ingroup <group>`, which answers
   `PAM_AUTH_ERR` both for a non-member AND for a group that does not exist — and
   its control field jumped those sessions over the module, so deleting the group
   handed every agent the host's `/tmp`. The classifier now keys on the reserved
   `bunker-*` NAME pattern (`user !~ <glob>` over the `PAM_USER` item), and the
   group is a REQUIREMENT the helper verifies (missing group or membership ⇒
   session DENIED). Keep it that way: never make group membership the agent test.
8. **`pam_namespace` alone cannot require a rule.** With no matching polydir it
   returns `PAM_SUCCESS` and the session keeps the shared `/tmp`, which is why
   the `pam_exec` precondition exists and why `default=die` — not
   `default=ignore` — is the control action for both the classifier and the
   verifier.
9. **`pam_exec` maps a non-zero exit, a signal and a failed `execve` all to
   `PAM_SYSTEM_ERR`**, so a deleted helper denies rather than skipping; and it
   passes the PAM environment through, which is why the helper sets its own
   `PATH` before running anything.
10. **A root-owned helper proves nothing while its DIRECTORY or MANIFEST is
    group/world writable.** The chain is directory → manifest → helper: with a
    writable `/usr/lib/bunker`, a local agent unlinks both files and writes its
    own pair, and with a writable manifest it can make any helper bytes it likes
    "match". The helper therefore verifies every link's owner and
    group/world-write bits BEFORE trusting the content, the installer repairs a
    pre-existing wide directory on every apply (chown alone left a 0777
    directory writable), and `Active` includes all six properties — otherwise
    `--status` reports isolated for a host an agent has taken over the helper on.
11. **`Active` must mirror the RUNTIME helper, not the installer's intent.**
    Every condition that makes the helper deny (missing/wrong drop-in rule,
    loosened instance-parent mode, wrong deployed group, broken trust chain) and
    every condition that lets an agent rewrite the boundary must make the
    verdict false. Keep ONE table (`boundaryCases()` in `pamguard_test.go`)
    driving both the fail-closed matrix and the status matrix: a row the helper
    denies but `--status` calls isolated is a false green, and that is exactly
    the bug the third revision fixed.
12. **Host-side and daemon-side hardening versions skew, and the skew is
    silent until it locks every agent out.** Any host config that DEPENDS on a
    daemon capability (here: the spawn-time isolation-group grant) must probe
    the installed daemon before installing, refuse on a skew older than the
    documented minimum, and treat a failed probe as UNKNOWN (warn, never
    refuse — the daemon may legitimately not be installed yet). Keep the
    uninstall path exempt: the escape hatch must outlive any skew. Test the
    daemon probe through its own seam (`DaemonVersionRunner`), never the real
    binary — a dev host with a stale bunkerd must not fail the suite (a stale
    `0.1.3` binary is exactly what the INT-DEMO-001 guard refuses).
