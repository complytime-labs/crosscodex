# Ingestion output fixtures

Real crosscodex-ingestion structured output (schema 1.2), used by the
`AdaptIngestionOutput` specs. Each file is the converter's output for a
document excerpt that crosscodex-ingestion ships as its own test fixture; it
was not edited or trimmed.

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
