#!/usr/bin/env bash
# Drive one crosswalk quality evaluation (#260) against a running gateway:
# subset the profile's source catalog, import it and the target, run the
# analysis, then export the results and score them with crosswalk-eval.
# Run it through `task eval:crosswalk`, which builds both binaries and sets the
# environment. Direct runs must set every variable marked required.
#
# Required: ROOT_DIR PROFILE CROSSCODEX_BIN EVAL_BIN EVAL_ENDPOINT EVAL_CA
#           EVAL_CERT EVAL_KEY
# Optional: LABEL (default: UTC timestamp), BASELINE (report.json to compare),
#           MODE=llm (print only the summary line), NO_CACHE=1 (run start
#           --no-cache), EVAL_TIMEOUT (seconds, default 3600), EVAL_POLL
#           (seconds, default 10), PROFILES_DIR, LABELS_DIR and EVAL_OUT_DIR (defaults
#           under ROOT_DIR; the tooling suite overrides them). A score uses
#           LABELS_DIR/PROFILE.yaml when it exists; without it no labels apply.
#
# PROFILE and LABEL become path components, so both are checked against a
# strict pattern before use. Catalog text is only ever passed to commands as
# quoted arguments or read by jq/crosswalk-eval; nothing reaches eval.
set -euo pipefail

: "${ROOT_DIR:?ROOT_DIR is required}"
: "${PROFILE:?PROFILE is required: the profile name, e.g. ssdf-to-sp800-53r5}"
: "${CROSSCODEX_BIN:?CROSSCODEX_BIN is required}"
: "${EVAL_BIN:?EVAL_BIN is required}"
: "${EVAL_ENDPOINT:?EVAL_ENDPOINT is required}"
: "${EVAL_CA:?EVAL_CA is required}"
: "${EVAL_CERT:?EVAL_CERT is required}"
: "${EVAL_KEY:?EVAL_KEY is required}"
LABEL="${LABEL:-}"
BASELINE="${BASELINE:-}"
MODE="${MODE:-}"
NO_CACHE="${NO_CACHE:-}"
EVAL_TIMEOUT="${EVAL_TIMEOUT:-3600}"
EVAL_POLL="${EVAL_POLL:-10}"
PROFILES_DIR="${PROFILES_DIR:-${ROOT_DIR}/test/eval/crosswalk/profiles}"
LABELS_DIR="${LABELS_DIR:-${ROOT_DIR}/test/eval/crosswalk/labels}"
EVAL_OUT_DIR="${EVAL_OUT_DIR:-${ROOT_DIR}/.test-output/eval/crosswalk}"

fail() {
	echo "ERROR: $*" >&2
	exit 1
}

# Progress goes to stderr and is silenced in llm mode, which prints only the
# summary line (and errors).
progress() {
	if [ "${MODE}" != "llm" ]; then
		echo "$*" >&2
	fi
}

[[ "${PROFILE}" =~ ^[a-z0-9][a-z0-9.-]*$ ]] ||
	fail "invalid PROFILE '${PROFILE}': use lowercase letters, digits, '.' and '-' (e.g. ssdf-to-sp800-53r5)."
[[ "${EVAL_TIMEOUT}" =~ ^[0-9]+$ && "${EVAL_POLL}" =~ ^[1-9][0-9]*$ ]] ||
	fail "EVAL_TIMEOUT must be whole seconds and EVAL_POLL a positive number of seconds."

profile_file="${PROFILES_DIR}/${PROFILE}.yaml"
if [ ! -f "${profile_file}" ]; then
	available="$(cd "${PROFILES_DIR}" && for f in *.yaml; do echo "  ${f%.yaml}"; done)"
	fail "unknown profile '${PROFILE}'. Available profiles:
${available}"
fi

if [ -z "${LABEL}" ]; then
	LABEL="$(date -u +%Y%m%dT%H%M%SZ)"
fi
[[ "${LABEL}" =~ ^[A-Za-z0-9._-]+$ ]] ||
	fail "invalid LABEL '${LABEL}': use letters, digits, '.', '_' and '-' only."

# Labels are optional, but a labels path that exists and cannot be read must
# stop the run now, before the expensive job, not silently score without them.
labels_file=""
if [ -e "${LABELS_DIR}/${PROFILE}.yaml" ] || [ -L "${LABELS_DIR}/${PROFILE}.yaml" ]; then
	[ -f "${LABELS_DIR}/${PROFILE}.yaml" ] && [ -r "${LABELS_DIR}/${PROFILE}.yaml" ] ||
		fail "labels path ${LABELS_DIR}/${PROFILE}.yaml exists but is not a readable regular file (a directory or dangling symlink?). Replace it with the labels YAML, or remove it to score without labels."
	labels_file="${LABELS_DIR}/${PROFILE}.yaml"
