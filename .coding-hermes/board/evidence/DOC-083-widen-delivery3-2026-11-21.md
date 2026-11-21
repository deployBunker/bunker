# DOC-083 scope widening — delivery 3 of tick bunker-docs-2026-11-21-04-18-40

Purpose: re-fire delivery of the same tick. Deliveries 1-2 filed DOC-080/081/082
(commit ae8299ab) and DOC-083 + DOC-060-R2 (commit e0fd2f42, memory binding
0d728e10). This delivery re-verifies the five published rows and sweeps the
surfaces delivery 2 named unswept: the usage skill's TLS, egress, image-spec and
release-channel sections. Result: four more entries in the same file teach
shipped-or-fixed defects as live traps — the same mechanism DOC-083 already owns
for six known-gap entries — so DOC-083 is widened in place instead of filing
siblings. Audited tree: origin/main 8985e071b0bdca7e671b8f52376c90d72c641ddd.
No new row, no new marker; the row's existing reasoning marker is untouched.

## Re-fire reconciliation (published board, git show origin/main:.coding-hermes/board/tasks.jsonl)

- Board parses 964 rows. DOC-080, DOC-081, DOC-082, DOC-083, DOC-060-R2 all
  present with status pending and exactly one trace marker each, stored in the
  reasoning field. No duplicate ids among the owned five.
- Re-triage leg (Bane 2026-09-30 ruling): pending P1-P3 rows are GAP-066 (P2,
  feature request), GAP-154/GAP-155 (P1, container-mode features), GAP-153 (P3,
  feature), DF-BUNKER-92 (P2, documented-path-is-broken — do-not-demote rule),
  DF-BUNKER-108-R2 (P2, functional label bug), QA-BUNKER-112 (P2, QA harness
  leak). None is a doc-rot/polish row filed before the ruling; nothing to
  re-triage. Matches delivery 2's census.

## The four stale entries (skills/bunker-usage/SKILL.md at 8985e071)

Line anchors from `git show HEAD:skills/bunker-usage/SKILL.md` (snapshot also at
stand-in docs/bunker/delivery3-skill-snapshot.md):

1. TLS refusal UX (:364) — "bunker status exits 0 on TLS refusals ... while list
   exits 1". STALE: DF-BUNKER-64 closed done; implementation 99663fdd ("TLS
   refusal classes map to non-zero exit codes") is an ancestor of HEAD
   (merge-base --is-ancestor 99663fdd HEAD -> true; merged via 7d4fd1f4). Source
   at HEAD: internal/cli/status.go:113-118 maps a classified TLS refusal to
   TLSRefusalExitError (distinct TLSRefusalExitCode (2), status.go comment).
   The skill teaches the pre-99663fdd behavior as current.

2. Release channel SSH-key bug (:378) — "Release channel still ships the
   spawn-SSH-key bug (DF-BUNKER-71, P1) ... The durable fix is a release cut,
   not code." STALE: the release cut happened. Tags: v0.2.0, v0.3.0 exist
   (v0.3.0 = de286fdc, 2026-11-09, "release: bump to v0.3.0"). The DF-BUNKER-59
   fix 19892c3 (2026-09-25, "attach Authorization header to spawn GetAgentKey
   request") is an ancestor of v0.3.0 AND of HEAD. DF-BUNKER-71 status complete.

3. Image-spec walled garden (:388) — "--image-spec is a walled garden until
   DF-BUNKER-77 lands. Exec on an image agent runs in a fresh container with
   ONLY $HOME mounted: env set fails ..., docker is unreachable, exec runs as
   root ...". STALE: DF-BUNKER-77 (P0) is complete; landed as 07fa266 (cherry
   pick of a3e05f9; judge gitreins DF-BUNKER-77-R2 PASS, commit 07fa266;
   foreman RED proof pre-fix 8902288 vs fix 07fa266). merge-base --is-ancestor
   07fa266 HEAD -> true.

4. Release-channel staleness (:397) — "release latest = v0.1.4, 879 commits
   behind HEAD; the prebuilt CLI lacks stop/start/restart, homes, linger,
   host-provision ENTIRELY." STALE twice over:
   - BUNKER-INST-001 (the row that produced this entry) is complete with close
     note: "verification-only close: release v0.2.0 is published and the latest
     release asset exposes stop start restart homes linger and host-provision;
     the row premise is stale".
   - In-repo: latest tag is v0.3.0 (2026-11-09), v0.3.0..HEAD = 206 commits;
     v0.3.0 tree contains internal/cli/stop.go, start.go, restart.go
     (git ls-tree v0.3.0 --name-only internal/cli/).
   The repo's own docs gate agrees: "docs OK: README/CHANGELOG/docs match
   v0.3.0 (42 released commands, 203 commit(s) after the tag)" (delivery 2
   probe, recorded in the delivery-2 audit report).

5. Egress enforcement (:403) — "DO NOT rely on --egress-mode allowlist|none yet
   — it does not enforce anywhere as of 2026-10-09. On nft hosts every
   enforced-mode spawn FAILS (DF-BUNKER-86: nft -f gets rule content as the
   file arg, internal/egress/manager.go:251). ..." STALE for current daemons:
   - DF-BUNKER-86 (P0) complete: fix 30afd519 pushed to origin/main (ExecExecutor
     pipes rule content via stdin), Tier-2 judge PASS 9cab456b;
     merge-base --is-ancestor 30afd519 HEAD -> true.
   - DF-BUNKER-87 complete (duplicate of DF-BUNKER-82). DF-BUNKER-82 complete:
     acceptance required client-side LOUD refusal on pre-egress daemons, server
     echo of the resolved mode, and bunker info printing "not enforced by this
     daemon" when unsupported.
   - Still TRUE and kept: a pre-fd974b51 daemon (old builds) silently ignores
     the flag; the entry must say the client now refuses loudly (DF-BUNKER-82)
     instead of "no guard anywhere".

## Widening decision

Same file, same mechanism (skill entry teaches a completed row's defect as a
live trap), four more entries -> widen the owning pending row DOC-083 in place
via boardctl update --acceptance-criteria (replace, original clauses preserved)
+ --note (append, scope-correction provenance). No new id minted; no sibling
rows. Foreign-dirty write stop honored: the canonical checkout /home/kara/deployBunker/bunker
carries a sibling's uncommitted PERF-021 judge-binding amendment in tasks.jsonl
(updated_at 2026-11-21T05:26:33Z, tick bunker-foreman-2026-11-21-02-19-33) and
untracked .gitreins/review-state.json/.lock, so the board mutation was performed
in this clean worktree at 8985e071 and the sibling's in-flight line was neither
staged nor published by this delivery.

## Baseline (this worktree, before the update)

boardctl -C <worktree> validate -> RESULT: OK (1009 warning(s)); errors: none.
Expected after: identical counts, zero new findings (metadata-only row update,
no new row).
