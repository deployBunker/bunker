# `bunkerd --config <missing path>` — the missing-config arm, finally run

Row: **DF-BUNKER-122** (P1, pending). Tick: `bunker-dogfood-2026-11-15-15-54-49`, fourth fire.

The 17:35Z fire recorded `chaos-errorpath` as **UNVERIFIED, not a pass**, because the QA
harness selected the CLI-help arm (`rc=0`) instead of exercising `bunkerd --config <missing>`.
This directory runs that arm directly against the product, plus the controls needed to say
what the behaviour actually is.

## Run it

```sh
bash probe.sh /tmp/df-missing-config      # scratch dir must be writable
```

Six arms, each writing `out/<arm>.out` (stdout+stderr and a trailing `rc=` line):

| arm | command (abridged) | observed |
|---|---|---|
| `01-portpool-real-overcommit` | `bunkerd --config ./overcommit.yaml port-pool check` | `verdict: BOOT REFUSED — agent.max_agents (400) exceeds the port pool capacity (10 ranges…)`, `rc=1` |
| `02-portpool-typo-path` | `bunkerd --config ./overcomit.yaml port-pool check` (one character different) | `config: …/overcomit.yaml`, `pool: 10000-19999`, `max_agents: 100`, `verdict: OK (config.Validate passed…)`, `rc=0` |
| `03-portpool-directory` | `--config <a directory>` | `load config: read config …: is a directory`, `rc=1` |
| `04-portpool-malformed` | `--config <broken yaml>` | `load config: … While parsing config: yaml: line 1: did not find expected ',' or ']'`, `rc=1` |
| `05-daemon-start-typo-path` | `BUNKERD_AUTH_TOKEN=… bunkerd --config <absent>.yaml` | `GENERATED a new auth.jwt_secret` written into the state tree, then `{"msg":"bunkerd config loaded","max_agents":100}`, then `rc=1` on the *default* registry path `/var/lib/bunkerd/agents.jsonl` |
| `06-daemon-start-real-overcommit` | same invocation, real overcommit file | `bunkerd: invalid config: agent.max_agents (400) exceeds the port pool capacity`, `rc=1` |

## What it means

`internal/config/config.go:1745-1750`:

```go
// Read config file if it exists
if _, err := os.Stat(path); err == nil {
    if err := v.ReadInConfig(); err != nil {
        return nil, fmt.Errorf("read config %s: %w", path, err)
    }
}
```

A failed `os.Stat` is not an error: ENOENT — and EACCES on an unsearchable parent — is
indistinguishable from "no config file was configured". Everything that runs after that
(`Unmarshal`, `Validate`, the port-pool pre-flight, the daemon's own startup log at
`internal/server/server.go:147`, and `EnsureJWTSecret`) therefore operates on built-in
defaults while the operator's path is echoed back as if it had been honoured.

Consequences a user hits:

- A pre-flight (`port-pool check`) returns **green** for a file that does not exist. An
  operator who typos the path — or whose unit file points at a moved file — ships a daemon
  configured by defaults (ports `10000-19999`, `max_agents: 100`) believing the intended
  pool was validated.
- If auth happens to be satisfiable from the environment, `bunkerd` does not stop at the
  config: it generates a `jwt_secret` into the state tree and logs `bunkerd config loaded`,
  a statement that is false. It only dies later, on an unrelated default path, which sends
  the operator after the wrong problem.
- The failure modes that *are* honest — an unreadable-in-a-readable-place file, a directory,
  malformed YAML — show the read error. The hole is specific to `stat` failing.

## Why the fix is not "make `Load` strict"

The stat-gate is load-bearing inside the package: `internal/config/config_test.go`
lines 117, 186, 223, 236, 308, 337 and `rootless_cache_test.go:61` all call
`Load("/nonexistent/path/config.yaml")` and expect defaults. The comment at `config.go:1745`
is also right for the *default* path. The defect is at the flag boundary
(`cmd/bunkerd/main.go:76`, default `defaultConfigPath`): an explicitly passed `--config`
that cannot be stat-ed should be an error (or, at minimum, a warning naming the path and
stating that defaults are in force) because the operator's intent was "read THIS file".
That is what the row's acceptance criteria require, together with the startup log naming the
file actually read.

## Performance

Not a finding. `port-pool check` completes in **0.01s wall** (3 runs, `/usr/bin/time`); the
defect is correctness, not latency, and nothing here is slow enough for a user to feel.
