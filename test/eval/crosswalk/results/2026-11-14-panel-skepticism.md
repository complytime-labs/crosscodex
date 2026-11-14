# 2026-11-14: independent panels, sceptical prompts and critique (#260)

A dated record of one measurement, kept so later runs have a baseline. Each
run is one job per profile, scored with `crosswalk-eval score` and the labelled
samples in `labels/` as committed with #260 (every pair judged; none
disputed). Numbers from a later labels file are not comparable without
rescoring these runs.

## Runs

Each run adds one change to the one before it.

| Run | Relationship / requires panel | Prompts | Critique |
|---|---|---|---|
| A | one model sampled 3 times (the build before #260) | before #260 | none |
| B | three aliases, one sample each: gpt-oss 20B, a Qwen 35B-A3B MoE and Qwen 27B dense | before #260 | off |
| C | as B | #260 (sceptical, `SANITY_CHECK` line) | off |
| D | as B | #260 | on, `veto: true`, same three models |

All four ran on locally served open-weight models. B was a scratch build of
#260 with only the relationship and requires prompt, guidance and parser
files restored to their pre-#260 versions. The panel analyzers ran with
`max_tokens: 2048`.

## Results

"Correct" is the number of produced rows labelled `related`. Recall is out of
the profile's reference pairs (SSDF 36, SP 800-171r3 28). LLM calls and wall
time are those the job made: D reused C's cached relationship and requires
results, so its figures are mostly the critique's, and a D from scratch costs
about C plus D.

### SSDF → SP 800-53r5

| Run | Recall exact | Recall base | Rows | Reference agreement | Labelled precision | Correct | LLM calls | Wall |
|---|---|---|---|---|---|---|---|---|
| A | 12/36 | 18/36 | 96 | 37.5% | 40.6% | 39 | 610 | 1833 s |
| B | 13/36 | 18/36 | 59 | 52.5% | 66.7% | 38 | 600 | 1850 s |
| C | 12/36 | 16/36 | 49 | 55.1% | 75.0% | 36 | 400 * | 2136 s * |
| D | 8/36 | 12/36 | 23 | 60.9% | 87.0% | 20 | 450 | 1344 s |

\* C reused 200 relationship results from an earlier attempt of the same
build, and its wall time includes a resume.

### SP 800-171r3 → SP 800-53r5

| Run | Recall exact | Recall base | Rows | Reference agreement | Labelled precision | Correct | LLM calls | Wall |
|---|---|---|---|---|---|---|---|---|
| A | 24/28 | 25/28 | 455 | 77.1% | 29.7% | 135 | 4555 | 11939 s |
| B | 24/28 | 25/28 | 353 | 81.6% | 37.4% | 132 | 2760 | 7147 s |
| C | 24/28 | 25/28 | 276 | 83.0% | 46.7% | 129 | 2760 | 7514 s |
| D | 18/28 | 23/28 | 98 | 85.7% | 79.6% | 78 | 894 | 1959 s |

Label coverage was 97–100% of produced rows in every run.

## Findings

- **Independent panel (A→B):** the largest precision gain at almost no recall
  cost. It also changes the model, so panel design and model can't be
  separated with these runs.
- **Sceptical prompts (B→C):** fewer rows and higher precision on both
  profiles, losing 2–3 correct rows.
- **Critique with veto (C→D):** precision of 80–87%, but about 40% of the
  correct rows and 4–6 exact matches are dropped. This is why
  `analysis.critique.veto` defaults to `false`. With veto off, a job keeps C's
  rows with each critique outcome attached, and
  `results export --stage analyzer --exclude-rejected` gives D's strict view
  of the same job without a re-run.
- **Reference agreement vs labelled precision:** the SP 800-171r3 reference is
  broad, so most rows agree with it while the labels reject many. Report both.
- **Answer budget:** one gpt-oss requires call used a whole 1024-token budget
  (median about 100) and stopped the stage, which is why the shipped panel
  budget is 2048.

## Caveats

- One job per run and profile, and LLM output is not deterministic.
  Differences of 1–2 rows or matches are noise.
- The labels are LLM judgements (Opus and Sonnet), not human ones. The pairs
  the first judges disputed were re-judged with full control context, and the
  orchestrating model broke the remaining ties (`judged_by: [opus]`).
- Two of the three panel models are Qwen, so the panel is less independent
  than the design intends.

## Reproducing

1. Bring up a smoke stack against an OpenAI-compatible server whose three
   panel models can stay loaded together (see `deploy/README.md`), with
   `OPENAI_COMPAT_PANEL_MODELS=<model-1>,<model-2>,<model-3>`.
2. For each profile, run `task eval:crosswalk PROFILE=<profile> LABEL=<run>`.
   Set `analysis.critique.enabled: false` for runs B and C, and
   `enabled: true` with `veto: true` for D. Restart `crosscodexd` after each
   config change.
3. For a run that stopped and was resumed with `crosscodex run retry
   --from-failure`, score it as `run.sh` does (export, candidates and status
   of the job, then `crosswalk-eval score`); #278 tracks a supported way.
4. Compare with `BASELINE=<earlier run>/report.json`, or rescore earlier run
   directories with `crosswalk-eval score --labels labels/<profile>.yaml`
   after the labels change.

A smoke stack brought up from another checkout generates its own attestation
key, so the daemon doesn't trust the earlier checkout's cached results and
recomputes every stage. Reuse the earlier checkout's
`.test-output/deploy-smoke/certs/` to keep the cache.
