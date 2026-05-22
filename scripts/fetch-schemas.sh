#!/usr/bin/env bash
set -euo pipefail

for cmd in curl sha256sum; do
	command -v "$cmd" >/dev/null 2>&1 || {
		echo "ERROR: required command '$cmd' not found" >&2
		exit 1
	}
done

OSCAL_VERSION="${1:?Usage: fetch-schemas.sh <oscal-version> <gemara-version> <schemas-dir>}"
GEMARA_VERSION="${2:?Usage: fetch-schemas.sh <oscal-version> <gemara-version> <schemas-dir>}"
SCHEMAS_DIR="${3:?Usage: fetch-schemas.sh <oscal-version> <gemara-version> <schemas-dir>}"

OSCAL_BASE_URL="https://github.com/usnistgov/OSCAL/releases/download/v${OSCAL_VERSION}"
GEMARA_BASE_URL="https://raw.githubusercontent.com/gemaraproj/gemara/v${GEMARA_VERSION}"

echo "Fetching OSCAL schemas version ${OSCAL_VERSION}..."
mkdir -p "${SCHEMAS_DIR}/oscal/${OSCAL_VERSION}/json-schema"
mkdir -p "${SCHEMAS_DIR}/oscal/${OSCAL_VERSION}/metaschema"

oscal_json_schemas=(
	oscal_complete_schema.json
	oscal_catalog_schema.json
	oscal_profile_schema.json
	oscal_component_schema.json
	oscal_ssp_schema.json
	oscal_assessment-plan_schema.json
	oscal_assessment-results_schema.json
	oscal_poam_schema.json
)
for schema in "${oscal_json_schemas[@]}"; do
	curl -fsSL -o "${SCHEMAS_DIR}/oscal/${OSCAL_VERSION}/json-schema/${schema}" \
		"${OSCAL_BASE_URL}/${schema}"
done

oscal_metaschemas=(
	oscal_catalog_metaschema_RESOLVED.xml
	oscal_profile_metaschema_RESOLVED.xml
	oscal_component_metaschema_RESOLVED.xml
	oscal_ssp_metaschema_RESOLVED.xml
)
for schema in "${oscal_metaschemas[@]}"; do
	curl -fsSL -o "${SCHEMAS_DIR}/oscal/${OSCAL_VERSION}/metaschema/${schema}" \
		"${OSCAL_BASE_URL}/${schema}"
done
echo "OSCAL schemas downloaded to ${SCHEMAS_DIR}/oscal/${OSCAL_VERSION}/"

echo "Fetching gemara CUE schemas version ${GEMARA_VERSION}..."
mkdir -p "${SCHEMAS_DIR}/gemara/${GEMARA_VERSION}"

gemara_schemas=(
	auditlog.cue
	capabilitycatalog.cue
	collections.cue
	controlcatalog.cue
	enforcementlog.cue
	entities.cue
	evaluationlog.cue
	guidancecatalog.cue
	lexicon.cue
	mapping_inline.cue
	mappingdocument.cue
	metadata.cue
	policy.cue
	principlecatalog.cue
	riskcatalog.cue
	threatcatalog.cue
	vectorcatalog.cue
)
for schema in "${gemara_schemas[@]}"; do
	curl -fsSL -o "${SCHEMAS_DIR}/gemara/${GEMARA_VERSION}/${schema}" \
		"${GEMARA_BASE_URL}/${schema}"
done
echo "Gemara schemas downloaded to ${SCHEMAS_DIR}/gemara/${GEMARA_VERSION}/"

# Verify all downloaded files exist and are non-empty.
echo ""
echo "Verifying downloads..."
fail=0
all_files=()
for schema in "${oscal_json_schemas[@]}"; do
	all_files+=("${SCHEMAS_DIR}/oscal/${OSCAL_VERSION}/json-schema/${schema}")
done
for schema in "${oscal_metaschemas[@]}"; do
	all_files+=("${SCHEMAS_DIR}/oscal/${OSCAL_VERSION}/metaschema/${schema}")
done
for schema in "${gemara_schemas[@]}"; do
	all_files+=("${SCHEMAS_DIR}/gemara/${GEMARA_VERSION}/${schema}")
done

for f in "${all_files[@]}"; do
	if [[ ! -s "$f" ]]; then
		echo "ERROR: missing or empty file: $f"
		fail=1
	fi
done
if [[ "$fail" -ne 0 ]]; then
	echo "Verification failed: one or more schema files are missing or empty."
	exit 1
fi
echo "All files present and non-empty."

# Print SHA-256 checksums for audit trail.
echo ""
echo "SHA-256 checksums:"
sha256sum "${all_files[@]}"

echo ""
echo "Schemas fetch complete."
