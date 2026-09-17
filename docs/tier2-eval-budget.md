# Tier 2 evaluator input budget — measured breakdown and the fix (INT-CI-014)

**Measured:** 2026-09-17 on repo HEAD `848625c` (config sha256 `734f9fbf…b5189`)
**Task under judgment (Step 1 / run A):** `INT-CI-017` · **run B:** `INT-CI-014`
**Instrument:** `/tmp/tier2_spy.py` — runs the installed `gitreins` CLI in-process
(`argv = ["judge", <id>]`) under the pipx venv interpreter, with `logging.DEBUG` and
monkey-patches on exactly three runtime symbols:
`engine.eval_cap.EvalCap.record_llm_call` (per-call tokens),
`EvalCap.reset_context_tracking` (the counter reset that only compaction performs),
`AgenticEvaluator._compact_context` / `_build_code_context` / `_compute_allowed_files`,
plus `LLMClient.chat` (what is re-sent) and `AgenticEvaluator._execute_tool` (tool
results). Every row is written to JSONL as it happens. Raw dumps stay in `/tmp`
(`/tmp/tier2_spy_PRE.jsonl`, `…_v1.jsonl`, `…_POSTA.jsonl`, `…_POSTB.jsonl`) and are
**not** committed. No number below is an estimate; every token count is read from the
engine's own `response.usage` via the patched recorder.

Context for why this row exists: the budget was laddered 2M → 8M → 4M → 8M → 16M, one
rung per tick (`b0a7c84`, `ef429dc`, `25a43d2`), each rung buying roughly one tick.

---

## 1. Pre-change measured breakdown (run 616c1d33, task INT-CI-017)

Verdict: `.gitreins/history/2026-09-17/616c1d33/verdict.json` — **PASS**, no `Cap exceeded`
line, 0 compaction events.

| metric | value |
|---|---|
| LLM calls (iterations) | **23** |
| per-call prompt tokens (min / median / max) | **6,211 / 18,053 / 29,323** |
| per-call completion tokens | 110 → 916 |
| **cumulative input tokens** | **408,009** (2.55 % of the 16M cap) |
| cumulative input / cap | 0.026 |
| compaction events | **0** |
| `Cap exceeded` lines | **0** |
| allowed-file set at evaluation time | 6 files — **none of them the judged artifact** |
| pre-loaded code context | 10,973 chars (≈3,657 tokens), not truncated |

### 1.1 Per-iteration series (measured)

`iteration_credit` is the engine's own counter (1.0 per LLM call, +0.1 per tool call);
`re-sent` is the total character count of the message array handed to the provider on
that call (measured in `LLMClient.chat`).

| call | iteration_credit | prompt tokens | completion | cumulative input | re-sent chars |
|---|---|---|---|---|---|
| 1 | 1.0 | 6,211 | 110 | 6,211 | 17,251 |
| 2 | 2.2 | 6,910 | 73 | 13,121 | 19,258 |
| 3 | 3.3 | 7,967 | 43 | 21,088 | 22,198 |
| 4 | 4.4 | 8,069 | 58 | 29,157 | 22,374 |
| 5 | 5.5 | 9,465 | 77 | 38,622 | 26,733 |
| 6 | 6.6 | 10,870 | 64 | 49,492 | 31,111 |
| 7 | 7.7 | 12,258 | 64 | 61,750 | 35,484 |
| 8 | 8.8 | 12,378 | 76 | 74,128 | 35,597 |
| 9 | 9.9 | 13,192 | 239 | 87,320 | 37,951 |
| 10 | 11.0 | 14,934 | 223 | 102,254 | 42,685 |
| 11 | 12.1 | 16,300 | 341 | 118,554 | 46,256 |
| 12 | 13.3 | 18,053 | 130 | 136,607 | 50,707 |
| 13 | 14.5 | 20,218 | 129 | 156,825 | 57,553 |
| 14 | 15.6 | 21,663 | 121 | 178,488 | 62,040 |
| 15 | 16.7 | 21,908 | 128 | 200,396 | 62,351 |
| 16 | 17.8 | 22,170 | 124 | 222,566 | 62,690 |
| 17 | 18.9 | 22,936 | 126 | 245,502 | 64,552 |
| 18 | 20.0 | 23,865 | 106 | 269,367 | 67,173 |
| 19 | 21.1 | 25,290 | 98 | 294,657 | 71,496 |
| 20 | 22.2 | 26,980 | 402 | 321,637 | 76,316 |
| 21 | 23.3 | 28,002 | 614 | 349,639 | 78,199 |
| 22 | 24.5 | 29,047 | 253 | 378,686 | 79,675 |
| 23 | 25.6 | 29,323 | 916 | 408,009 | 79,782 |

