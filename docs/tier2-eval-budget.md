# Tier 2 evaluator input budget — measured breakdown and the fix (INT-CI-014)

**Measured:** 2026-09-17 on repo HEAD `848625c` (pre-change config sha256 `734f9fbf…b5189`)
**Runs:** ten instrumented runs on this repo — two pre-change (`INT-CI-017`), eight
post-change (five on `INT-CI-017`, three on `INT-CI-014`). The two consecutive post-change
merit runs are F and G (§4.3); runs E and H are the live compaction witnesses, and run H —
this row judging itself — is the final verification (§4.6).
**Instrument:** `/tmp/tier2_spy.py` — runs the installed `gitreins` CLI in-process
(`argv = ["judge", <id>]`) under the pipx venv interpreter, with `logging.DEBUG` and
monkey-patches on exactly three runtime symbols:
`engine.eval_cap.EvalCap.record_llm_call` (per-call tokens),
`EvalCap.reset_context_tracking` (the counter reset that only compaction performs),
`AgenticEvaluator._compact_context` / `_build_code_context` / `_compute_allowed_files`,
plus `LLMClient.chat` (what is re-sent) and `AgenticEvaluator._execute_tool` (tool
results). Every row is written to JSONL as it happens. Raw dumps stay in `/tmp`
(`/tmp/tier2_spy_PRE.jsonl`, `…_v1.jsonl`, `…_POSTA.jsonl` … `…_POSTH.jsonl`) and are
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
why the ladder never converged. Refitting every run in §4.5 gives its own wall between
iteration **132 and 200** — same shape, run-dependent slope.

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

Independent confirmation of the threshold's size (added after runs C and E, §4.4): the
deepest uncompacted run measured on this repo reached a **78,930-token** single prompt at
48 LLM calls (**98.7 %** of the valve) without tripping it, and the next deep run (run E)
crossed the valve at **80,860 tokens / iteration 65** and rebuilt its context. So the valve
sits just above what a genuinely deep run reaches (it does not fire prematurely and does
not discard context the judge is using) while keeping the per-segment counter at 17.7 % of
the cap.

---

## 4. Post-change runs (instrumented, same repo)

Ten instrumented runs exist in total — two pre-change, eight post-change (eight because
this row's own verification uncovered two engine properties that forced extra runs; §4.4,
§4.6).
All verdict files are `.gitreins/history/2026-09-17/<id>/verdict.json`, and no engine
`Cap exceeded` line exists in any of them (the phrase appears only inside the stored
criterion text / judge prose that quotes it).

### 4.1 The engine's verdict class (why the runs below are labeled the way they are)

`Verdict.verdict` has exactly two values, `"COMPLETE" | "INCOMPLETE"` (`evaluator.py:368`);
`_parse_verdict` coerces anything else — including a model-emitted `"FAIL"` — to
`INCOMPLETE` (`:1869-1871`); the partial path sets `"COMPLETE" if complete else
"INCOMPLETE"` with `complete = all(items PASS)` (`:1310-1312`); and `judge.py:175` computes
`result.passed = tier2.verdict == "COMPLETE"`. So a merit FAIL is not representable as a
*class* in this engine version — every run that finds an unmet criterion prints
`Tier 2 (Agentic Evaluator): INCOMPLETE` beside `Overall: FAIL`, the same label a cap death
produces. The witness for the change must therefore be runs that reached a verdict on the
merits **and** are free of cap deaths: class `COMPLETE` with no `Cap exceeded` line, which a
starved run can never fake (§1.2 — a cap death returns `INCOMPLETE`).

### 4.2 All ten runs

