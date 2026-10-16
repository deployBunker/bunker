# Bunker dogfood — 2026-10-16 (dfb-1016, bunker-mvp)

## Promise tested
"a user can spin up an isolated, resource-limited Linux environment with rootless
Docker, controlled through a single CLI call, and work in it over SSH/mount/tunnel."

## Real-use workflow executed (agent dfb-1016, 1h TTL, destroyed)
1. `bunker spawn --server bunker-mvp --ttl 1h --agent-id dfb-1016` — 19s, clean
   connection bundle + key saved to ~/.config/bunker/keys/.
2. SSH in with the stored key: 2s roundtrip, `bunker-dfb-1016`, go 1.22 + docker CLI
   present on the bare user, egress to github.com OK.
3. Real task: clone the public origin (8s), `make build` from a cold module cache —
   **87s** rc=0; `./bunker --version` stamps `commit=d7609239` (the cloned HEAD).
4. Docker leg (see finding DF-BUNKER-96): the documented default socket
   (/run/user/<uid>/docker.sock) is DEAD on a fresh spawn (systemd unit in
   restart-failure loop, rootlesskit lock collision). The daemon actually listens
   on `/run/bunker/dfb-1016/docker.sock` — pointing DOCKER_HOST there gave a
   working engine: `docker ps` clean, `docker run --rm alpine:latest echo` pulled
   and ran OK.
5. `bunker tunnel dfb-1016` (documented `docker -H localhost:2376 ps`) — BROKEN:
   connection reset by peer on two tries (8s, 12s waits). Same root cause as #4
   (tunnels to the dead socket).
6. `bunker mount` (sshfs): works, sane UX (private-mount allow_other guard is a
   nice touch). Pull performance: 2MB read 9s (vs 2s raw ssh = 4.5x), 23MB tar
   37s — fine for editing, painful for artifacts (DF-BUNKER-98).
7. `bunker exec` + `bunker cp` push: both work. Pull path: undocumented, error
   message unhelpful (DF-BUNKER-97).
8. `bunker destroy dfb-1016` — 62s, archive + delete, local key removed. No dfb
   residue after the ~90s recheck.

## Timings (Step 2b)
- cold `make build` in agent: 87s (documented, README warns "several minutes" — honest)
- warm rebuild: 6.4s
- spawn: 19s; destroy: 62s; exec roundtrip: 1s; mount up: ~6s
- sshfs 2MB read: 9s vs raw ssh 2s; 23MB tar through mount: 37s

## Verdict
PROMISING-BUT-ROUGH. Core lifecycle (spawn/ssh/build/exec/mount/destroy) is solid
and fast; the docker story on a fresh agent is broken exactly where a new user
lands first (default socket + tunnel), which is a P1.
