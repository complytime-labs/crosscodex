# Tree fixtures

Excerpts of published OSCAL catalogs, used by the `pkg/oscal` tree specs. Each
file keeps a few real controls and drops the rest. Content is verbatim except
where noted: the SP 800-53r5 and SP 800-171r3 excerpts omit the control's
`guidance` part.

The exact upstream revision of each excerpt was not recorded when it was made,
so the source URL below points at the current `main` (or release tag) and may
be newer than the excerpt. The SHA-256 is of the full original downloaded on
the retrieval date. Verification on that date: every control ID in each excerpt
exists in the original, the excerpt's `metadata` title, version and
last-modified match, and `props` are identical (all four files), `title`
and `parts` are identical for SSDF and ISM.

Retrieval date for all entries: 2026-11-11.

Full (untrimmed) sources not shipped in the repo are listed in [Sources of Test Material](../../../../docs/dev/test-sources.md).

| File | Excerpts | Source URL | SHA-256 of original |
| --- | --- | --- | --- |
| `sp800-53r5.json` | NIST SP 800-53 Rev 5.2.0 catalog (`ac` group; `ac-2`, `ac-2.7`) | <https://raw.githubusercontent.com/usnistgov/oscal-content/main/nist.gov/SP800-53/rev5/json/NIST_SP-800-53_rev5_catalog.json> | `64ca6b4161de51ab6af098a7d101d326f14c95e1abf721396cd8a049e8115eeb` |
| `sp800-171r3.json` | NIST SP 800-171 Rev 3 catalog (`03.01` family; `03.01.01`) | <https://raw.githubusercontent.com/usnistgov/oscal-content/main/nist.gov/SP800-171/rev3/json/NIST_SP800-171_rev3_catalog.json> | `fd68bad4c1fee98dd1b2511884b585fb1f14265d4c5693d0492a25dd66e73dde` |
| `ssdf.json` | NIST SP 800-218 SSDF (ver1) catalog (`PO` group) | <https://raw.githubusercontent.com/usnistgov/oscal-content/main/nist.gov/SP800-218/ver1/json/NIST_SP800-218_ver1_catalog.json> | `fb864f27ccfe2c501737455180db91d8c05285a2ebb3fe1bdf1e193b06081634` |
| `ism-e8.json` | ACSC ISM Essential Eight Maturity Level One resolved baseline catalog, release v2026.09.4 | <https://raw.githubusercontent.com/AustralianCyberSecurityCentre/ism-oscal/v2026.09.4/ISM_E8_ML1-baseline-resolved-profile_catalog.json> | `11ecf7897a2a50cc1a9986b79bbba33f1a0be2c1ca83b0fa99f7033c5f04a4c0` |

The excerpts below were trimmed from copies downloaded on 2026-11-10
(2026-11-10T22:18:12Z) and re-downloaded from the listed URL on 2026-11-11;
both copies had the SHA-256 shown, so the URL pins the excerpt's revision as of
that date. Each keeps one group and the named controls verbatim (with their
ancestors), and drops the other groups and controls and the catalog's
`back-matter`; `metadata` is unchanged.

| File | Excerpts | Source URL | SHA-256 of original |
| --- | --- | --- | --- |
| `ai-rmf.json` | NIST AI 100-1 AI RMF 1.0 catalog (`MANAGE` group; `MANAGE_4`, `MANAGE_4.1`, `MANAGE_4.2`: statement-less prose in `overview`) | <https://raw.githubusercontent.com/usnistgov/oscal-content/main/nist.gov/AI100-1/ver1/json/NIST_AI-RMF_v1_catalog.json> | `09de09ecd15f861c4ec05aa192872dc2faafc3bef41c4bc57e19247e84f92545` |
| `cis-rhel9.json` | ComplianceAsCode CIS RHEL 9 catalog (`cis_rhel9_1` group; `cis_rhel9_1-1`, `cis_rhel9_1-1.1`, `cis_rhel9_1-1.1.1`, `cis_rhel9_1-1.1.2`: title-only controls) | <https://raw.githubusercontent.com/ComplianceAsCode/oscal-content/main/catalogs/cis_rhel9/catalog.json> | `51d3d77139a95592d11f475abcdac662c00113354cce5b20b9142d9a1205ca0c` |
| `pcidss-4.json` | ComplianceAsCode PCI DSS 4 catalog (`pcidss_4_2` group; `pcidss_4_2-1`, `pcidss_4_2-1.1` without a statement, `pcidss_4_2-1.2` with one) | <https://raw.githubusercontent.com/ComplianceAsCode/oscal-content/main/catalogs/pcidss_4/catalog.json> | `c9d03b78609bbb883cd2ea8de19f48f6e8c824ef5436ce94d090efe72627568b` |