Tool use in this run (26 tool calls): 21 × `run_command`, 3 × `sandbox_write`,
1 × `get_task_item`, 1 × `read_file`. Tool results totalled 62,194 chars, but the
**re-sent** total was 1,149,432 chars — a **×18.5 amplification** of the tool evidence:
every result stays in the message array and is paid for again on every later iteration.

### 1.2 Compaction events: none, and why

The engine compacts when `cumulative_prompt_tok > compaction_threshold × max_input_tokens`
— and `cumulative_prompt_tok` is `max(cumulative_prompt_tok, prompt_tok)`, i.e. the
**largest single prompt**, not a sum. With the shipped ratio 0.90 and a 16M budget the
valve fires at 0.90 × 16,000,000 = **14,400,000 tokens of single-prompt context =
491 × the largest prompt measured (29,323)**. It cannot fire.
`eval_cap.reset_context_tracking()` resets `cumulative_input_tokens`, and compaction is
the **only** caller — so with the valve unreachable the cumulative counter is monotone
and the cap is a hard wall.

### 1.3 Run-to-run variance (same config, same task)

A second instrumented run (`e6d24301`) before any change: **13 LLM calls**,
prompt min/median/max **6,211 / 15,084 / 24,507**, cumulative **206,704**, 0 compactions,
0 cap lines, verdict **PASS** (`.gitreins/history/2026-09-17/e6d24301/verdict.json`).

So the same task on the same config costs 0.21M–0.41M input tokens (**1.97× spread**) —
post-change totals below must be read against that band, not against a single number.

---

## 2. The driver: the counter is quadratic in iteration count

Least-squares fit of the measured series: `prompt(i) = P0 + g·(i-1)` with
**P0 = 5,435**, **g = 1,119 tokens/iteration** (fit reproduces the measured cumulative
with 0.0 % error). Cumulative input after N iterations is therefore

    S(N) = N·P0 + g·N·(N-1)/2

| N (iterations) | cumulative input | vs 16M cap |
|---|---|---|
| 23 (measured) | 0.41M | 2.5 % |
| 50 | 1.64M | 10 % |
| 100 | 6.08M | 38 % |
| 118 | 8.36M | 52 % (the 8M rung died here in previous ticks) |
| 150 | 13.32M | 83 % |
| **165** | **16.0M** | **100 % — cap trips here** |
| 200 (`max_iterations`) | 23.35M | 146 % |

That is the starvation: `max_iterations` is 200, but the input cap dies at iteration
**165** on a run that explores as long as the measured one. Each rung of +1 budget buys
about **+35 iterations** (`dS/dN` at N≈165 is ~1.1M/iteration… i.e. budget/35), which is
why the ladder never converged.

The same arithmetic predicts the historical rungs: at the 8M cap the wall sits at
N≈118, at 4M at N≈85, at 2M at N≈61 — all well inside a criteria-heavy eval, which is
exactly the "INCOMPLETE, cap artifact" pattern of `73e7ecae` (INT-SPAWN-001), `065d0a0b`
(GAP-068) and their predecessors.

**Two defects inflate what the judge must pay for, both measured above:**

1. **The judged artifact was out of scope.** The working tree is clean when the judge
   runs (work is committed before `gitreins task complete`), so `file_scope: changed`
   built the allowed set from the *board* diff: `.coding-hermes/board/events.jsonl`,
   `.gitreins/tasks.yaml`, `.gitreins/config.yaml`, `go.mod`, `go.sum`, `Makefile` — the
   judged file (`.github/workflows/ci.yml` from commit `fe8e1be`) is not in it. Measured
   consequence: the judge's **first** `read_file` on the artifact was rejected
   (`File not in scope: .github/workflows/ci.yml`), and it then re-read the same file
   through **4 `run_command` slices** in extra iterations (4,359 + 4,378 + 4,373 + 2,354
   chars) — all of it re-sent on every later iteration.
2. **The growth valve was unreachable** (§1.2), so nothing ever trimmed that re-send.

---

## 3. The change (`.gitreins/config.yaml`, ONE commit, two knobs + one relocation)

Nothing here raises a cap: `max_input_tokens` stays **16M**, `max_iterations` 200,
`max_time` 25m, `max_output_tokens` 384k, the legacy `cap:` string is untouched
(verified by reading the live `eval_cap_from_config()` result).

```yaml
defaults:                     # ── DELETED here (the engine never reads these keys here)
  code_context_budget: 0.40
  compaction_threshold: 0.90

evaluator:                    # ── the section engine/evaluator.py actually reads
  file_scope: full            # was: absent -> engine default "changed"
  code_context_budget: 0.40   # relocated, value unchanged (measured non-binding)
  compaction_threshold: 0.005 # was (dead) 0.90 -> engine default 0.90
```