fi

run_dir="${EVAL_OUT_DIR}/${PROFILE}/${LABEL}"
if [ -e "${run_dir}" ]; then
	fail "${run_dir} already exists; it holds an earlier run and is never overwritten. Choose another LABEL."
fi
# mkdir without -p on the final directory is atomic, so a concurrent run with
# the same label loses here instead of sharing the directory.
mkdir -p "${EVAL_OUT_DIR}/${PROFILE}"
mkdir "${run_dir}" || fail "cannot create ${run_dir}. Choose another LABEL."

crosscodex() {
	"${CROSSCODEX_BIN}" \
		--endpoint "${EVAL_ENDPOINT}" \
		--tls-ca "${EVAL_CA}" \
		--tls-cert "${EVAL_CERT}" \
		--tls-key "${EVAL_KEY}" \
		"$@"
}

# import_catalog prints the catalog ID the gateway assigned to a file.
import_catalog() {
	local id
	id="$(crosscodex --json catalog import "$1" | jq -er '.catalog_id')" ||
		fail "importing $1 failed or returned no catalog_id; see the CLI error above."
	echo "${id}"
}

progress "Building the subset for profile ${PROFILE} ..."
# subset prints the absolute path of the profile's target catalog, the file it
# validated and hashed; import exactly that file rather than re-parsing the
# profile here.
target_path="$("${EVAL_BIN}" subset --root "${ROOT_DIR}" --profile "${profile_file}" --out "${run_dir}/subset.json")" ||
	fail "building the subset failed; see the error above."
[ -n "${target_path}" ] || fail "crosswalk-eval subset printed no target catalog path."

progress "Importing the subset and the target catalog ..."
source_id="$(import_catalog "${run_dir}/subset.json")"
target_id="$(import_catalog "${target_path}")"

start_args=(run start --source "${source_id}" --target "${target_id}")
if [ "${NO_CACHE}" = "1" ]; then
	start_args+=(--no-cache)
fi
job_id="$(crosscodex "${start_args[@]}" --json | jq -er '.job_id')" ||
	fail "run start failed or returned no job_id; see the CLI error above."
progress "Job ${job_id} started (source ${source_id}, target ${target_id}); polling every ${EVAL_POLL}s, timeout ${EVAL_TIMEOUT}s ..."

# Poll until the job is terminal. A failed status call is reported and retried
# until the timeout, so a gateway restart mid-run does not discard the job.
started="$(date +%s)"
status_file="${run_dir}/status.json"
while true; do
	if crosscodex run status "${job_id}" --json >"${status_file}.tmp"; then
		mv "${status_file}.tmp" "${status_file}"
		status="$(jq -r '.status' "${status_file}")"
		case "${status}" in
		JOB_STATUS_COMPLETED) break ;;
		JOB_STATUS_FAILED | JOB_STATUS_CANCELLED)
			echo "Job ${job_id} ended ${status}: $(jq -r '.error.message // "no error message reported"' "${status_file}")" >&2
			fail "no score was produced; run state is in ${run_dir}."
			;;
		esac
		progress "  job ${status} ..."
	else
		rm -f "${status_file}.tmp"
		echo "WARNING: run status ${job_id} failed; retrying." >&2
	fi
	if [ $(($(date +%s) - started)) -ge "${EVAL_TIMEOUT}" ]; then
		fail "timed out after ${EVAL_TIMEOUT}s waiting for job ${job_id}; it may still be running. Raise EVAL_TIMEOUT or check 'crosscodex run status ${job_id}'."
	fi
	sleep "${EVAL_POLL}"
done

progress "Exporting results ..."
crosscodex results export "${job_id}" --stage analyzer --format json >"${run_dir}/mappings.json"
crosscodex results candidates "${job_id}" --format json >"${run_dir}/candidates.json"

score_args=(score --root "${ROOT_DIR}" --profile "${profile_file}"
	--subset "${run_dir}/subset.json" --mappings "${run_dir}/mappings.json"
	--candidates "${run_dir}/candidates.json" --status "${status_file}"
	--label "${LABEL}" --out "${run_dir}")
if [ -n "${BASELINE}" ]; then
	score_args+=(--baseline "${BASELINE}")
fi
if [ -n "${labels_file}" ]; then
	score_args+=(--labels "${labels_file}")
fi
"${EVAL_BIN}" "${score_args[@]}"

if [ "${MODE}" != "llm" ]; then
	cat "${run_dir}/report.md"
	echo "Report: ${run_dir}/report.md and report.json"
fi
