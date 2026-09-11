# Bunker — Containment Disclosure Specification

Version: 1.0.0
Status: Implemented (GAP-067)
Last Updated: 2026-09-11
Related: specs/container-mode.md (containment substrate), specs/api.md

## 0. Overview

Containment disclosure is an **admin-controlled, hidden-by-default** feature
that lets a bunkerd operator make managed agents honestly disclose the
sandbox they run in. When enabled, two observable changes happen:

1. Every agent session (shell exec, raw exec, script exec, detached
   `bunker run --detach`) receives the environment variable
   `BUNKER_SANDBOX=1`.
2. An allowed **system-info probe** command (strict allowlist, §3) gets one
   self-describing marker line appended to its stdout stream:
   `[bunker: managed sandbox environment — containment active]`

When disabled — the default — the daemon's behavior is **byte-identical** to
a build without the feature: no env var, no marker, no extra stream bytes,
no log lines beyond the startup note.

The design intent: an autonomous client (or human) SSH-exec'ing into an
agent can discover — through normal, non-destructive reconnaissance — that
the machine is a managed sandbox, without the operator having to publish
anything or the daemon having to alter arbitrary command output.

## 1. Configuration

```yaml
# /etc/bunkerd/config.yaml
containment:
  disclosure: false   # hidden by default; enabling changes observable behavior
```

- Go type: `config.ContainmentConfig{Disclosure bool}` in
  `internal/config/config.go`, mapped as `mapstructure:"containment"`.
- Default: `false` (asserted by `TestDefaultConfig_ContainmentDisclosureDisabled`).
- Env override: `BUNKERD_CONTAINMENT_DISCLOSURE` (explicit `BindEnv`), and it
  **overrides a config-file `false`** — the same precedence every other
  `BUNKERD_*` var has (`TestLoad_ContainmentDisclosureEnvOverrideBeatsFile`).
- Safe startup note: `Run()` logs whether disclosure is enabled right after
  `bunkerd config loaded`. The note takes only the boolean — there is no
  code path by which secrets, tokens, or host paths can reach it.

## 2. BUNKER_SANDBOX=1 environment injection

When disclosure is enabled, `BUNKER_SANDBOX=1` is injected through the SAME
explicit env injection paths already used for `PATH`/`DOCKER_HOST`/`TMPDIR`
— it does not depend on sshd `PermitUserEnvironment`/`AcceptEnv`:

| Session type | Injection point | Code |
|--------------|-----------------|------|
| Shell exec (`bunker exec`) | `env ... BUNKER_SANDBOX=1 sh -c '<cmd>'` | `buildAgentExecCommand` |
| Raw exec (`bunker exec --raw`) | dedicated `env(1)` argv element | `buildAgentRawExecCommand` |
| Script exec (`bunker exec --script`) | `env ... BUNKER_SANDBOX=1 <script>` | `buildAgentScriptCommand` |
| Detached run (`bunker run --detach`) | `systemd-run --setenv=BUNKER_SANDBOX=1` | `buildRunAgentArgs` |

Semantics:

- The canonical constant is `config.ContainmentSandboxEnv` — one literal,
  referenced everywhere; never duplicated.
- Disabled → the built command strings/argv are **byte-identical** to
  pre-GAP-067 output (regression-pinned by exact-string tests).
- The disclosure value is **admin-controlled** for detached runs: when
  enabled, an agent-supplied env override (`bunker env set
  BUNKER_SANDBOX=...`) can neither suppress nor rewrite it — the canonical
  value stays. When disabled there is nothing to protect and user env
  passes through untouched.
- No user-provided env file (`/run/bunker/<id>/env`) is mutated; no host
  paths or secrets ride the injection.

## 3. Probe allowlist (strict)

A command is an allowed probe iff, after whitespace-splitting the command
token and appending args:

- The first token's basename resolves per the path rules below to a command
  in the per-command safe-flag table (`uname`, `hostname`, `uptime`, `free`,
  `df`, `id`, `whoami`, `lsb_release`) and **every remaining token is an
  exact entry in that command's safe-flag set**. Bare form (no extra
  tokens) is always allowed. Flags that take their own argument (`free -s
  N`, `df --output=...`, `id someuser`) are deliberately absent from the
  table — unsupported flags and any operand fail the allowlist; or
- The command is `cat` with **only** harmless display-only flags
  (`-n`, `-b`, `-s`, `-v`, `-E`, `-e`, `-T`, `-A`, their `--long` forms, and
  combined short clusters like `-nsv`) and the **single operand
  `/etc/os-release`** (literal match).

