#!/usr/bin/env bash
set -euo pipefail

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
	curl -sSL -o "${SCHEMAS_DIR}/oscal/${OSCAL_VERSION}/json-schema/${schema}" \
		"${OSCAL_BASE_URL}/${schema}"
done

oscal_metaschemas=(
	oscal_catalog_metaschema_RESOLVED.xml
	oscal_profile_metaschema_RESOLVED.xml
	oscal_component_metaschema_RESOLVED.xml
	oscal_ssp_metaschema_RESOLVED.xml
)
for schema in "${oscal_metaschemas[@]}"; do
	curl -sSL -o "${SCHEMAS_DIR}/oscal/${OSCAL_VERSION}/metaschema/${schema}" \
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
	curl -sSL -o "${SCHEMAS_DIR}/gemara/${GEMARA_VERSION}/${schema}" \
		"${GEMARA_BASE_URL}/${schema}"
done
echo "Gemara schemas downloaded to ${SCHEMAS_DIR}/gemara/${GEMARA_VERSION}/"
echo "Schemas fetch complete."
