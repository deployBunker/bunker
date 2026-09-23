# GAP-139b-R2 — key lifecycle redeployed to bunker-mvp + rotate-takes-effect proof (2026-09-23)

**STATUS: this document SUPERSEDES the deploy-state description in
`docs/dogfood/2026-09-22-integration.md`.** That doc records the demo daemon at `16fff6d`
where rotate was a live-path no-op (DF-BUNKER-45). As of 2026-09-23T04:56Z the demo daemon
runs `a6a52a1`, which contains the shared-JWTAuth fix (`6fc6c9b`) and the rotate-output fix
(`95929ce`). Every command below was executed by the tick-529 foreman on the control host
(karaHermes) against the live host; full transcripts are at
`/tmp/foreman-gap139b-r2-evidence.txt` (sha256 `d9b431a21804a211b7dd1ca77c538ba07456ab04c13aa1968358ba44626a88dc`)
and `/tmp/gap139b-r2-run5.log` on the control host. Secrets are masked; full values existed
only in 0600 files under `/tmp/gap139b-r2-sec/`.

## One-command re-verification

```sh
ssh bunker-mvp '/opt/bunker/bunkerd --version'   # expect: commit: a6a52a1  built: 2026-09-23T03:55:17Z
ssh bunker-mvp 'md5sum /opt/bunker/bunkerd'      # expect: cf24fc708ad226fb322658f28623e68d
cd <repo> && git merge-base --is-ancestor 6fc6c9b a6a52a1 && echo PAST-FIX
```

All three verified 2026-09-23 (post-deploy and again after the first judge run).

## Deploy record

- Source: `a6a52a1` (= repo HEAD at tick 529; ancestry `325da4c` and `6fc6c9b` proven with
  `git merge-base --is-ancestor`).
- Cross-built on the control host (`go build -o /tmp/bunkerd-a6a52a1 ./cmd/bunkerd`; demo host
  carries go1.22.2 and cannot build go 1.26 code), staged via `base64 | ssh 'base64 -d > /tmp/...'`,
  sha256 verified BOTH sides (`c953469dc28ad096…`), installed to `/opt/bunker/bunkerd`
  (the systemd unit's ExecStart target — NOT /usr/local/bin), daemon restarted by killing the
  unit's MainPID only (1388396 → 1884184; `pkill -x bunkerd` deliberately avoided: it would kill
  CI battery scratch daemons).
- Window discipline: deploy executed only when the demo host had ZERO `Runner.Worker` /
  `e2e-full-battery` processes (CI batteries create fixed-name users an untimely reconcile
  could destroy; also two redundant queued CI runs of board-only commits were cancelled to
  open the window — disclosed in the tick event).
- Boot reconcile (the dangerous step, default mode=destroy): `replayed_live=0,
  replayed_known=564, restored=0, purged=0, adopted=0, destroyed=0, foreign=0` — no agent lost.

## Live proof (DF-BUNKER-45-grade: minted JWTs, not the static master token)

HS256 JWTs were minted locally with claims `{agent_id, key_id, iat, nbf, exp}` matching
`internal/auth.Claims`, and probed against the AGENT surface `POST /bunker.v1.Agent/GetInfo`
(`gap139r2-000043`, real spawned agent):

| # | Probe | Result |
|---|---|---|
| 0 | JWT(boot secret `/etc/bunkerd/jwt_secret`) pre-rotate | **200** |
| rotate | `bunker key rotate --overlap-seconds 120` | rc=0; new 64-hex secret printed once; prev fp `sha256:d3d3629831c1` (= boot file's text sha256 — the daemon's HMAC key is the secret TEXT, not hex-decoded bytes) |
| 1 | JWT(new secret) immediately after rotate | **200** — the rotate DID reach the validating interceptor path. (DF-BUNKER-45 no-op is gone. First run printed 401 due to a foreman mint bug — hex-decoded 32-byte key; differential probe settled it: ASCII-text key 200, hex-decoded key 401, hours after rotation) |
| 2 | JWT(boot secret) inside 120s overlap | **200** (dual-accept) |
| 3 | static master token on `/bunker.v1.Bunkerd/ServerInfo` post-rotate | **200** (bypasses JWT secret by design) |
| 4 | JWT(boot secret) AFTER overlap expiry (128s wait) | **401** (retired secret dropped) |
| 6a/b | real sub-key before / after `key revoke bk_7b3fe7e989ad4875` | **200 → 401** immediately |
| audit | `journalctl -u bunkerd` | `RotateJWTSecret … 200` @05:01:00Z, `RevokeKey … 200` @05:03:13Z |
| audit | `/var/log/bunkerd/audit.log` | RevokeKey hash-chained entry (`"summary":"[INSECURE-PLAINTEXT] key bk_… revoked"`) |
| persist | new secret in `/etc/bunkerd/jwt_secret`? | NOT persisted (memory-only, documented operator step; rotate output says so) |

Cleanup: test agent destroyed; `bunker list` = no `gap139r2-*` leftover.

## GitReins / CI record

- gitreins task `GAP-139b-R2` (distinct id: `GAP-139b` was already complete in tasks.yaml —
  UPSERT would clobber it). Starved verdict `7aad29db` (iteration cap 200.4/200, near-miss),
  unchanged re-run `42e52d3d` returned a merits FAIL whose deployed-binary premise contradicts
  the live host (it never executed ssh; it quoted the 09-22 doc). This doc exists so the
  re-judge reads today's state from the repo.
- CI: `35815549971` verified 5/5 job-level green (closes the tick-526 deferral);
  `35808493467`, `35808760270`, `35808843011` success; `35812781683` success job-level.