| run | task | class | overall | calls | per-call prompt min/median/max | cumulative (counter) | real input sent | % of cap | compactions | cap line |
|---|---|---|---|---|---|---|---|---|---|---|
| PRE v1 `e6d24301` | INT-CI-017 | COMPLETE | PASS | 13 | 6,211 / 15,084 / 24,507 | 206,704 | 206,704 | 1.29 % | 0 | none |
| PRE v2 `616c1d33` | INT-CI-017 | COMPLETE | PASS | 23 | 6,211 / 18,053 / 29,323 | 408,009 | 408,009 | 2.55 % | 0 | none |
| POST A `f6abe9e6` | INT-CI-017 | **COMPLETE** | **PASS** | 35 | 6,211 / 29,736 / 34,310 | 933,147 | 933,147 | 5.83 % | 0 | none |
| POST B `61b146b3` | INT-CI-014 | INCOMPLETE | FAIL | 15 | 10,108 / 28,230 / 34,547 | 380,592 | 380,592 | 2.38 % | 0 | none |
| POST C `dcb853ee` | INT-CI-014 | INCOMPLETE | FAIL | 48 | 6,177 / 47,890 / 78,930 | 2,301,457 | 2,301,457 | 14.38 % | 0 | none |
| POST D `a713a269` | INT-CI-017 | **COMPLETE** | **PASS** | 25 | 6,211 / 23,971 / 36,776 | 613,063 | 613,063 | 3.83 % | 0 | none |
| POST E `2aaef334` | INT-CI-014 | INCOMPLETE | FAIL | 74 | 6,177 / 41,659 / **80,860** | 445,943 | 3,221,740 | 2.79 % (counter) | **1** | none |
| **POST F `07b37f81`** | INT-CI-017 | **COMPLETE** | **PASS** | 15 | 6,211 / 20,469 / 31,504 | **308,413** | 308,413 | 1.93 % | 0 | none |
| **POST G `45cdbd05`** | INT-CI-017 | **COMPLETE** | **PASS** | 15 | 6,211 / 23,180 / 32,580 | **321,672** | 321,672 | 2.01 % | 0 | none |
| **POST H `8c923e98`** | INT-CI-014 | **COMPLETE** | **PASS** | **144** | 6,177 / 30,438 / 80,207 | 940,270 | **7,243,486** | 5.88 % (counter) | **1** | none |

"cumulative (counter)" is what `EvalCap.cumulative_input_tokens` reported at the end —
the number the 16M cap is checked against; "real input sent" is the sum of every
`prompt_tokens` in the run, which diverges from it exactly when a compaction reset the
counter (run E).

### 4.3 The two CONSECUTIVE post-change merit runs — F and G

| | run F (`07b37f81`) | run G (`45cdbd05`) |
|---|---|---|
| task | INT-CI-017 | INT-CI-017 |
| class / overall | **COMPLETE / PASS** | **COMPLETE / PASS** |
| `passed` / items in verdict.json | `true` / `["PASS"]` | `true` / `["PASS"]` |
| LLM calls | 15 | 15 |
| per-call prompt min/median/max | 6,211 / 20,469 / 31,504 | 6,211 / 23,180 / 32,580 |
| **cumulative input tokens** | **308,413** | **321,672** |
| compaction events | 0 | 0 |
| `Cap exceeded` line | none | none |
| `File not in scope` denials | 0 | 0 |
| allowlist construction | not invoked (`file_scope: full`) | not invoked |

F and G ran back to back with no evaluation between them (the only post-change runs after
G are the self-judgments discussed in §4.4), so they are a **consecutive** pair that both
finished with a merit verdict and no cap line — criterion 3's requirement. Runs A and D
were also merit runs (933,147 and 613,063) but are separated by B and C, so they are not
the consecutive pair; the earlier revision of this document wrongly offered them as the
evidence and the row's own judge rejected them on exactly that ground (§4.4, run E).

What the whole post-change set shows, and what it does not:

- **The scope fix is live.** `_compute_allowed_files()` is never invoked in any
  post-change run (no `allowed_files` row; pre-change it returned a 6-file board-only set),
  and the judge reads the judged artifact directly — run A read
  `read_file(".github/workflows/ci.yml")` twice with zero errors, where the pre-change run
  was denied on its first attempt. `File not in scope` denials post-change: **0**.
- **The valve works live** — run E fired it (§4.4). No post-change run has a cap line.
- **Nothing here claims the change shrinks a short run.** Post-change totals (308,413 /
  321,672 / 613,063 / 933,147) overlap the pre-change band (206,704 / 408,009): judge depth
  is not under this row's control (run A issued 45 `run_command` calls, an `actionlint`
  pass and per-job YAML multiset diffs, re-sending 2.45M chars). The valve bounds a *long*
  run; a 15-call run never reaches the 80,000-token threshold.

### 4.4 Run E — the valve fires live — and the self-judgments (B, C, E; H in §4.6)

**Run E (`2aaef334`) is the first run in which compaction engaged, exactly as designed**
(run H engaged it a second time, §4.6):

```
gitreins.evaluator WARNING  Context near limit (80860/16000000 tokens) — compacting (compaction #1)
gitreins.evaluator INFO     Compacting evaluator context (compaction #1, 125 messages → clean slate)
                            messages 125 → 2, new prompt 13,003 chars
EvalCap.reset_context_tracking: counter 2,775,797 → 0   at iteration_credit 65.4
run totals: 74 LLM calls, real input 3,221,740, final counter 445,943, 0 cap lines
```

- The threshold was crossed at **80,860 tokens**, i.e. the first prompt past the configured
  80,000-token valve, at **iteration 65** — against the §3 model's prediction of
  L = (80,000 − 5,435)/1,119 + 1 = **67** iterations.
- The counter stood at 2,775,797 when it fired = **17.3 %** of the cap, matching the §3
  prediction of a 2.84M per-segment peak (17.7 %) within 2 %.
