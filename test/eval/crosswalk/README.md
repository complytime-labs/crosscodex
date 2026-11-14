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

## Adding a profile

A profile (`profiles/<name>.yaml`) names a source and target catalog, a fixed
list of source `controls`, and where the references come from. The name must
match `^[a-z0-9][a-z0-9.-]*$`.

1. Fetch the catalogs: `task fetch:oscal-docs`.
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
