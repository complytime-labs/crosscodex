# OSCAL mapping export

`crosscodex results export` can write a completed job's mappings as an [OSCAL](https://pages.nist.gov/OSCAL/) 1.2.3 mapping collection, so other OSCAL tooling can read them.

```sh
crosscodex results export <job-id> --format oscal-mapping --output mapping.json
```

The `oscal-mapping` format is new; `json` and `csv` keep their layout (the one change, a trimmed `--control`, is in the README "Upgrading" notes). `results candidates` does not accept `oscal-mapping` (candidates are not mappings).

## What the document says

The output is a `mapping-collection`, deterministic and checked against the OSCAL schema before anything is written.

- **Mappings.** One `mapping` per direction. The forward mapping holds rows from the job's source catalog to its target catalog. A bidirectional job also gets a reverse mapping. Relationships are never flipped between directions. A job whose source and target are the same catalog gets a single mapping over one resource.
- **Catalogs.** Each catalog is a `back-matter` resource. Its title is the catalog name plus its version. The `catalog-id` property holds the content-derived catalog ID, which identifies the exact input; `version-display` is added when the catalog has one. CrossCodex does not know a catalog's source URL, so there is no `rlink`.
- **Maps.** Each row is one `map` with the relationship, a `confidence-score` (0 to 1, rounded to four decimals; 0 is written as `0`), and one source and one target control, identified by their OSCAL control IDs. Properties carry `mapping-id`, plus `cleaning-state` (`restored` or `stale`) when edge cleaning or a reviewer changed the row, and `critique-outcome` (`upheld`, `split`, `rejected` or `failed`) on `--stage analyzer` rows the critique reviewed.
- **Provenance.** Method `automation`, matching rationale `semantic`, status `draft`. Properties record the `job-id`, the `stage` (also recorded as `filter-stage`, because the stage is a filter), one `filter-<name>` for each filter you set (`min-confidence`, `type`, `control`, `exclude-rejected`), and one `model` per model, written `alias=observed-model` or `alias=observed-model@revision`. The models come from the panels the job ran, so the document names what produced the mappings. It lists the panel models of every stage of the job, whatever `--stage` was exported.
- **Metadata.** `last-modified` is when the job's last stage completed, and `version` is the job ID.
- **Identifiers.** Every UUID is derived from the content, so exporting the same job with the same filters gives byte-identical output. Changing a filter or a row changes the document UUID.

Catalog names and versions are flattened to one line, because the OSCAL schema forbids line breaks in titles and descriptions. The `catalog-id` property still identifies the exact catalog. Catalog names reach OSCAL title fields as-is, so any Markdown in them is kept and may be rendered by downstream tools.

Custom properties use the namespace `https://compliance-mapper/ns`.

### Relationships

OSCAL defines `equivalent-to`, `superset-of`, `subset-of` and `no-relationship` (among others). The other four CrossCodex types have no OSCAL equivalent, so they are written with the namespace above on the `map`. Tools that do not know the namespace will not recognise them.

| CrossCodex | OSCAL `relationship` | Definition |
|------------|----------------------|------------|
| `EQUIVALENT` | `equivalent-to` | Same scope and intent; either requirement could satisfy the other. |
| `SUPERSET_OF` | `superset-of` | Source is broader; target is a specific instance of source. |
| `SUBSET_OF` | `subset-of` | Source is narrower; source is a specific instance of target. |
| `CONTRIBUTES_TO` | `contributes-to` (CrossCodex namespace) | Target measurably advances the source's obligation but neither fully encompasses the other. Requires shared subject matter, not merely shared vocabulary. |
| `COMPLEMENTS` | `complements` (CrossCodex namespace) | Same risk domain, different mechanism, no scope overlap; jointly necessary for complete coverage. |
| `PARTIAL` | `partial` (CrossCodex namespace) | Adjacent domains with a tangential connection; shared governance vocabulary or an adjacent risk area but different subject matter, negligible coverage contribution. |
| `CONFLICTS_WITH` | `conflicts-with` (CrossCodex namespace) | Direct operational contradiction; satisfying one violates or undermines the other. |
| `NO_RELATIONSHIP` | `no-relationship` | Different functional domains or intent; no genuine compliance relationship. |

OSCAL's `equal-to` and `intersects-with` are never written: CrossCodex makes no claim that matches them. `PARTIAL` in particular is not an overlap.

## What is left out, and why

- **Retired rows.** A mapping that edge cleaning or a reviewer retired is no longer a current result, so it is not exported. `--format json` still shows it with its cleaning record.
- **Vetoed and non-viable pairs.** An OSCAL `map` asserts a relationship, and these pairs are not mappings. `--include-vetoed` and `--include-non-viable` are therefore refused with `oscal-mapping`; use `--format json` to see them.
- **Everything you filter out.** The filters you set are recorded in the provenance, so a reader can tell a partial export from a complete one.

## Why the status is `draft`

The mappings are machine output from LLM panels. Nobody has reviewed them, so the document does not claim otherwise. Review, then change the status in your own copy if your process calls for it.

## When the export refuses

A refusal writes nothing: `--output` is not created, and an existing file is not truncated.

| Refused when | What to do |
|--------------|------------|
| The job is not completed. | Wait for it to finish, or export `--format json` for partial results. |
| The job recorded no stage completion time, or no panel model. | Export `--format json`. The document would be undated or would not say what produced the mappings. |
| `--include-vetoed` or `--include-non-viable` is set. | Drop the flag, or export `--format json`. |
| No rows remain after the filters and the retired rows are removed. | Widen the filters, or export `--format json` to see every row. |
| A row has a relationship this version cannot express, a confidence outside 0 to 1, or control IDs that belong to neither catalog. | Export `--format json`. The error names the mapping. |
| A recorded value, such as a model name or filter, is not valid in OSCAL. | Export `--format json`. The error quotes the schema problem. |

## Validating the output

CrossCodex validates the document against the embedded OSCAL 1.2.3 schema before it writes it. To check it independently, run it through the validate command of NIST's [oscal-cli](https://github.com/usnistgov/oscal-cli). Check `oscal-cli --help` for the exact command and its options in your version.
