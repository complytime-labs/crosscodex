# Crosswalk quality evaluation (maintainers)

User documentation (how to run it, what the numbers mean, limits) is in the
root README, "Evaluating crosswalk quality". This file explains how the
package works and how to add a yardstick. Design and rationale:
`docs/superpowers/specs/2026-11-07-crosswalk-eval-design.md`.

## Architecture

The eval never talks to the gateway from Go. Two pieces cooperate:

- `run.sh` (driven by `task eval:crosswalk`, defined in `.taskfiles/eval.yml`)
  is the only part that touches a daemon. It calls the `crosscodex` CLI the way
  `test/deploy/smoke_up.sh` does: `catalog import`, `run start`, `run status
  --json`, `results export --stage analyzer`, `results candidates`. All CLI
  output is machine-readable (`--json`, parsed with `jq`, which the Taskfile requires) and lands in the run
  directory.
- `package crosswalk` plus `cmd/crosswalk-eval` read files only, so every
  metric is unit-testable without a stack: `subset` (build the fixed source
  subset; it also prints the absolute path of the profile's target catalog,
  which `run.sh` imports instead of re-parsing the profile), `discover` (help
  write a profile), `score` (compute the report). The subset is deterministic:
  the same profile and source give byte-identical output, with a UUID derived
  from the source UUID and the controls.

Files in the package:

| File | Role |
|---|---|
| `profile.go` | strict YAML profile loading and validation |
| `catalog.go`, `subset.go` | OSCAL catalog reading and the source subset |
| `canonical.go`, `target_index.go` | target-driven identifier resolution and base controls |
| `references.go`, `discover.go` | reference extraction from links or CSV; `discover` report |
| `inputs.go` | loading and cross-checking the job's exports and status |
| `score.go`, `report.go` | metrics, agreement with a baseline, `report.json` / `report.md` |

Trust boundaries: profiles and catalogs are untrusted input (strict YAML
decoding, RE2 regexes with a length cap, paths confined to the repository
root). `run.sh` validates `PROFILE` and `LABEL` against strict patterns before
using them as path components and passes catalog text only to quoted arguments.
Identifier handling is driven by the target catalog's own IDs and `label`
props; the code contains no framework-specific syntax.

`run.sh` is tested by `test/tooling/eval-crosswalk.venom.yml` (run with
`task test:tooling`), which swaps the CLI for `test/tooling/lib/fake-crosscodex.sh`
and uses the profile in `testdata/profiles/synth-run.yaml`. Rerun that suite
after changing `run.sh` or `.taskfiles/eval.yml`.

## Labels

Precision needs a judgement of each produced row. The reference alone gives a
lower bound (a row it omits may be right; a row agrees when it shares the
same base control as a reference row for the same source, the recall rule,
which is symmetric), so `labels/<profile>.yaml` holds
judgements of candidate pairs. `run.sh` passes it to `score` as `--labels`
when the file exists; without it the report says "No labels" and nothing
else changes. `LABELS_DIR` overrides the directory.

```yaml
version: 1
pairs:
  - source: PO.1.1          # control IDs exactly as the exported rows write them
    target: sa-8
    verdict: related        # related | unrelated | disputed
    judged_by: [opus, sonnet] # non-empty subset of opus | sonnet | human
    rationale: Both require security requirements to be defined for development.
    date: 2026-11-12        # YYYY-MM-DD
```

The report's provenance records the labels file's SHA-256. `run.sh` stops
before the job if `labels/<profile>.yaml` exists but is not a readable file.
The loader treats the file as untrusted: unknown keys, aliases, a missing or
malformed field and a second entry for one pair are errors naming the file,
line and rule, and nothing is scored. Source and target match produced rows
by exact string, as the scorer matches them to the references. A BOM and CRLF
are accepted.

- **Add an entry** for each pair in the report's "Unlabelled rows" list.
- **Override** a model judgement by adding a second entry for the pair with
  `human` in `judged_by` (`[opus, human]` counts as human). The human entry
  wins wherever it sits in the file; delete or edit the model entry only if you
  want it gone. Two entries for a pair without a human one, or two human
  entries, are rejected.
- **Resolve `disputed`** by replacing the entry with a human `related` or
  `unrelated` one. Disputed rows are excluded from the rates and listed in the
  report as needing a decision.
- **Rates.** Labelled precision is rows labelled `related` over rows with a
  non-disputed label. Coverage is rows with a non-disputed label over all
  produced rows: read the labelled precision of a run only as far as coverage
  supports. Labels for pairs a run did not produce are ignored.

## Adding a profile

A profile (`profiles/<name>.yaml`) names a source and target catalog, a fixed
list of source `controls`, and where the references come from. The name must
match `^[a-z0-9][a-z0-9.-]*$`.

1. Fetch the catalogs: `task fetch:oscal-docs`. Where these and other test sources come from: [Sources of Test Material](../../../docs/dev/test-sources.md).
2. Find where the source catalog cites the target:

   ```bash
   task eval:crosswalk:discover \
     SOURCE=catalogs/NIST_SP800-218_ver1_catalog.json \
     TARGET=catalogs/NIST_SP-800-53_rev5_catalog.json
   ```

   For each (link `rel`, back-matter resource) pair it prints the share of
   cited identifiers that resolve against the target, for both identifier
   modes (`link_text` and `resource_title`). Pick the pair that resolves best
   and is the intended target.
3. Declare the selector in `references.oscal_links` (`rel`, plus `resource`
   `title` and/or `rlink` regexes, `identifier`, `separator`), or supply a
   `csv` of `source_id,target_id[,citation]` rows.
4. Choose 1 to 50 `controls`, spread across the source's groups. The profile
   does not bound the cost. Relationship and requires tasks per analyzer are
   candidate pairs times the panel size, and candidate pairs are the source
   items the controls decompose into (the importer turns a control with
   sub-statements into several items) times candidates per item, so the
   controls count alone understates it (SP 800-171r3: 10 controls, 46 requirement
   items with candidates, 460 candidates, 1,380 tasks per analyzer; SSDF: 10
   controls, 10 items, 100 candidates, 300 tasks). Classify and embedding also run over every control
   of both catalogs, which dominates a cold first run regardless of the
   profile and is reused from the result cache afterwards.
5. Run `task eval:crosswalk PROFILE=<name>` against a smoke stack, record the
   resolved reference count in the profile's header comment (the
   `# references:` line that `profiles_bdd_test.go` checks) and add it to the
   root README table.

### Why selectors are declared

`discover` is advisory; scoring never uses it. A selector must be explicit
because the best-resolving candidate is not always the right one. In SSDF's
`discover` output, SP 800-161 ties SP 800-53 at 100% resolution: SP 800-161
cites many of the same control labels, so choosing by resolution rate would
pick either at random, and a looser selector would silently mix both
frameworks' references into the score. The shipped profile therefore anchors
the resource title (`^SP80053$`). Review `discover` output as evidence, then
write the selector yourself.
