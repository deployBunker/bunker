# Dogfood integration report — 2026-11-27: release-candidate CLI against deployed fleet

Run: tick `bunker-dogfood-2026-11-27-21-33-50`; product HEAD `cc410552ec6e5cfb8c28be4fab852641757c62af`, fetched `origin/main` at 0/0.
Driver: direct dogfood loop; CLI built from the target tree (`bunker 0.4.0`, commit `cc41055`, Go 1.26.11) against `bunker-las-03` daemon v0.2.1. No source code changed.

## Promise

Bunker claims a user can create an isolated Linux coding agent with `bunker spawn`, use it through `exec`, and then destroy it cleanly. The README also promises private per-agent `/tmp` after host provisioning and a fresh-machine install path through `bunker-qa.sh`.

## Real use

- `bunker status --server bunker-las-03`: ONLINE, daemon v0.2.1, 3 existing agents, but `/tmp: HOST-SHARED` with the missing-PAM warning. The daemon also reports shared network mode. This is an existing fleet condition, not created by this run.
- Default documented spawn: `bunker spawn df27standard-164139 --server bunker-las-03 --ttl 30m` failed rc=1 after 45.54s at `slice-limits`: `containment landing did not converge within 40 reads: memory.swap.max = "max", want "0" (bar swap)`. The CLI output says the deployed daemon is a pre-DOGFOOD-015 build and recommends a 45m daemon setting, while the actual client deadline was 45s.
- Control spawn: `bunker spawn df27open-164308 --server bunker-las-03 --preset open --ttl 30m` succeeded rc=0 in 24.55s. `bunker exec df27open-164308 --server bunker-las-03 -- uname -a` succeeded three times: 0.65s, 0.54s, 0.54s. `bunker info` succeeded in 0.41s. The agent was destroyed rc=0; a subsequent `info` returned not_found and `list --status all` showed the agent absent. This validates the usable open-preset lifecycle and clean teardown.

## Performance

Host load was 12.14/12.99/14.23 on 16 cores at the start, so these are loaded-host measurements. Successful exec warm samples were 0.65/0.54/0.54s (mean 0.577s); info was 0.41s. The successful spawn was 24.55s. These successful operations are not slow enough to justify a new PERF row; the 45.54s failed default spawn is a correctness failure and is covered by the existing `DF-BUNKER-124`/`PERF-024` mechanism rather than a new performance row. No flamegraph was taken because no successful user path was user-noticeably CPU-slow.

## Install leg — not silently passed

Required detached leg: `BUNKER_QA_SERVER=bunker-las-03 BUNKER_QA_EVIDENCE=<run>/install-evidence.jsonl BUNKER_QA_TTL=2h bunker-qa.sh launch /home/kara/deployBunker/bunker --keep`, agent `68651d20`, tested source SHA `cc410552ec6e5cfb8c28be4fab852641757c62af`. The host and credential path were available, so this was run, not skipped. At the last collect, the battery was still RUNNING and the agent was deliberately retained; the ledger had 17 rows: 13 OK, 2 FAIL, 2 N/A. `fresh-install` was OK; lint failed rc=3 because `parallel golangci-lint is running`; tests failed rc=1 with the borrowed-registry/default-path and host-environment failures in the harvested log. The final install verdict is PARTIAL/FAILED, not PASS, and teardown is unverified because the battery was still active and the agent was retained for collection.

Evidence: `.hermes/cache/scratch/bunker-dogfood-2026-11-27-21-33-50/install-evidence.jsonl`, SHA256 `f0af15b4c3bd42167615b4f387f5c69753e9333243876703b54dd97b1250555b5` at the last read. The collected evidence is scratch-only at this close and therefore remains unverified as a committed repo artifact; the agent-side install process and agent `68651d20` are still live.

## Judgement

Verdict: PROMISING-BUT-ROUGH. The current CLI's open-preset create → exec → destroy workflow works, but the documented default spawn still fails on the deployed daemon after 45.54s, and the deployed host does not enforce the README's private-/tmp promise. Fresh install reached a real successful build, but the QA battery did not complete cleanly: lint and tests were red and teardown was not yet available.

Findings and ownership: `DF-BUNKER-124` owns the repeated deployed standard-preset containment failure; `DF-BUNKER-117` owns the host's host-shared-/tmp status condition; `QA-BUNKER-099-R2` owns the clean-machine suite's default-registry/host-mode dependency; `GR-001` owns the parallel golangci-lint false-red mechanism. No new row was filed because these are existing board premises. No new PERF row was filed.

Unverified this fire: full install-battery completion and terminal cell set; final agent teardown; cold CLI performance (the successful spawn was first-run but not an isolated cold-cache measurement); private-/tmp cross-agent isolation; large-file transfer; Docker tunnel; REST workflow; and candidate-bound GitReins/CI readiness.

Left behind: this report, the scratch timing/probe scripts and outputs, and the live install handle/agent for later collection. No code, board state, visibility, permissions, or credentials were changed or minted.