Path rules: bare names are honored directly. Absolute paths are honored
ONLY when the parent directory is exactly one of the trusted executable
directories `/bin`, `/usr/bin`, `/usr/sbin` (`/usr/bin/uname` — the common
probe shape). Everything else is rejected: untrusted directories
(`/tmp/uname`, `/home/x/uname`), nested paths (`/usr/local/bin/uname`),
relative paths (`./uname`, `bin/uname`), and trailing-slash shapes
(`/usr/bin/`, `/usr/bin/uname/`). Rejected on top of that: shell compounds
(`uname; whoami`, pipes, redirections, `$()`, backticks, quotes, globs),
non-probe commands (`ls`, `echo`, `docker ps`), lookalikes (`uname2`,
`catfile`), other `cat` targets (`/etc/passwd`), stdin cat (`cat -`),
unsupported flags, and operands (`df /etc`, `id someuser`).

The matcher (`isContainmentProbe` in `internal/server/disclosure.go`) is a
**table lookup with a metacharacter fail-closed split** — it never regex-
matches arbitrary command text, and it never alters how a command runs; it
only decides marker emission.

## 4. Marker emission and exit-code preservation

The marker constant is `containmentDisclosureMarker` in
`internal/server/disclosure.go`:

```
[bunker: managed sandbox environment — containment active]
```

Emission rules (in `ExecAgent`, after `wg.Wait()`, before the exit-code
frame):

1. Fires only when disclosure is enabled AND the invocation is an allowed
   probe (§3). Script uploads (empty `req.Msg.Command`) are never probes.
2. Exactly ONE additional stdout frame is sent. Its content is built by
   `markerFrameForStream` from the ACTUAL streamed-stdout state (any bytes
   forwarded? last byte a `'\n'`? — recorded by the stdout streamer and
   synchronized via `wg.Wait`): stdout with no trailing newline gets a
   leading newline first (`Linux` + frame → marker on its own line
   client-side); stdout already ending in `'\n'` gets no extra blank line;
   no stdout at all gets no leading blank line. The frame always ends with
   the marker line + `'\n'`; when disabled the frame is empty and nothing
   is sent.
3. The command's exit code is sent **unchanged** in the final frame — a
   failing probe keeps its non-zero exit code.
4. Non-probe commands and every exec when the flag is off produce no marker
   and no extra bytes.

The appended marker is designed to be greppable and self-describing:
`grep -F "containment active"` over any captured output identifies a
managed sandbox.

### Machine-parsing implications

Clients that parse probe output line-by-line must tolerate the final
`[bunker: ...]` line when a server discloses. Structured parsers (e.g.
`lsb_release -a` field extractors) are unaffected because the marker is on
its own line and bracketed; positional parsers of `uname` output are
unaffected because the marker does not continue the kernel-release line.
Consumers that hash raw probe output verbatim will see different hashes
against disclosing servers — compare after stripping the marker line.

## 5. Safety boundaries

- **Hidden by default**: enabling changes observable behavior, so it can
  never be on unless the operator asked for it (config or env).
- **Zero-change when off**: byte-identical command strings/argv and stream
  output; pinned by exact-string regression tests.
- **No shell-surface growth**: the injected env var uses the existing
  `env(1)` / `--setenv` quoting; the marker is a constant — no user input
  ever reaches a shell string via this feature.
- **No spawn/destroy/docker/SSH behavior changes**: the feature only reads
  config, appends one env var, and appends one stdout frame.
- **No secrets**: the startup log note and the marker are static strings;
  the audit trail is untouched (probe commands are already audited like any
  other RPC).

## 6. Test map

| Behavior | Test |
|----------|------|
| Probe matcher table (allowed + rejected shapes) | `TestIsContainmentProbe_Allowed` |
| Marker frame from streamed-stdout state (own line; no extra blank) | `TestMarkerFrameForStream` + `TestMarkerCountsAsSent` |
| Marker frame before exit-code frame (exit code untouched) | `TestContainmentMarkerPreservesNonZeroExit` |
| Shell exec env injection + byte-identical off | `TestBuildAgentExecCommand_ContainmentEnv` |
| Raw exec argv injection + identical off | `TestBuildAgentRawExecCommand_ContainmentEnv` |
| Script exec injection + identical off | `TestBuildAgentScriptCommand_ContainmentEnv` |
| Detached `--setenv` + user override wins | `TestBuildRunAgentArgs_ContainmentEnv` |
| Default false / env override / file true / env-beats-file | `internal/config/config_test.go` GAP-067 tests |
| Safe startup note wording | `TestLogDisclosureStartup` |

## 7. Live E2E

The foreman runs the flag-on/flag-off battery on `bunker-mvp`
(`e2e-full-battery.sh`, `VERIFY-PASS`) before publishing: flag-off must show
byte-identical exec behavior; flag-on must show `BUNKER_SANDBOX=1` in exec
sessions and the marker after `uname` / `cat /etc/os-release` output with
exit codes preserved.
