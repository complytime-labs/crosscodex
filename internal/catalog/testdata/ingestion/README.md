# Ingestion output fixtures

Real crosscodex-ingestion structured output, used by the
`AdaptIngestionOutput` specs. The two document fixtures (schema 1.2) are the
converter's output for document excerpts that crosscodex-ingestion ships as
its own test fixtures; they were not edited or trimmed. The ASVS and
SP 800-53 files are trimmed schema 1.3 output.

How they were made (2026-11-24): crosscodex-ingestion at commit
`d2864009864bdd1d60e3f5678105001d5a90b76a` (package 0.1.0, Docling 2.148.0,
models from that repo's pinned download in `.test-output/docling-models`), by
calling `converter.convert_document` with default `IngestionSettings`, the
packaged HTML profiles, then `output_schema.validate_output`. The result was
written with Python `json.dump(indent=2, ensure_ascii=False)`. No
`conversion_warnings` were raised. A second run gave identical output apart
from `metadata.processing_time_ms`.

| File | Input (in crosscodex-ingestion) | Input SHA-256 | Original document |
| --- | --- | --- | --- |
| `nist-sp800-171r3-excerpt.json` | `tests/fixtures/documents/nist-sp800-171r3-excerpt.pdf` (PDF pages 13-17, copied unchanged) | `61f5d194715293bb7debdc5d8efc8285ca9f9593a3e0abbba2799a231bbbb07b` | NIST SP 800-171 Rev. 3 (May 2024), <https://nvlpubs.nist.gov/nistpubs/SpecialPublications/NIST.SP.800-171r3.pdf>, retrieved 2026-11-19. US Government work, not subject to copyright in the US (17 U.S.C. 105). |
| `eurlex-nis2-32022L2555.json` | `tests/fixtures/documents/eurlex-nis2-32022L2555.html` (trimmed by whole subdivisions; see that repo's `tests/fixtures/documents/NOTICE`) | `6aa347de4e94c8f8f4e1b5264eeae462d748d1ac99df9368def5447f23851e51` | Directive (EU) 2022/2555 (NIS2, CELEX 32022L2555), <https://eur-lex.europa.eu/legal-content/EN/TXT/HTML/?uri=CELEX:32022L2555>, retrieved 2026-11-18. (c) European Union, reused under Commission Decision 2011/833/EU; only the Official Journal text is authentic. Shaped by the packaged `eur-lex-oj` HTML profile (recorded in `metadata.html_profile`). |

The input excerpts' commit is `fe7fcb52548687165c6f82255ba800afc684b301`
(last change to both files).

## Schema 1.3 structured-source fixtures

Real crosscodex-ingestion output for structured sources (element `role`s,
`catalog_title`, `catalog_version`), trimmed to a connected subset.

How they were made (2026-11-25): crosscodex-ingestion at commit `d68ecb0`
(`uv run python -m crosscodex_ingestion.structured --mapping
src/crosscodex_ingestion/mappings/<mapping>.yaml <file>`), run on a copy of the
release file in an empty directory, after checking the release's SHA-256.
Trim rule: keep every element whose root ancestor (following `parent_id` to
the element with `parent_id: null`) has a listed `id`, in the original order,
with `element_id`s as emitted, so every kept element keeps its ancestors and no
group is left without children. Then set `metadata.element_count` to the
number of kept elements and `metadata.source.node_count` to the number of kept
elements with an `id`. Write with `json.dump(indent=2, ensure_ascii=False)`
and a trailing newline. The trim is a ten-line script that is not kept in the
repo; this paragraph is its specification.

| File | Mapping | Release file SHA-256 | Kept root ids | Elements / nodes |
| --- | --- | --- | --- | --- |
| `owasp-asvs-5.0.0-excerpt.json` | `owasp-asvs-5.0.0-json` | `bcdbec214d70abcfad9284a31d4f9e5134305831d628aad3aa85d7e26626cb35` | `V1`, `V5` (chapters, all sections and requirements) | 54 / 54: 11 groups (2 chapters, 9 sections), 43 requirements, each with a `level` property |
| `nist-sp800-53r5-excerpt.json` | `nist-sp800-53r5-catalog-xlsx` | `1777ed6f839bccd55516e6e703bfe75e3cf63efb2ec80bb9309ed960af2c9ea4` | `AC-1`, `AC-2`, `AC-3` (with all their enhancements) | 33 / 31: no groups, 31 controls; enhancements such as `AC-2(1)` sit under their base control |

Release URLs and licences are in `docs/dev/test-sources.md`.

## Schema 1.4 document fixtures

The same two document sources as above, converted by the schema 1.4 ingestion
(document structure: roles, `metadata.structure_method`, the `section` role).
Unlike the 1.2 fixtures, no layout elements remain: every element is a
structure node, and both outputs have `structure_method` `heading`.

How they were made (2026-11-26): crosscodex-ingestion branch
`feat/38-document-structure` at commit `470bf52f3221d2d552b85d03709414c6c12c0bcc`
(package 0.1.0, Docling 2.148.0, models from that repo's `.test-output/docling-models`).
From that worktree, with `DOCLING_ARTIFACTS_PATH` set to the models directory,
a short `uv run python` script called `converter.convert_document(path, fmt,
IngestionSettings(), load_html_profiles(""), load_registry(""))` (the call
`worker.py` makes), dropped `conversion_warnings`, ran
`output_schema.validate_output`, and wrote the result with
`json.dump(indent=2, ensure_ascii=False)` and a trailing newline. The
script is not kept in the repo; this paragraph is its specification. The inputs
and their SHA-256 are those in the first table.

| File | Input (in crosscodex-ingestion) | Notes |
| --- | --- | --- |
| `nist-sp800-171r3-excerpt-1.4.json` | `tests/fixtures/documents/nist-sp800-171r3-excerpt.pdf` (`pdf`) | 13 controls, no section; `page` set |
| `eurlex-nis2-32022L2555-1.4.json` | `tests/fixtures/documents/eurlex-nis2-32022L2555.html` (`html`) | 36 nodes: 34 controls and 2 `section` nodes (Article 2 lead-ins whose points are their children); `page` null |

`owasp-asvs-5.0.0-excerpt-1.4.json` is not a converter output. It is
`owasp-asvs-5.0.0-excerpt.json` with `schema_version` set to `1.4` and
`metadata.structure_method` set to `mapping` (placed after `metadata.dropped`),
so a mapped import under schema 1.4 can be tested. It has the same release file
and mapping as the 1.3 row above.
