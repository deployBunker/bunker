# Bunker — Configuration Reference (`bunkerd`)

Version: 1.0.0
Status: implemented — generated from `internal/config/config.go` (source of truth) and
`internal/invalidation` for the `server.invalidation.*` block.
Last Updated: 2026-10-09
Related: [architecture.md](architecture.md) (where each setting lands on the host),
[agent-lifecycle.md](agent-lifecycle.md) (the spawn path the `agent.*` keys drive),
[containment-disclosure.md](containment-disclosure.md) (`containment.disclosure`),
[safety-presets.md](safety-presets.md) (`safety.preset` and the trust tiers),
[../docs/egress-policy.md](../docs/egress-policy.md) (`agent.egress.*`, the GAP-134
per-agent egress policy).

> **Every key on this page is read from the Go source, not from memory.** The
> reader-facing surface is `config.example.yaml` (the worked example); this page
> is the complete surface. §13 documents how to re-derive the key set and diff it
> against this page.

---

> Note: internal/config/config.go also defines an `APIKey` struct (mapstructure keys `key_id`, `token_hash`, `agent_id`, `created_at`, `expires_at`). These are fields of the generated API-key record persisted at runtime, NOT operator-settable config knobs — they cannot appear in config.yaml or as BUNKERD_* env vars and are therefore omitted from the tables above.

## 1. The file, the defaults and the env overrides

`bunkerd` reads a single YAML file, `/etc/bunkerd/config.yaml` by default
(`cmd/bunkerd` passes the path to `config.Load`). Loading is
(`internal/config/config.go:1020` `Load`):

1. start from the built-in defaults,
2. overlay the file when it exists (a **missing file is not an error** — every
   key falls back to its default),
3. overlay environment variables for every **bound** key,
4. resolve credentials (`ResolveSecrets` — the `*_FILE` indirection, §11),
5. `Validate()` — **fail loud, fail before any listener binds**. A value outside
   its declared range or outside the vocabulary stops the daemon; it is never
   replaced by a default.

### 1.1 The env override convention

Viper is configured with `SetEnvPrefix("BUNKERD")` and a dot-to-underscore
replacer (`config.go:1030-1031`: `strings.NewReplacer(".", "_")`), so the env
override of a key is:

```
BUNKERD_<KEY, dots → underscores, uppercased>
```

Examples: `server.grpc_addr` → `BUNKERD_SERVER_GRPC_ADDR`;
`agent.registry.max_bytes` → `BUNKERD_AGENT_REGISTRY_MAX_BYTES`;
`agent.isolation.shared_scratch_per_agent_bytes` →
`BUNKERD_AGENT_ISOLATION_SHARED_SCRATCH_PER_AGENT_BYTES`.

**Only keys explicitly bound in `Load` (`config.go:1035-1147`) are honoured as
env overrides.** Eight keys are not bound — `tls.hosts`, the three
`agent.image_spec.*` keys, the two limit-style keys
`agent.reconciliation.orphan_sweep_guard_disabled` /
`agent.reconciliation.unproven_orphan_limit`, and the two `agent.egress.*`
keys — and their tables below say so.
The `server.invalidation.*` leaves are bound individually (`config.go:1136-1147`),
one env var per leaf. A handful of credentials use a `BUNKER_` (no `D`) prefix on
purpose; those are listed in §11 and are not part of the `BUNKERD_*` namespace.

### 1.2 Reading the tables

| Column | Meaning |
|---|---|
| Key | the YAML key, dotted exactly as the daemon reads it (the same spelling the env override derives from) |
| Default | the value a deployment with the key **absent** gets (`DefaultConfig`, `config.go:893`, or the resolver documented in the semantics column) |
| Env override | `BUNKERD_...` when the key is bound in `Load`; `— (not bound)` otherwise |
| Semantics | what the daemon does with it, taken from the Go comments |

Durations are Go duration strings (`300s`, `6h`, `20m`); byte counts are plain
integers; a `bool` is `true`/`false`.

---

## 2. `server.*` — listeners, timeouts, optional transports

| Key | Default | Env override | Semantics |
|---|---|---|---|
| `server.grpc_addr` | `:9090` | `BUNKERD_SERVER_GRPC_ADDR` | gRPC/connect listener address. **Required** — `Validate` refuses an empty value. |
| `server.rest_addr` | `:8080` | `BUNKERD_SERVER_REST_ADDR` | REST (connect) listener address. |
| `server.request_timeout` | `300s` | `BUNKERD_SERVER_REQUEST_TIMEOUT` | Per-request budget; every handler is wrapped in chi `middleware.Timeout(request_timeout)` (`internal/server/server.go`). A handler that runs an external command with `exec.CommandContext` hands this context to the child, so a **client deadline shorter than this budget SIGKILLs the child mid-flight** — the fail-closed destroy home archive is exactly that shape (`DefaultServerRequestTimeout`, `config.go:890`). |
| `server.h2c_enabled` | `false` | `BUNKERD_SERVER_H2C_ENABLED` | Cleartext prior-knowledge HTTP/2 (`net/http.Protocols.SetUnencryptedHTTP2`). Prior knowledge only: net/http does not implement the RFC 7540 `Upgrade: h2c` dance, so `curl --http2` over cleartext silently lands on HTTP/1.1. For loopback/LAN or our own clients. |
| `server.webdav_enabled` | `false` | `BUNKERD_SERVER_WEBDAV_ENABLED` | Mounts the WebDAV surface under `/dav` on the existing listeners. OFF by default: it exposes a served tree, so it is an explicit operator decision. |
| `server.webdav_root` | `""` | `BUNKERD_SERVER_WEBDAV_ROOT` | Directory served under `/dav`. **Required, and required to be absolute**, when `webdav_enabled` is true — refused at load rather than mounting a 500-answering surface that depends on the daemon's working directory. |
| `server.h3_enabled` | `false` | `BUNKERD_SERVER_H3_ENABLED` | Adds the HTTP/3 (QUIC) UDP listener carrying the same handler. **Requires `tls.enabled: true`** — QUIC always encrypts, and the incoherent pair is refused before any socket opens. Alt-Svc is announced only while the UDP socket is live. |
| `server.h3_addr` | `""` | `BUNKERD_SERVER_H3_ADDR` | UDP bind address. Empty derives it from the TCP listeners: `rest_addr` when set, otherwise `grpc_addr` — one port number for both transports by default. A non-empty value must be `host:port`. |
| `server.invalidation` | absent (nil block) | — (block; leaves are bound, see §3) | The BFS-043 server-side invalidation surface: a **pointer block**, so an absent key takes the declared default in `internal/invalidation` while a written key is validated and refused by name. A nil block (the common case) keeps every default and leaves the watcher off. |