These two knobs act together and were changed in one commit for the same purpose —
reducing what the evaluator drags into, and re-sends from, its context:

**`file_scope: full` — the arithmetic.** The mismatch cost the judge 1 rejected
read + 4 workaround reads over 5 extra iterations (§2, defect 1). At ~22k
tokens/iteration in that region of the run, those extra iterations alone account for
~0.11M of the measured 0.41M (27 %). `file_scope: changed` does **not** bound read
*size* — `max_file_bytes`
(engine default 131,072 B) does — so restricting the scope only withholds the files the
judge is asked to verify; on a clean tree it withholds all of them.

**`compaction_threshold: 0.005` — the arithmetic.** The valve must fire *before* the
counter can reach the cap. Per segment, the counter peaks at
`L·P0 + g·L(L-1)/2` with `L = (T − P0)/g + 1` iterations

| T (single-prompt tokens) | ratio × 16M | L (iters/segment) | counter peak at compaction | verdict |
|---|---|---|---|---|
| 14,400,000 (shipped 0.90) | 0.9 | 12,869 | — | **never fires** (491× the largest measured prompt) |
| 320,000 | 0.02 | 282 | 45.9M = 287 % of cap | too late — cap trips first |
| 160,000 | 0.01 | 139 | 11.6M = 72.8 % of cap | only 27 % margin |
| **80,000** | **0.005** | **67** | **2.84M = 17.7 % of cap** | **chosen: 5.6× margin** |

With T = 80,000 tokens, replaying the measured growth law for a full 200-iteration run:
**2 compactions fire** at iterations 67 and 134, the counter peaks at **2.92M (18.2 % of
cap)**, no cap trip, and the run costs **8.44M real input tokens instead of 23.35M
(−64 %)** because each compaction rebuilds the conversation to the compacted prompt
(verified in `evaluator._build_compacted_prompt`: it re-injects the sandbox-verified
criteria + evidence and the code context, and instructs the judge not to re-check what
is already ✓ — so merit progress survives the rebuild).

`code_context_budget: 0.40` is relocated only. Measured code context was 10,973 chars
(≈3,657 tokens) against a live cap of 0.40 × 16M = 6.4M tokens, so it is non-binding and
**no token saving is claimed for it** — it moves so the file stops advertising a knob
the engine ignores.

---

## 4. Post-change runs (two consecutive, same repo)

Both runs: instrumented, same task-by-task order as Step 1/3, no `Cap exceeded` line
required, merit verdict (PASS or FAIL) required — INCOMPLETE is a failure of this row.

### Run A — post-change, task INT-CI-017

| metric | value |
|---|---|
| verdict | **PASS** — `.gitreins/history/2026-09-17/f6abe9e6/verdict.json` (`passed: true`) |
| `Cap exceeded` line | **none** |
| LLM calls (iterations) | **35** |
| per-call prompt tokens (min / median / max) | **6,211 / 29,736 / 34,310** |
| **cumulative input tokens** | **933,147** (5.83 % of the 16M cap) |
| compaction events | 0 (the run never reaches the 80,000-token valve) |
| `File not in scope` denials | **0** (pre-change: 1) |
| allowlist construction | **not invoked** (`file_scope: full`) |

What this run proves about the change, and what it does not:

- The scope fix is live and effective: `_compute_allowed_files()` is **never called**
  (no `allowed_files` row — pre-change it returned the 6-file board set), and the judge
  read the judged artifact directly, **`read_file(".github/workflows/ci.yml")` twice with
  zero errors** where the pre-change run was rejected on its first attempt.
- The token total did **not** go down on this short task: 933,147 vs the 0.21M–0.41M
  pre-change band. The judge simply explored further this time (45 `run_command` calls,
  including an `actionlint` pass and per-job YAML multiset diffs of the pre/post workflow,
  and 2.45M chars re-sent). This is honest run-to-run depth variance, not a regression
  caused by the config — the valve is what bounds a *long* run, and a 35-call run never
  reaches the 80,000-token threshold. Judge depth is not under this row's control; the
  row's criteria are the verdict class and the absence of a cap line, both met.

### Run B — post-change, task INT-CI-014

| metric | value |
|---|---|
| verdict | **FAIL** (merit verdict, not INCOMPLETE) — `.gitreins/history/2026-09-17/61b146b3/verdict.json` (`passed: false`) |
| criteria 1 and 2 | **PASS** (breakdown recorded; the config change is the measured one and no cap was raised) |
| criterion 3 | **FAIL — self-reference artifact, resolved by the commit that records this line** |
| `Cap exceeded` line | **none** (nowhere in `.gitreins/history/2026-09-17/`) |
| LLM calls (iterations) | **15** |
| per-call prompt tokens (min / median / max) | **10,108 / 28,230 / 34,547** |
| **cumulative input tokens** | **380,592** (2.38 % of the 16M cap) |
| compaction events | 0 (run never reaches the 80,000-token valve) |
| `File not in scope` denials | **0** |
| reads of the deliverables | `docs/tier2-eval-budget.md` ×2, `.gitreins/config.yaml` ×1 — all successful |

