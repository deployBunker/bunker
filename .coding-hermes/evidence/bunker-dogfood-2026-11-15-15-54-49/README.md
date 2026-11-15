# Evidence — `bunker-dogfood-2026-11-15-15-54-49`

Committed because a scratch-only trail is not evidence: the next fire must be able to re-count
every claim in `docs/dogfood/2026-11-15-containment-disclosure.md` from these files.
`manifest.json` lists sha256, byte size and origin path for each entry.

| path | what it is |
|---|---|
| `fire1-containment/remote.sh` | the probe driver: scratch daemon setup on `bunker-mvp` (`127.0.0.1:19221` REST / `:19222` gRPC), spawn, 20 arms, teardown |
| `fire1-containment/bunkerd.log` | daemon log; line 3 = disclosure OFF, line 122 = disclosure ON |
| `fire1-containment/arms/*.out`, `*.err` | per-arm stdout/stderr, disclosure-ON battery (the OFF battery reused these filenames and was overwritten) |
| `install/fire1-ledger.jsonl` | fire-1 install leg: one row, the 45 s-deadline spawn failure ×3 |
| `install/fire1-launch.log` | fire-1 launch log |
| `install/fire2-ledger.jsonl` | fire-2 install leg: 27 rows over 17 cells (14 OK / 4 FAIL / 5 N/A / 4 INFO), sha256 `e581e48f…071d` |
| `install/fire2-launch.log` | fire-2 launch log (`BUNKER_QA_SPAWN_TIMEOUT=180`, agent `8eb14067` created on attempt 1) |
| `install/fire2-cell-logs/` | per-cell QA logs, harvested before the destroy trap |
| `install/fire2-chaos-disconnect.log` | the disconnect cell |
| `install/fire2-spawn-bundle-redacted.txt` | spawn bundle with the agent API key replaced by `<REDACTED-agent-api-key>` (line 11) |
| `perf/` | the read-path measurement harness and its samples |

Recount the markers yourself, do not trust a summary line:

    for f in fire1-containment/arms/*.out; do
      printf '%-24s %s\n' "$(basename "$f" .out)" "$(grep -cF 'managed sandbox environment' "$f")"
    done

Reproduce the install leg: `BUNKER_QA_SPAWN_TIMEOUT=180 bunker-qa.sh` against a bank host, and
harvest `<evidence>.logs` before the destroy trap or the per-cell logs die with the agent.

Redaction: no credential material ships here. The only secret seen this run was the agent API key
inside the spawn bundle (mode 664 on the remote) — redacted at harvest time and re-checked with
`grep -rE 'sk-[A-Za-z0-9]{20,}|API Key: [A-Za-z0-9]|BEGIN OPENSSH PRIVATE KEY' .` returning
no matches.