---

## 3. `server.invalidation.*` — the watcher and push knobs (BFS-043)

Every field is a **pointer** in the spec: an absent key is a fact about the file
(declared default applies), a written key — including `0` — is an instruction and
is validated strictly. An invalid value is refused by name with its range and is
**never** replaced by the default (BFS-031/BFS-032's defect class: a bound that is
reported and not enforced). The table is `internal/invalidation/knobs.go`; the
same numbers are used to validate, to report, and to obey.

### `server.invalidation.watch.*`

| Key | Default | Env override | Semantics |
|---|---|---|---|
| `server.invalidation.watch.enabled` | `false` | `BUNKERD_SERVER_INVALIDATION_WATCH_ENABLED` | Enables the server-side watcher. A CONTRACT default, not a quiet one: enabling it changes the served revision's kind for a git tree (`git` → `git+watch`, SPEC-watcher-capability §7.2 R-V3). It does **not** turn invalidation off — the poll form (`X-Bunker-Op: events`) is not behind this knob and cannot be switched off. |
| `server.invalidation.watch.heartbeat_ms` | `30000` (range 100..30000) | `BUNKERD_SERVER_INVALIDATION_WATCH_HEARTBEAT_MS` | Heartbeat period, SPEC-push-channel §4.4 (`heartbeat_ms <= 30000`). The client's 90 s dead-channel rule is three missed beats, so both the watcher's heartbeats and the stream's declared period use this number. |
| `server.invalidation.watch.install_headroom` | `512` (range 1..1048576) | `BUNKERD_SERVER_INVALIDATION_WATCH_INSTALL_HEADROOM` | REQUIRED, reported, non-zero headroom over the tree's watch need (SPEC-watcher-capability §4.2): the per-user watch total in use is not observable, so "we fit exactly" is not a claim this build makes. |
| `server.invalidation.watch.flush_every_ms` | `50` (range 1..60000) | `BUNKERD_SERVER_INVALIDATION_WATCH_FLUSH_EVERY_MS` | Coalescing window: a burst inside one window becomes one sorted path list. |
| `server.invalidation.watch.flush_max_paths` | `4096` (range 1..4096) | `BUNKERD_SERVER_INVALIDATION_WATCH_FLUSH_MAX_PATHS` | Aligned with the surface's declared bound `max_paths_per_event = 4096`: above it a flush is a full rescan, never a longer or partial list. The range maximum is the same number, so the knob cannot raise the declared client bound. |
| `server.invalidation.watch.scan_limit` | `100000` (range 1..2^31) | `BUNKERD_SERVER_INVALIDATION_WATCH_SCAN_LIMIT` | Install/rescan walk bound, aligned with the poll form's own observation bound (`eventsScanLimit`): a walk that hits it cannot be claimed as complete coverage. |
| `server.invalidation.watch.max_watches` | `0` = AUTO (range 0..2^31) | `BUNKERD_SERVER_INVALIDATION_WATCH_MAX_WATCHES` | The one knob whose `0` is a documented sentinel **AUTO** = use the platform's own ceiling (`fs.inotify.max_user_watches`), reported beside it. A positive value the platform cannot give is reported as an *unhonoured* pair (configured AND observed), never silently clamped. |

### `server.invalidation.push.*`

| Key | Default | Env override | Semantics |
|---|---|---|---|
| `server.invalidation.push.subscriber_buffer_bytes` | `4194304` (4 MiB; range 65536..2^30) | `BUNKERD_SERVER_INVALIDATION_PUSH_SUBSCRIBER_BUFFER_BYTES` | Per-subscriber buffer, bounded in bytes **and** events (SPEC-push-channel §6.1 B-7). Worst-case memory = `max_subscribers × subscriber_buffer_bytes`. |
| `server.invalidation.push.subscriber_buffer_events` | `256` (range 1..65536) | `BUNKERD_SERVER_INVALIDATION_PUSH_SUBSCRIBER_BUFFER_EVENTS` | Event bound per subscriber; equals the journal bound, because a subscriber that far behind resyncs anyway. |
| `server.invalidation.push.max_subscribers` | `8` (range 1..4096) | `BUNKERD_SERVER_INVALIDATION_PUSH_MAX_SUBSCRIBERS` | Declared maximum subscriber count (SPEC-push-channel §6.4 B-12). Default 8 bounds the declared worst case at 32 MiB. |
| `server.invalidation.push.write_deadline_ms` | `10000` (range 100..29999) | `BUNKERD_SERVER_INVALIDATION_PUSH_WRITE_DEADLINE_MS` | Write deadline per subscriber. **Cross-validated**: it must be strictly below `watch.heartbeat_ms`, so a stalled subscriber is detected within one heartbeat period. The pair is refused rather than adjusted. |
| `server.invalidation.push.max_event_bytes` | `1048576` (1 MiB; range 65536..8388608) | `BUNKERD_SERVER_INVALIDATION_PUSH_MAX_EVENT_BYTES` | Declared byte bound per line (§7.2 B-15). The landed consumer's own per-line read cap is 8 MiB, so declaring more would produce a reconnect loop. |