- The run then continued for another ~9 iterations of fresh context and ended with a
  counter of 445,943 while having really sent 3,221,740 input tokens across 74 calls —
  i.e. **the counter no longer tracks the wall, because the conversation it measures was
  rebuilt**, and the run finished with no cap line.
- Run E's judge verified criteria 1 and 2 PASS (the third time criteria 1–2 have been
  independently confirmed) and failed criterion 3 because the merit runs it could see
  (A and D) were not consecutive — the finding that produced runs F and G.

**Runs B and C** (both on INT-CI-014, the row itself) are recorded for completeness, not as
merit runs:

- **Run B** (`61b146b3`, 15 calls, 380,592 tokens, no cap line): criteria 1 and 2 PASS;
  criterion 3 failed because the document it was reading still said "Run B … PENDING" — a
  run's record cannot precede its own verdict.
- **Run C** (`dcb853ee`, 48 calls, 2,301,457 tokens = 14.38 % of cap, no cap line): rejected
  the earlier revision's claim that run B was a "merit FAIL" (correctly, per §4.1) and
  failed criterion 3 under the same consecutive-runs reading. Its largest prompt —
  **78,930 tokens** — is 98.7 % of the valve, and run E crossed it 65 iterations in, which
  is how the 80,000-token size was confirmed rather than assumed.
- **Why a self-judgment of this row cannot be a merit run:** a run that judges INT-CI-014
  reads this document, and this document can only record a run after that run produced a
  verdict. The last word therefore belongs to the independent task runs (A, D, F, G).

### 4.5 Growth-law sensitivity (why the fix is about the shape, not the constant)

Each run gets its own least-squares fit of `prompt(i) = P0 + g·(i-1)`; the cumulative
counter would reach the 16M cap at:

| run | calls | P0 | g (tokens/iteration) | cap wall at iteration |
|---|---|---|---|---|
| `616c1d33` (the §2 fit) | 23 | 5,435 | 1,119 | 165 |
| `e6d24301` | 13 | 6,097 | 1,634 | 137 |
| `f6abe9e6` | 35 | 15,631 | 649 | 200 |
| `61b146b3` | 15 | 13,898 | 1,639 | 132 |
| `dcb853ee` | 48 | 15,816 | 1,367 | 143 |
| `a713a269` | 25 | 10,757 | 1,147 | 159 |
| `07b37f81` | 15 | 8,483 | 1,725 | 132 |
| `45cdbd05` | 15 | 8,535 | 1,844 | 128 |
| `2aaef334` (post-compaction segment) | 74 | 31,978 | 317 | 233 |

The absolute wall lands between **iteration 128 and 200** across the eight uncompacted
profiles — a full `max_iterations = 200` run is at or past the edge for all but one. Every
profile has the same *shape*: the per-iteration prompt grows about linearly because the
whole conversation is re-sent, so the counter is quadratic. That is what makes this a
justification for the valve and the scope fix rather than for another budget rung. Run E's
post-compaction segment shows the intended consequence: a much flatter slope (g = 317)
because the rebuilt conversation starts from the compacted prompt instead of the whole
accumulated history.


### 4.6 Final verification — the row judges itself and passes

Run H (`8c923e98`, commit `4755e92`): **`Tier 2 (Agentic Evaluator): COMPLETE` /
`Overall: PASS`**, items `["PASS","PASS","PASS"]`, `passed: true` — all three of this
row's criteria verified against the live tree, including criterion 3, which cites runs F
and G and their mtimes (F 15:17:24, G 15:18:10, no verdict between them).

| metric | value |
|---|---|
| verdict | **PASS** — `.gitreins/history/2026-09-17/8c923e98/verdict.json` |
| LLM calls | **144** (the longest run measured on this repo) |
| per-call prompt min/median/max | 6,177 / 30,438 / **80,207** |
| real input tokens sent | **7,243,486** (45.3 % of the cap) |
| counter at the end | **940,270** (5.88 %) |
| compaction events | **1** — `"Context near limit (80207/16000000 tokens) — compacting (#1)"`, 237 messages → 2 |
| `Cap exceeded` line | none |

Honest reading of what run H adds. It is **not** a rescue: without the valve this run's
cumulative would have been 7,243,486 = 45.3 % of the cap, so a 144-call run of *this*
shape would not have died anyway. What it does show is the mechanism working on a long,
real run — the valve fired once, rebuilt a 237-message conversation to a 2-message one,
and the run continued to a COMPLETE merit verdict with the counter at 5.88 % rather than
45.3 %. The runs that *were* dying (the 2M→8M rungs) were of the steeper shapes in §4.5,
where the wall sits at iteration 128–165; the valve now fires at iteration ~65 on every
shape, well before any of them.

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
