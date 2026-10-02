# The Bunker backup set — keys, state and the restore order

Every secret and state file the two binaries resolve lives at a DOCUMENTED
absolute path (GAP-181): explicit flag → environment variable → per-user
config dir (`${XDG_CONFIG_HOME:-~/.config}/bunker`, with the legacy
`~/.bunker` kept as a read alias) → documented system paths. Nothing resolves
relative to the process working directory, so a shipped binary with **no repo
and no checkout** still finds, saves and backs up its keys — and this
document is the checklist a rebuilt host follows.

Run `bunker paths` (CLI) or `bunkerd --show-paths` (daemon, prints and exits)
on the live host for the machine's ACTUAL resolved locations — this page
documents the rules; the diagnostic documents your machine.

## What IS in the backup set

### CLI side (per user who runs `bunker`)

| Path (default) | Rule that produces it | What it is | Mode |
|---|---|---|---|
| `~/.config/bunker/config.yaml` | XDG config dir (`BUNKER_HOME` or `--config` relocates) | Server registry: per-host tokens, TLS pins, active server | 0600 |
| `~/.config/bunker/keys/<agent-id>` | sibling `keys/` of the config file | Per-agent SSH **private keys** saved at spawn time | 0600 |

A legacy `~/.bunker/config.yaml` is ADOPTED (copied, byte-identical, never
moved) into the config dir on the first post-upgrade run; after the adoption
notice the config dir is authoritative. If you backed up the legacy dir, you
have backed up the same bytes — keep the backup until the new location is
verified.

### Daemon side (`bunkerd`, usually root)

| Path (default) | Rule that produces it | What it is | Mode |
|---|---|---|---|
| `auth.token_file` target (typically `/etc/bunkerd/secrets/token`) | config file (`auth.token_file`) or `BUNKER_AUTH_TOKEN_FILE` | The **master API token** | 0600 |
| `auth.jwt_secret_file` target / `<secrets-dir>/jwt_secret` | config file, `BUNKER_AUTH_JWT_SECRET_FILE`, or generated under the secrets dir | The **JWT signing secret** — every issued agent API key is derived from it; losing it invalidates every agent key | 0600 |
| `<secrets-dir>/` (`$BUNKER_SECRETS_DIR`, else `<agent.base_data_dir>/secrets`, else `<registry parent>/secrets`) | env → config state tree → registry parent | Directory holding generated secrets | 0700 |
| `/etc/bunkerd/config.yaml` | `--config` / `BUNKERD_CONFIG` / the documented default | The daemon config (paths, TLS, limits) — NOT a secret when `*_file` indirections are used | 0600 |
| `audit.path` (default `/var/log/bunkerd/audit.log`, plus `.1`–`.3` rotations and `<path>.shipstate`) | config file or the documented default | The hash-chained audit trail (token-free by design) | 0600 |
| `agent.registry.path` (default `/var/lib/bunkerd/agents.jsonl`) | config file or the documented default | The agent registry (agent identities, ports, users) | 0600 |

`agent.ssh_dir` (default `/etc/bunkerd/ssh`) holds the SERVER-side copies of
agent SSH keys; the client-side `keys/` above are the ones operators keep.
Back up both sides when you back up a full host.

## What is NOT in the backup set

- **Inline secrets in copied config files.** `auth.token:` /
  `auth.jwt_secret:` values written directly in a config file are LEGACY
  storage (GAP-129): the daemon still loads them — with a loud
  "legacy secret storage" warning — but they are not the backup set.
  Generators/writers refuse to emit them (`config.EmitInlineSecret`).
  Migrate with `token_file:` / `jwt_secret_file:` (or the
  `BUNKER_AUTH_TOKEN_FILE` / `BUNKER_AUTH_JWT_SECRET_FILE` env vars) and the
  config file becomes safe to copy around.
- Anything under the process working directory — nothing state-bearing is
  ever created or read there (GAP-181; asserted by the test suite).
- Cache/ephemeral dirs (`XDG_RUNTIME_DIR/bunker/mnt/...` mountpoints,
  `~/.cache`, rootless-installer download cache).

## Restore order into a fresh host

Follow top to bottom; each step depends only on the ones above it.

1. **Install the binaries** (`bunker`, `bunkerd`) — no repo or checkout
   needed; every path below resolves without one.
2. **Daemon config**: restore `/etc/bunkerd/config.yaml` (or your
   `--config`/`BUNKERD_CONFIG` location). Verify it contains `*_file`
   references, not inline secrets — if it carries inline values, fix that
   now (step 3 rewrites them anyway).
3. **Secrets dir + credential files**:
   - create the secrets dir 0700 (`$BUNKER_SECRETS_DIR`, else
     `<agent.base_data_dir>/secrets`);
   - restore the master token to the `auth.token_file` path (0600);
   - restore `jwt_secret` into the secrets dir (0600) — **restoring this
     exact file is what keeps every already-issued agent API key valid**.
     If it is lost, agents must be re-keyed (rotation runbook:
     `docs/incident-runbook.md` §6).
4. **State**: restore `agent.registry.path` (`agents.jsonl`) and, for a full
   host, `agent.ssh_dir`. Restore the audit log + rotations + shipstate to
   `audit.path` when the trail must survive (its hash chain verifies with
   `bunker audit verify`).
5. **CLI side, per operator**: restore `~/.config/bunker/config.yaml` and
   `~/.config/bunker/keys/` (or the `$BUNKER_HOME`/`--config` location you
   actually use — check the old host's `bunker paths` output).
6. **Verify with the diagnostic, before first boot**:
   - `bunkerd --show-paths --config /etc/bunkerd/config.yaml` — every row
     must show the restored location with `Exists` implied by a real mode
     (no "missing" markers on the secret rows);
   - `bunker paths --enforce` — exits 0 only when every required path
     exists, and names the exact missing path otherwise.
7. **Boot and smoke**: start `bunkerd`, confirm the jwt_secret boot notice
   says "loaded" (not "GENERATED" — generated would mean step 3's file did
   not land where the daemon resolved it), then `bunker list` from an
   operator account.

No step needs a manual path fixup: the paths were all documented before the
host died, and the diagnostics confirm the same resolution on the new one.
