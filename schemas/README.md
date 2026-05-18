# Schemas Directory

This directory contains compliance and validation schemas used by CrossCodex for parsing and validating compliance frameworks.

## Directory Structure

```
schemas/
├── oscal/           # OSCAL (Open Security Controls Assessment Language) schemas
│   └── {version}/   # Version-specific schemas
│       ├── json-schema/   # JSON Schema definitions
│       └── metaschema/    # OSCAL metaschema definitions (XML)
└── gemara/          # Gemara CUE validation files
```

## Fetching Schemas

Schemas are not committed to the repository and must be fetched using the Taskfile task:

```bash
task fetch-schemas
```

This task will:
- Fetch the latest OSCAL schemas from the [NIST OSCAL GitHub releases](https://github.com/usnistgov/OSCAL/releases)
- Fetch the latest gemara CUE schemas from the [gemara GitHub repository](https://github.com/gemaraproj/gemara)
- Organize them by version under `schemas/oscal/{version}/` and `schemas/gemara/{version}/`
- Download OSCAL JSON Schema and metaschema definitions
- Download all gemara CUE validation files

## OSCAL Schemas

The following OSCAL model schemas are fetched:

### JSON Schemas (`json-schema/`)
- `oscal_complete_schema.json` - Complete OSCAL schema (all models)
- `oscal_catalog_schema.json` - Catalog model (control catalogs like NIST 800-53)
- `oscal_profile_schema.json` - Profile model (baselines, overlays)
- `oscal_component_schema.json` - Component Definition model
- `oscal_ssp_schema.json` - System Security Plan model
- `oscal_assessment-plan_schema.json` - Assessment Plan model
- `oscal_assessment-results_schema.json` - Assessment Results model
- `oscal_poam_schema.json` - Plan of Action and Milestones model

### Metaschemas (`metaschema/`)
- `oscal_catalog_metaschema_RESOLVED.xml` - Catalog metaschema
- `oscal_profile_metaschema_RESOLVED.xml` - Profile metaschema
- `oscal_component_metaschema_RESOLVED.xml` - Component Definition metaschema
- `oscal_ssp_metaschema_RESOLVED.xml` - SSP metaschema

## Gemara CUE Files

The `gemara/{version}/` directory contains CUE-based validation schemas from the [gemara project](https://github.com/gemaraproj/gemara). Gemara is a standardized, machine-readable data model for governance and risk assessment that bridges compliance requirements with technical evidence.

### Schema Files

The following CUE schemas are fetched:

| Schema | Purpose |
| ------ | ------- |
| `auditlog.cue` | Audit logging structures |
| `capabilitycatalog.cue` | Capability tracking and cataloging |
| `collections.cue` | Collection definitions and structures |
| `controlcatalog.cue` | Security control definitions |
| `enforcementlog.cue` | Enforcement action logging |
| `entities.cue` | Entity definitions and relationships |
| `evaluationlog.cue` | Evaluation records and results |
| `guidancecatalog.cue` | Guidance documentation structures |
| `lexicon.cue` | Term definitions and vocabulary |
| `mappingdocument.cue` | Control mapping schemas |
| `mapping_inline.cue` | Inline mapping definitions |
| `metadata.cue` | Metadata structures |
| `policy.cue` | Policy definitions and validation |
| `principlecatalog.cue` | Security principles catalog |
| `riskcatalog.cue` | Risk catalogs and classifications |
| `threatcatalog.cue` | Threat models and cataloging |
| `vectorcatalog.cue` | Attack vector definitions |

These CUE schemas provide strongly-typed validation and constraint checking for compliance data, complementing OSCAL's JSON Schema definitions.

## Usage in CrossCodex

The Catalog Service uses these schemas to:
- Validate imported OSCAL documents
- Ensure compliance framework documents conform to OSCAL specifications
- Support multiple OSCAL versions for backward compatibility

## Version Management

Schemas are organized by version to support:
- Testing against multiple OSCAL versions
- Upgrading OSCAL support without breaking existing functionality
- Version-specific validation rules

To fetch a specific OSCAL version (instead of latest), modify the `OSCAL_VERSION` variable in the `fetch-schemas` task.

## References

- [OSCAL Project](https://pages.nist.gov/OSCAL/)
- [OSCAL GitHub Repository](https://github.com/usnistgov/OSCAL)
- [OSCAL Documentation](https://pages.nist.gov/OSCAL/documentation/)
