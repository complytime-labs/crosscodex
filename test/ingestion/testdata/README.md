# Ingestion end-to-end fixture

`nist-sp800-171r3-excerpt.pdf` is copied unchanged from crosscodex-ingestion
`tests/fixtures/documents/nist-sp800-171r3-excerpt.pdf` (last changed in
commit `fe7fcb52548687165c6f82255ba800afc684b301`). It is PDF pages 13-17 of
NIST SP 800-171 Rev. 3 (May 2024),
<https://nvlpubs.nist.gov/nistpubs/SpecialPublications/NIST.SP.800-171r3.pdf>,
retrieved 2026-11-19. US Government work, not subject to copyright in the US
(17 U.S.C. 105).

SHA-256: `61f5d194715293bb7debdc5d8efc8285ca9f9593a3e0abbba2799a231bbbb07b`

Its converted output is `internal/catalog/testdata/ingestion/nist-sp800-171r3-excerpt.json`;
the heading tier structures it under the default config, with no LLM.