The push block is resolved and reported but nothing in this build consumes it yet
(BFS-036 owns the wire form); the read-back reports it with `applied:false`.

---

## 4. `tls.*`

| Key | Default | Env override | Semantics |
|---|---|---|---|
| `tls.enabled` | `false` | `BUNKERD_TLS_ENABLED` | TLS on the listeners. Required by `server.h3_enabled` (QUIC always encrypts). |
| `tls.cert_file` | `""` (resolved `/etc/bunkerd/tls/cert.pem` when `self_signed: true`) | `BUNKERD_TLS_CERT_FILE` | Certificate path. Required when TLS is enabled without `auto_tls` and without `self_signed`; when `self_signed` is on the daemon fills the documented default itself. `auto_tls` (certmagic) manages its own certificate and does not read this key. |
| `tls.key_file` | `""` (resolved `/etc/bunkerd/tls/key.pem` when `self_signed: true`) | `BUNKERD_TLS_KEY_FILE` | Private key path; same resolution and requirement as `cert_file`. |
| `tls.auto_tls` | `false` | `BUNKERD_TLS_AUTO_TLS` | Certmagic/Let's Encrypt automation. Requires `tls.domain`. |
| `tls.self_signed` | `false` | `BUNKERD_TLS_SELF_SIGNED` | Generates a self-signed certificate on first start for the hosts below; the CLI pins the certificate on first connect, so clients verify without disabling verification. |
| `tls.domain` | `""` | `BUNKERD_TLS_DOMAIN` | Domain for `auto_tls`. Required when `auto_tls` is true. |
| `tls.mtls` | `false` | `BUNKERD_TLS_MTLS` | Mutual TLS. Requires `tls.ca_file`. |
| `tls.ca_file` | `""` | `BUNKERD_TLS_CA_FILE` | CA bundle for client-certificate verification; required when `mtls` is true. |
| `tls.verify_cn` | `""` | `BUNKERD_TLS_VERIFY_CN` | Expected CN when verifying a presented certificate. |
| `tls.hosts` | `["localhost"]` | — (not bound) | Names/IPs written into the generated self-signed certificate. A list, so it is a file-only key. |
| `tls.insecure_dev` | `false` | `BUNKERD_TLS_INSECURE_DEV` | GAP-126/REQ-T1 explicit opt-in for binding a **non-loopback listener with TLS disabled**. With it false (default) `CheckTLS` REFUSES to start on a non-loopback plaintext bind; with it true the daemon starts, logs a loud INSECURE warning, and stamps every audit record with `[INSECURE-PLAINTEXT]`. No effect while `tls.enabled` is true; a loopback bind never needs it. Never set it on a shared or internet-reachable host. |

---

## 5. `auth.*`

Secret storage has three sources resolved by `ResolveSecrets` in the order
inline < config-file path < env-file path; see §11 for the `*_FILE` env names and
the fail-loud rules.

| Key | Default | Env override | Semantics |
|---|---|---|---|
| `auth.enabled` | `true` | `BUNKERD_AUTH_ENABLED` | Secure by default. A daemon with auth enabled but no token/jwt_secret **refuses to start**; `auth.enabled: false` prints a loud `*** WARNING: AUTH DISABLED ***`. |
| `auth.token` | `""` | `BUNKERD_AUTH_TOKEN` | Master token sent as `Authorization: Bearer …`. Legacy inline storage is still supported but warned about at startup. |
| `auth.jwt_secret` | `""` | `BUNKERD_AUTH_JWT_SECRET` | HS256 signing secret for agent sub-keys and issued JWTs. Auto-generated (32 random bytes, hex) on first boot when auth is enabled, a static token exists and no secret is configured; persisted 0600 under the secrets dir and **never rotated on restart**. |
| `auth.jwt_ttl` | `6h` | `BUNKERD_AUTH_JWT_TTL` | Lifetime of issued JWTs. |
| `auth.token_file` | `""` | `BUNKERD_AUTH_TOKEN_FILE` | Path (mode 0600) holding the master token; the file value wins over an inline one so a secret can be moved out of the config in one edit. A set-but-unreadable or empty file is a hard startup error. |
| `auth.jwt_secret_file` | `""` | `BUNKERD_AUTH_JWT_SECRET_FILE` | Path holding the JWT secret; same precedence and fail-loud rules as `token_file`. |

---

## 6. `agent.*` — lifecycle, limits, network boundaries and the containment override seam

