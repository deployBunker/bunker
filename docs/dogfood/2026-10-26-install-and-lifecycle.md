# Bunker dogfood — 2026-10-26

ch:trace tick=bunker-dogfood-2026-10-26-16-20-05; target=bunker; repo-head=db612dab9e861eb34ba01058c8379d390d6e69b7; witness=bunker-mvp; evidence=/tmp/bunker-qa-tick-20261026-evidence.jsonl

## Promise

Bunker claims that a user can build the CLI from a clone, connect to a remote root daemon, spawn an isolated Linux agent, execute commands, configure persistent environment, inspect metrics, and destroy the agent.

## Real use

A detached HEAD worktree at `/tmp/bunker-dogfood-20261026` built with `make build` twice: cold real 1.28s and warm real 1.43s. The resulting CLI reported version 0.2.1, commit db612dab, Go 1.26.10. A clean clone inside ephemeral agent df261026 at bunker-mvp followed the documented `git clone` + `make build` path: clone 2.206s; cold build 90.862s; rc=0; version commit db612da.

The live lifecycle then worked on df261026: `info` reported the created agent, `exec` returned the isolated user and Linux identity, `env set/get` round-tripped `DF_DOGFOOD=ok`, metrics returned 896.6 MB used / 4.0 GB limit, and list showed the agent. The spawn bundle used the current XDG key path `/home/kara/.config/bunker/keys/df261026`. Destroy returned rc=0; a subsequent `info` returned `not_found` and a fresh list census matched the agent zero times.

One current-build concern remains: `info` rendered `running (docker degraded)` while an exec probe found `docker_socket=present`. The closed DF-BUNKER-108 fix commit a8a12dfc is an ancestor of HEAD, so this is filed as DF-BUNKER-108-R2 pending daemon provenance confirmation.

## Performance

Measured user-visible operations: `make build` cold 1.28s / warm 1.43s on the control host; clean-agent clone 2.206s; clean-agent build 90.862s cold due to toolchain/module downloads; live `bunker status` 0.55s cold and 0.55s warm; live spawn approximately 20s. No PERF row was filed: the measured status and warm build are below a noticeable wait, and the agent build cost is dominated by first-use dependency download. The build timing is retained as evidence for future comparison.

## Fresh-machine install leg

The required `bunker-qa.sh` leg launched fbbdd1fd on bunker-mvp and collected 19 evidence rows. It was not a silent pass. `repo-head` and toolchain bootstrap passed, but `fresh-install` failed at 2026-10-26T16:38:35Z with `compile: writing output: write $WORK/b381/_pkg_.a: no space left on device`. `upgrade` also failed with module-cache ENOSPC; native ci-pass failed downstream builds; act could not create `catthehacker/ubuntu:act-latest`; chaos-resource and chaos-disconnect were unverified. The host reported 94% disk usage (141.4 GB/149.9 GB) during the run. Collect destroyed fbbdd1fd; no install agent remains. This is environment capacity evidence, not a source-build failure: the same HEAD built successfully on the control host and in the earlier fresh clone agent. Rows DF-BUNKER-113, DF-BUNKER-114, DF-BUNKER-115, and QA-BUNKER-085 carry the exact classification and evidence.

## Verdict

PROMISING-BUT-ROUGH. The core CLI lifecycle and documented build path work when the substrate has capacity. Fresh-machine installability is not proven on the required QA host in this tick because the host was disk-starved, and the status Docker qualifier needs a deployed-build provenance check.

## Cleanup

Owned agent df261026: destroyed and verified by `info -> not_found` plus list count 0. QA agent fbbdd1fd: collected and the harness reported `temp agent destroyed`; the evidence file records that outcome. No source code, scheduler state, repo visibility, or permissions were changed. No credentials were minted.