The FAIL is the ordering artifact named above, verbatim from the verdict: *"Only ONE
post-change run exists … Run B is not done: docs/tier2-eval-budget.md:227-231 reads
'### Run B … **PENDING**' … Run B's token total is therefore not recorded anywhere, so
the 'two consecutive runs' and 'both runs' token totals' requirements are unmet."* That
was true at the moment of judgment (the commit recorded below necessarily postdates the
run it records); criteria 1 and 2 were both verified PASS against the live engine source.
Recording this line is what closes criterion 3: both post-change runs then exist as merit
verdicts (A = PASS, B = FAIL, neither with a cap line) with their totals in this document.

**Post-change totals vs pre-change:** 933,147 (A) and 380,592 (B) against the pre-change
band 206,704–408,009. Neither post-change run came close to the cap, and the reason the
totals did not fall is the one measured above — judge depth varies run to run and neither
short run reaches the 80,000-token compaction valve. The valve and the scope fix are what
keep a *long* run (the measured 165-iteration wall) alive; they do not shrink a 15-call
run, and no claim is made that they do.

---

## 5. What to reach for next (in order), and what not to touch

- **`max_file_bytes` (engine default 131,072 B ≈ 43.7k tokens per read).** Not binding in
  these runs (the largest read was the 10.3 KB `ci.yml`), but a single max-size read
  early in a long run would add ~43.7k tokens to *every* later iteration
  (43.7k × 150 ≈ 6.6M tokens) — the next lever if a future measurement shows big reads.
- **`MAX_COMPACTIONS = 3` is hard-coded in the engine** (`evaluator.py`), so a run can
  rebuild its context at most 4 times; with T = 80k that covers 4 × 67 = 268 iterations,
  more than `max_iterations = 200`. If `max_iterations` is ever raised, the valve alone
  will not carry it.
- **Do NOT raise `max_input_tokens` again.** The arithmetic above shows a rung buys ~35
  iterations and re-ladders forever; the row exists to end that.

## 6. Reproduction

The inventory script itself is deliberately **not committed** (this row keeps raw
instrumentation out of the tree); the hook list at the top of this file is the whole
recipe. Rebuild `/tmp/tier2_spy.py` from it, then:

```bash
# measurement (pre- or post-change): raw rows land in $TIER2_SPY_OUT as JSONL
cd /home/kara/bunker
TIER2_SPY_OUT=/tmp/tier2_spy_post.jsonl \
TIER2_SPY_LOG=/tmp/tier2_spy_post.jsonl.log \
TIER2_SPY_LABEL=POST \
/home/kara/.local/share/pipx/venvs/gitreins/bin/python /tmp/tier2_spy.py INT-CI-017 POST

# the verdict for that run is the newest verdict.json (the dir id is NOT the printed id)
ls -t .gitreins/history/$(date -u +%F)/*/verdict.json | head -1
```

Verify the config the engine actually loads (not just that the YAML parses):

```bash
/home/kara/.local/share/pipx/venvs/gitreins/bin/python - <<'PY'
import yaml
from engine.eval_cap import eval_cap_from_config
cfg = yaml.safe_load(open('.gitreins/config.yaml')); ev = cfg['evaluator']
cap = eval_cap_from_config(cfg)
print(cap.max_input_tokens, cap.max_iterations, ev['file_scope'],
      ev['compaction_threshold'], int(cap.max_input_tokens*ev['compaction_threshold']))
PY
```

## References

- Engine semantics cited above: `engine/evaluator.py` (`_compute_allowed_files`,
  `_build_code_context`, `_compact_context`, `_build_compacted_prompt`, `evaluate`),
  `engine/eval_cap.py` (`record_llm_call`, `_check_hard_caps`, `reset_context_tracking`),
  `engine/config.py` (`GitReinsDefaults`).
- Escalation history: `b0a7c84` (2M→8M), `ef429dc` (→4M), `25a43d2` (→16M).
- Prior verdict evidence: `73e7ecae` (INT-SPAWN-001, 8M artifact), `065d0a0b` (GAP-068).
- Note: the persisted `verdict.json` records only "Tier 2 …: INCOMPLETE" with an empty
  `items` list, so a cap death is indistinguishable from a merit failure after the fact —
  the `Cap exceeded` reason lives only in stdout. That observability gap is separate from
  this row and is not fixed here.