| Key | Default | Env override | Semantics |
|---|---|---|---|
| `agent.base_data_dir` | `/var/lib/bunkerd` | `BUNKERD_AGENT_BASE_DATA_DIR` | Daemon data root: the API-key store (`<dir>/keys`, GAP-132), agent metadata, and the base for other daemon-owned state. |
| `agent.ssh_dir` | `/etc/bunkerd/ssh` | `BUNKERD_AGENT_SSH_DIR` | Server-side per-agent SSH key storage. |
| `agent.port_range_start` | `10000` | `BUNKERD_AGENT_PORT_RANGE_START` | First port of the per-agent pool. |
| `agent.port_range_end` | `19999` | `BUNKERD_AGENT_PORT_RANGE_END` | Last port of the pool. |
| `agent.port_range_per_agent` | `100` | `BUNKERD_AGENT_PORT_RANGE_PER_AGENT` | Ports reserved per agent out of the pool. |
| `agent.max_agents` | `100` | `BUNKERD_AGENT_MAX_AGENTS` | Capacity: spawns beyond it are refused with `CodeResourceExhausted`. |
| `agent.default_cpu_quota` | `2.0` | `BUNKERD_AGENT_DEFAULT_CPU_QUOTA` | Default CPU cores per agent (emitted as systemd `CPUQuota=200%`). |
| `agent.default_memory_bytes` | `4294967296` (4 GiB) | `BUNKERD_AGENT_DEFAULT_MEMORY_BYTES` | Default memory cap (systemd `MemoryMax` / cgroup v2 `memory.max`). |
| `agent.default_disk_bytes` | `21474836480` (20 GiB) | `BUNKERD_AGENT_DEFAULT_DISK_BYTES` | **Per-file** size cap (systemd `LimitFSIZE` → `RLIMIT_FSIZE`), **not a total-disk quota** — no mechanism counts an agent's aggregate usage, and finite values crash-loop .NET apps that `ftruncate` a large sparse file at first boot (`EFBIG` → `SIGXFSZ`). Total-disk enforcement is GAP-161, not implemented (DF-BUNKER-54). |
| `agent.default_max_processes` | `4096` | `BUNKERD_AGENT_DEFAULT_MAX_PROCESSES` | Default `TasksMax`. |
| `agent.default_max_open_files` | `65536` | `BUNKERD_AGENT_DEFAULT_MAX_OPEN_FILES` | Default `LimitNOFILE` (emitted as `<n>:<n>`). |
| `agent.default_max_docker_containers` | `10` | `BUNKERD_AGENT_DEFAULT_MAX_DOCKER_CONTAINERS` | Default container count per agent. |
| `agent.default_ttl` | `6h` | `BUNKERD_AGENT_DEFAULT_TTL` | Default lifetime when a spawn carries no `ttl`; heartbeat-extendable, then the agent auto-destroys. |
| `agent.default_memory_swap_max_bytes` | `0` | `BUNKERD_AGENT_DEFAULT_MEMORY_SWAP_MAX_BYTES` | Flat GAP-118 override of the tier table (systemd `MemorySwapMax` / `memory.swap.max`). See §8 for the envelope. |
| `agent.default_memory_high_bytes` | `0` | `BUNKERD_AGENT_DEFAULT_MEMORY_HIGH_BYTES` | Flat override of the soft throttle (systemd `MemoryHigh` / `memory.high`). |
| `agent.default_memory_oom_group` | `false` | `BUNKERD_AGENT_DEFAULT_MEMORY_OOM_GROUP` | Break-glass flag; **`true` is REFUSED by validation** — the GAP-114 matrix measured the knob UNMEASURED on this host (the systemd 259 user manager refuses the property, delegated cgroupfs writes `EACCES`), so it is blocked from any default-on and can only be armed by a tier that requests it. |
| `agent.default_io_weight` | `0` | `BUNKERD_AGENT_DEFAULT_IO_WEIGHT` | Flat override of `IOWeight` / `io.weight`. The matrix measured the knob **inert** on uncontended NVMe, so no tier defaults it; a positive value opts this daemon in for every tier (spinning-disk or shared-bus hosts). Range 1..10000. |
| `agent.default_io_write_bps` | `0` | `BUNKERD_AGENT_DEFAULT_IO_WRITE_BPS` | Flat override of `IOWriteBandwidthMax` / `io.max` `wbps`, applied to the **whole disk device** (`io.max` rejects partitions — measured). See §8 for the floor. |
| `agent.egress.mode` | `""` → `open` | — (not bound) | GAP-134 daemon-wide default egress policy for new spawns. Vocabulary (owned by `internal/egress`, verbatim): `open` (unrestricted outbound, no firewall interaction — the safe default: a config that does not mention egress runs byte-identically to a pre-GAP-134 daemon), `allowlist` (per-agent default-deny chain keyed on the agent uid; accepted = loopback, established/related return traffic, and the `agent.egress.allowlist` destinations), `none` (deny-all except loopback and established/related return traffic — the control channel only). An unknown name **refuses to start** (§10-style fail-closed: a typoed mode never silently resolves to a different, weaker or stronger boundary). Whitespace is trimmed before validation (`" none "` is valid); an empty/absent value is the unset default and always passes. Runtime precedence (single resolver, `ResolveEgressMode`): per-spawn `--egress-mode` / `SpawnAgentRequest.egress_mode` > this key > `open`. A failed rule installation fails the spawn — an agent is never left unenforced. See [../docs/egress-policy.md](../docs/egress-policy.md). |
| `agent.egress.allowlist` | `[]` (empty) | — (not bound) | Admin-managed accept list used when the resolved mode is `allowlist` (ignored by `open` and `none`). Entries: CIDRs (`10.0.0.0/8`), bare IPs (`203.0.113.7`), or hostnames (`corp.example.com` — resolved to IPs at rule-install time, so a rotating DNS answer goes stale until the next reinstall; DNS itself must be allowed explicitly by adding your resolver's addresses). IPv4-lookalikes that do not parse (`10.0.0.999`) are refused as typos, and hostnames must pass the label grammar (≤253 chars, labels ≤63, alnum + inner hyphens). Entries are validated at config load (fail-closed — see above); a syntactically valid but empty allowlist with `mode: allowlist` still fails the SPAWN at install time, never an accept-nothing chain reported as enforced. |
| `agent.network_mode` | `""` → `shared` | `BUNKERD_AGENT_NETWORK_MODE` | NET-BUNKER-010 daemon-wide default network-isolation mode. Vocabulary (owned by `internal/netmode`): `shared` (host network namespace, no isolation — the default), `systemd` (private network namespace, loopback only; **all outbound is lost by design** — docker image pulls fail in this mode), `procvis` (private `/proc`, hidepid=2 semantics). An unknown name **refuses to start**. Runtime precedence (single resolver, `ResolveNetworkMode`): per-spawn `--network-mode` / `SpawnAgentRequest.network_mode` > `BUNKERD_NETWORK_MODE` > this key > `shared`. See specs/network-isolation.md. |
| `agent.image_spec.enabled` | `true` | — (not bound) | GAP-064 per-agent image customization on spawn; when false, a spawn carrying an `image_spec` is rejected with `CodeInvalidArgument` before any side effect. |
| `agent.image_spec.cache_dir` | `/var/cache/bunkerd/imagespec` | — (not bound) | Where canonicalized spec builds are cached (one directory per spec cache key). |
| `agent.image_spec.build_timeout` | `20m` | — (not bound) | Hard bound on a single rootless image build. |
| `agent.rootless_installer_cache_dir` | `/var/cache/bunker/rootless-installer` | `BUNKERD_AGENT_ROOTLESS_INSTALLER_CACHE_DIR` | Host-level cache of the downloaded rootless Docker installer (~93 MB). A cache hit makes a fresh-agent spawn skip the `get.docker.com` download entirely; `""` restores the legacy uncached path. `cmd/bunkerd` arms this at startup with `env > config > default` precedence, and the env var it reads there is **`BUNKER_ROOTLESS_INSTALLER_CACHE_DIR` (no `D`)** — which therefore wins over this key; an empty env value counts as unset and never clobbers the file. See §11. |
| `agent.registry.enabled` | `true` | `BUNKERD_AGENT_REGISTRY_ENABLED` | GAP-070 durable agent registry (append-only JSONL replayed at startup). When false the daemon behaves as pre-GAP-070 (in-memory tracker only). **A daemon that cannot open this file refuses to start** — every spawn it accepted would otherwise be forgotten by the next replay. |
| `agent.registry.path` | `/var/lib/bunkerd/agents.jsonl` | `BUNKERD_AGENT_REGISTRY_PATH` | Active registry file (mode 0600); rotated backups are `<path>.1` … `.max_backups`. Required when the registry is enabled. |
| `agent.registry.max_bytes` | `5242880` (5 MiB) | `BUNKERD_AGENT_REGISTRY_MAX_BYTES` | Rotation threshold for the active file; must be `> 0` when enabled. |
| `agent.registry.max_backups` | `3` | `BUNKERD_AGENT_REGISTRY_MAX_BACKUPS` | Rotated files retained; must be `> 0` when enabled. |
| `agent.registry.known_id_cap` | `10000` | `BUNKERD_AGENT_REGISTRY_KNOWN_ID_CAP` | Bounds the destroyed-agent ID index that compaction persists (newest kept) — what keeps a repeated destroy idempotent after compaction. |
| `agent.reconciliation.mode` | `destroy` | `BUNKERD_AGENT_RECONCILIATION_MODE` | Startup policy for users the registry does not know. `destroy` removes the leftover `bunker-*` user; `adopt` re-registers it and restores its exact persisted port reservation (adoption is exact-port or nothing — an orphan without readable valid free port metadata is destroyed in both modes). Any other value is refused by `Validate`. |
| `agent.reconciliation.orphan_sweep_guard_disabled` | `false` | — (not bound) | Turns the boot-time bulk-destroy guard OFF (REV-BUNKER-P1-PATCH), loudly warned at boot. Expressed as a **DISABLE** so a zero-valued config gets the guard, not the hole. The guard refuses the whole sweep — destroying nothing — when the registry cannot vouch for the host AND the sweep would remove more than `unproven_orphan_limit` users. |
| `agent.reconciliation.unproven_orphan_limit` | `3` | — (not bound) | Largest number of orphans an UNPROVEN sweep may destroy; above it the sweep is refused and counted. Provenance is unproven when the replayed live set is empty or the registry file did not exist before this boot. `0` is the strictest setting (refuse every unproven sweep) and there is **no "unlimited" value**; a negative value is refused as a typo. Default 3 sits below the deployment sizes on record (the production host runs five agents, so a limit of 5 would sweep it) while still letting a handful of genuine leftovers through. |
| `agent.isolation.agent_group` | `bunker-agents` | `BUNKERD_AGENT_ISOLATION_AGENT_GROUP` | The GAP-075 isolation group every agent joins. It is verified by the fail-closed `pam_exec` precondition (membership is required, denial is a denial — never a fallback to the host `/tmp`) and owns the shared-scratch tree. Membership is granted at spawn whether or not the exchange point is enabled; the group is the isolation identity, not a feature toggle. |
| `agent.isolation.shared_scratch_enabled` | `true` | `BUNKERD_AGENT_ISOLATION_SHARED_SCRATCH_ENABLED` | Gates the ONLY sanctioned cross-agent exchange directory. When false, agents keep their private `/tmp` and have no sanctioned way to hand each other files. |
| `agent.isolation.shared_scratch_root` | `/srv/bunker-share` | `BUNKERD_AGENT_ISOLATION_SHARED_SCRATCH_ROOT` | The exchange directory (root-owned, setgid, group-visible). |
| `agent.isolation.shared_scratch_group` | `bunker-agents` | `BUNKERD_AGENT_ISOLATION_SHARED_SCRATCH_GROUP` | **Legacy alias** of `agent_group` (the first GAP-075 revision's name). An explicit value wins over the default so an existing deployment keeps its group name; prefer `agent_group`. |
| `agent.isolation.shared_scratch_per_agent_bytes` | `268435456` (256 MiB) | `BUNKERD_AGENT_ISOLATION_SHARED_SCRATCH_PER_AGENT_BYTES` | Caps ONE agent's scratch directory (kernel-enforced tmpfs size). A scratch that cannot be mounted at the cap is **not created at all** — the bound is never skipped. |
| `agent.isolation.private_tmp_root` | `/var/lib/bunkerd/agent-tmp` | `BUNKERD_AGENT_ISOLATION_PRIVATE_TMP_ROOT` | Parent of the `pam_namespace` `/tmp` instances: one instance directory per agent holds that agent's private `/tmp`. The private-`/tmp` boundary itself has no toggle. |
| `agent.destroy_home_policy` | `archive` | `BUNKERD_AGENT_DESTROY_HOME_POLICY` | What destroy does with an agent's home before the Linux user is removed (DF-BUNKER-33). `archive` tars the home into `destroy_archive_dir` and **verifies the archive** before `userdel -rf`; `purge` keeps the historical delete-the-home-with-the-user behavior. Any other value resolves to `archive` — a typo never re-enables the unrecoverable delete. |
| `agent.destroy_archive_dir` | `/var/backups/bunker` | `BUNKERD_AGENT_DESTROY_ARCHIVE_DIR` | Where home archives are written (one `<agent-id>-<UTC timestamp>.tar.gz` per destroy, containing everything `userdel -rf` is about to delete); created on demand as 0700. An empty value NEVER disarms the archive — `destroy_home_policy: purge` is the documented opt-out. |
| `agent.destroy_archive_keep` | `20` (viper default) | `BUNKERD_AGENT_DESTROY_ARCHIVE_KEEP` | Retention bound on the archive directory (INFRA-BACKUP-01): after each successful archive the oldest tarballs are pruned so only the newest N remain. `0` is an explicit OPT-OUT that disables pruning entirely; negative values behave like 0. Unset resolves to 20 via `viper.SetDefault` (`config.go:1087`), so "unset" and "explicitly 0" are distinguishable at the struct. Without the bound the directory grows forever (1394 tarballs ≈ 130 GB once filled a disk to 100%). |
| `agent.destroy_archive_max_bytes` | `0` (disabled) | `BUNKERD_AGENT_DESTROY_ARCHIVE_MAX_BYTES` | Optional total-size cap on the archive directory, applied after the keep pass: oldest files are deleted first until the total is under the cap, always retaining the single newest archive. `0` disables the cap. |
| `agent.containment.memory_swap_max_bytes` | `0` | `BUNKERD_AGENT_CONTAINMENT_MEMORY_SWAP_MAX_BYTES` | Nested GAP-118 override of `MemorySwapMax`. Set EITHER this or the flat `agent.default_memory_swap_max_bytes` — a value in both places is refused as ambiguous. |
| `agent.containment.memory_high_bytes` | `0` | `BUNKERD_AGENT_CONTAINMENT_MEMORY_HIGH_BYTES` | Nested GAP-118 override of `MemoryHigh`; same either/or rule against `agent.default_memory_high_bytes`. |
| `agent.containment.io_write_bps` | `0` | `BUNKERD_AGENT_CONTAINMENT_IO_WRITE_BPS` | Nested GAP-118 override of the IO write bound; either/or against `agent.default_io_write_bps`. |
| `agent.containment.memory_oom_group` | `false` | `BUNKERD_AGENT_CONTAINMENT_MEMORY_OOM_GROUP` | Nested GAP-118 override of `MemoryOOMGroup`; **`true` is refused** (UNMEASURED, same rule as the flat field). |
| `agent.containment.io_weight` | `0` | `BUNKERD_AGENT_CONTAINMENT_IO_WEIGHT` | Nested GAP-118 override of `IOWeight`; either/or against `agent.default_io_weight`. |

---

## 7. `agent.egress.*` — the per-agent egress policy (GAP-134)

Two keys under `agent.egress` set the daemon-wide default egress policy for
newly spawned agents; the vocabulary is owned by `internal/egress` (the
netmode-ownership rule) and the firewall mechanics are documented in
[../docs/egress-policy.md](../docs/egress-policy.md). Summary:

| Key | Default | Env override | Semantics |
|---|---|---|---|
| `agent.egress.mode` | `""` → `open` | — (not bound) | One of `open` / `allowlist` / `none`. `open` = unrestricted outbound, no firewall interaction (zero behavior change vs. pre-GAP-134); `allowlist` = per-agent default-deny chain (loopback, established/related return traffic and the allowlist entries are accepted); `none` = deny-all except loopback and established/related return traffic. |
| `agent.egress.allowlist` | `[]` | — (not bound) | The admin-managed accept list for `allowlist` mode: CIDRs, bare IPs, or hostnames (resolved at rule-install time). Empty list + `mode: allowlist` passes config load but fails the spawn at install time — never an accept-nothing chain reported as enforced. |

**Fail-closed startup validation (GAP-134):** `Validate()` runs during
daemon start (`config.go:1432-1436`) — an unknown `agent.egress.mode`
(`agent.egress.mode must be one of [open allowlist none], got "…"`), a
malformed allowlist entry (`agent.egress.allowlist[N]: …`), or an
IPv4-lookalike typo (`10.0.0.999`) each **refuse daemon start**. An empty
mode is the unset default (`open`) and always passes, so a config that does
not mention egress gets exactly the pre-GAP-134 daemon. At spawn time the
per-request mode wins over this default through the single resolver
`ResolveEgressMode`; an unknown name from either source is a hard error,
never a silent fallback to `open`.

## 8. The containment override envelope (GAP-118)

The five containment knobs above are **admin-override inputs to the tier table**
(`internal/agent/isolation.go` `containmentForPreset`), not a second source of
tier values — the measured verdicts in
[docs/presets/knob-safety-matrix.md](../docs/presets/knob-safety-matrix.md)
decide what each tier emits. The override envelope, exactly as `Validate`
enforces it (`config.go:571`, `config.go:1266`):

| Value | Meaning |
|---|---|
| `0` | **The tier table decides.** For `memory_swap_max_bytes` this is indistinguishable from "emit 0", which is why the release form below exists. |
| `-1` | **Release the tier knob back to the host default** — the swap convention. For `memory_swap_max_bytes` the tier table bars swap (`MemorySwapMax=0`) on `standard`/`hardened`, and `-1` means "host default / opt OUT of the tier's bar-swap", **never "infinity"**. It is the measured-UNSAFE direction: barring swap on `open` converts a would-have-completed run into a hard OOM. `-1` is the only accepted negative anywhere in the envelope. |
| positive | An operator override in bytes. For `memory_swap_max_bytes` the range is `-1..-1`, i.e. **only `-1` (or `0`) is accepted** — any other number is refused. |

Numeric floors and ceilings (measured bounds — every number comes from the
knob-safety matrix, nothing is guessed):

- **`MinContainmentIOWriteBps = 20 MiB/s` = `20971520` bytes/s.** This is the
  LOWEST measured-enforced write bound (the matrix's hostile 20 MiB/s cell:
  "21.0 MB/s against a 20MiB/s cap"). **A configured write bound below 20 MiB/s
  is refused** — nothing below it was ever measured, and under the
  experience-budget rule an unmeasured cost may not ship.
- `MaxContainmentIOWriteBps = 2 GiB/s` = `2147483648`: sanity ceiling, so a
  GB/s typo fails at load instead of throttling nothing in production.
- `MinContainmentIOWeight = 1`, `MaxContainmentIOWeight = 10000`: the kernel
  `io.weight` range (docker `--blkio-weight 150` was measured mapping to
  `io.weight` 1415).
- `memory_oom_group: true` is refused by name; the only sanctioned way to arm it
  is a tier that requests it.

Failing to satisfy the envelope stops the daemon at load with the knob's name,
its value and its range (`ContainmentKnobs.Validate`, `config.go:571`). A knob
set in BOTH shapes (flat `agent.default_*` and nested `agent.containment.*`) is
refused as ambiguous (`agent.<name> and agent.containment.* disagree; set only
one (containment.* wins)`, `config.go:605`).

---

## 9. `tunnel.*`, `named_tunnel.*`, `tailscale.*`

| Key | Default | Env override | Semantics |
|---|---|---|---|
| `tunnel.enabled` | `true` | `BUNKERD_TUNNEL_ENABLED` | Cloudflare TryCloudflare quick tunnels for agents. |
| `tunnel.binary_path` | `cloudflared` | `BUNKERD_TUNNEL_BINARY_PATH` | The `cloudflared` executable to run. |
| `tunnel.tunnel_port` | `8080` | `BUNKERD_TUNNEL_TUNNEL_PORT` | Locally exposed port to forward through the tunnel. |
| `tunnel.no_autoupdate` | `true` | `BUNKERD_TUNNEL_NO_AUTOUPDATE` | Passed as `--no-autoupdate`; keeps a tunnel start from downloading a new binary. |
| `tunnel.startup_timeout` | `30s` | `BUNKERD_TUNNEL_STARTUP_TIMEOUT` | How long to wait for the tunnel URL before failing the spawn. |
| `named_tunnel.enabled` | `false` | `BUNKERD_NAMED_TUNNEL_ENABLED` | Cloudflare named tunnel for custom-domain routing. |
| `named_tunnel.name` | `""` | `BUNKERD_NAMED_TUNNEL_NAME` | The named tunnel's name. |
| `named_tunnel.credentials_file` | `""` | `BUNKERD_NAMED_TUNNEL_CREDENTIALS_FILE` | Cloudflare credentials JSON for the named tunnel. |
| `named_tunnel.domain` | `""` | `BUNKERD_NAMED_TUNNEL_DOMAIN` | Domain routed to the agent through the named tunnel. |
| `tailscale.enabled` | `false` | `BUNKERD_TAILSCALE_ENABLED` | Per-agent tailnet IPs (mesh networking). |
| `tailscale.binary_path` | `tailscale` | `BUNKERD_TAILSCALE_BINARY_PATH` | The `tailscale` executable to run. |
| `tailscale.authkey` | `""` | `BUNKERD_TAILSCALE_AUTHKEY` | Tailscale auth key used to join agents to the tailnet. Treat it as a credential. |
| `tailscale.startup_timeout` | `30s` | `BUNKERD_TAILSCALE_STARTUP_TIMEOUT` | How long to wait for the tailnet address before failing the spawn. |

---

## 10. `audit.*`, `containment.*`, `safety.*`

| Key | Default | Env override | Semantics |
|---|---|---|---|
| `audit.enabled` | `true` | `BUNKERD_AUDIT_ENABLED` | Appends one JSONL record per authenticated RPC to `audit.path` (mode 0600). The log never contains token values; each record carries `hash`/`prev_hash`, a tamper-evident chain. If the log cannot be opened the daemon warns and continues WITHOUT auditing — audit failure never blocks startup. |
| `audit.path` | `/var/log/bunkerd/audit.log` | `BUNKERD_AUDIT_PATH` | The append-only JSONL audit file. |
| `audit.ship_to` | `""` (off) | `BUNKERD_AUDIT_SHIP_TO` | GAP-073 retention hardening: ships each ROTATED segment to a remote endpoint so a root attacker cannot complete a cover-up by deleting local backups. `https://`/`http://` webhook (segment POSTed as the body, `X-Bunker-Chain-Head` header) or `syslog://host[:port]` (RFC 3164 over UDP; the lossless copy is the webhook). Fire-and-forget: a dead endpoint never blocks or fails the audit write. OFF by default. |
| `audit.seal_key` | `""` (no seals) | `BUNKERD_AUDIT_SEAL_KEY` | GAP-073: when set, every rotation appends a chained SEAL record carrying `HMAC-SHA256(key=seal_key, msg=<sealed chain head>)`, letting holders of shipped copies prove a local file was truncated or replaced. Keep the key secret and back it up with the shipped copies. OFF by default. |
| `containment.disclosure` | `false` | `BUNKERD_CONTAINMENT_DISCLOSURE` | GAP-067 admin-controlled, HIDDEN BY DEFAULT: when enabled every agent session gets `BUNKER_SANDBOX=1` and allowlisted system-info probes get a self-describing marker line on stdout. When disabled, behavior is byte-identical to a daemon without the feature. See [containment-disclosure.md](containment-disclosure.md). |
| `safety.preset` | `""` → built-in default `standard` | `BUNKERD_SAFETY_PRESET` | GAP-116 daemon-wide default safety preset. Empty (unset) is the built-in default `standard` and is byte-identical to pre-GAP-116 behavior. Vocabulary: `standard` (shipped), `open`, `hardened`. An unknown name **refuses to start** — never a silent fallback to a weaker or stronger knob set. Runtime precedence: per-spawn `--preset` / `SpawnAgentRequest.safety_preset` > `BUNKERD_SAFETY_PRESET` > this key > `standard`. See [safety-presets.md](safety-presets.md). |

---

## 11. Credential-bearing env vars outside the `BUNKERD_*` namespace

These are read directly (not via viper) and keep credentials out of the config
file entirely. The precedence for `auth.token` / `auth.jwt_secret` is **inline <
config-file path (`auth.*_file`) < env-file path**; a path that is set but
unreadable — at every level, including the env level — is a hard error before any
listener binds, never a silent fallback to a weaker source.

| Env var | Purpose |
|---|---|
| `BUNKER_AUTH_TOKEN_FILE` | Path to a file holding the master token; wins over `auth.token_file` and inline `auth.token`. |
| `BUNKER_AUTH_JWT_SECRET_FILE` | Path to a file holding the JWT secret; wins over `auth.jwt_secret_file` and inline `auth.jwt_secret`. |
| `BUNKER_SECRETS_DIR` | Where generated secrets are persisted. Default `$HOME/.config/bunkerd/secrets`; dir created 0700, files 0600. An auto-generated `jwt_secret` lives at `<dir>/jwt_secret`. |
| `BUNKER_ROOTLESS_INSTALLER_CACHE_DIR` | Read by `cmd/bunkerd` at startup for the rootless-installer cache, with `env > agent.rootless_installer_cache_dir > default` precedence (an empty value counts as unset and never clobbers the file). The same key is also bound as `BUNKERD_AGENT_ROOTLESS_INSTALLER_CACHE_DIR`, so both env spellings exist and this one wins. |
| `BUNKERD_SAFETY_PRESET` | The GAP-116 preset env override (§10). Named here because it is read directly by the precedence resolver as well as bound to `safety.preset`. |

---

## 12. Complete key census

`internal/config/config.go` declares **112** `mapstructure` fields, and
`internal/config/egress.go` (the GAP-134 `EgressConfig` block) adds **2**. They
decompose as: **92 leaf keys** reachable from `Config` (90 in `config.go` — the
DOC-25 set plus `agent.network_mode` — plus the 2 `EgressConfig` leaves),
**7** nested-block placeholders (`server.invalidation`, `agent.image_spec`,
`agent.registry`, `agent.reconciliation`, `agent.isolation`,
`agent.containment`, `agent.egress`), **10** top-level block names, and **5**
fields of `APIKey` (a stored record type, not an operator key). The twelve
`server.invalidation.*` leaves live in `internal/invalidation` and are bound
one env var each by `Load`.

| Block | Keys on this page |
|---|---|
| `server.*` (top-level) | 8 |
| `server.invalidation.*` (§3) | 12 |
| `tls.*` | 11 |
| `auth.*` | 6 |
| `agent.*` top-level (incl. the 5 flat containment mirrors) | 24 |
| `agent.image_spec.*` + `agent.registry.*` + `agent.reconciliation.*` + `agent.isolation.*` + `agent.containment.*` + `agent.egress.*` | 3 + 5 + 3 + 6 + 5 + 2 = 24 |
| `tunnel.*` + `named_tunnel.*` + `tailscale.*` | 5 + 4 + 4 = 13 |
| `audit.*` | 4 |
| `containment.*` | 1 |
| `safety.*` | 1 |
| **Total dotted keys documented** | **90 config.go leaves + 2 egress.go leaves + 12 invalidation leaves = 104** |

## 13. Re-deriving the key set (verification recipe)

The census above is checkable from the repo root. Save the extractor (it parses
the `Config` struct tree and follows nested block types), then diff it against
this page — the delta must be empty:

```bash
cat > /tmp/cfgkeys.py <<'PY'
import re
src = open("internal/config/config.go").read()
structs = {m.group(1): [(f.group(1), f.group(2), f.group(3))
           for f in re.finditer(r'(?m)^\t(\w+)\s+([\w\.\*\[\]]+)\s+`mapstructure:"([^"]+)"`', m.group(2))]
           for m in re.finditer(r"(?ms)^type (\w+) struct \{(.*?)^\}", src)}
out = []
def walk(s, p):
    for _, t, k in structs.get(s, []):
        path = f"{p}.{k}" if p else k
        t2 = t.lstrip("*")
        if t2 in structs and structs[t2]:
            walk(t2, path)
        else:
            out.append(path)
walk("Config", "")
print("\n".join(out))
PY

python3 /tmp/cfgkeys.py | sort -u > /tmp/keys.txt
grep -oE '`[a-z_]+\.[a-z0-9_.]+`' specs/configuration.md | tr -d '`' | sort -u > /tmp/doc.txt
comm -23 /tmp/keys.txt /tmp/doc.txt   # empty output = every key is documented
```

The walk reads only `config.go`, so it yields **92** paths — the 90 leaf keys
plus **2** block placeholders: `server.invalidation` (§2), whose twelve leaves
are in §3, and `agent.egress` (§7), whose two leaves (`agent.egress.mode`,
`agent.egress.allowlist`) live in `internal/config/egress.go` and are
documented in §6/§7. At the time of writing `comm -23` prints nothing.

The `server.invalidation.*` leaves are additionally pinned by the table in
`internal/invalidation/knobs.go` (`Knobs()`), which also carries each knob's
declared range and failure mode — the same numbers the daemon validates with.
