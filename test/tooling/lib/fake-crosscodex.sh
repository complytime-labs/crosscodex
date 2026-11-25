#!/usr/bin/env bash
# Stand-in for the crosscodex CLI in the eval-crosswalk tooling suite. It
# records each call's argv to $FAKE_ARGV_LOG and replays the canned run in
# $FAKE_FIXTURES (mappings.json, candidates.json, status.json). $FAKE_JOB_STATUS
# overrides the job status, $FAKE_JOB_ERROR sets the job's error message and
# $FAKE_STATUS_JQ is a jq filter applied last to the run status JSON. With
# $FAKE_RUNNING_POLLS=N the first N status calls report a running job whose
# status has no "analysis" yet, unless $FAKE_RUNNING_KEEPS_ANALYSIS=1 keeps it.
# catalog inspect reports as content_hash the
# SHA-256 of $FAKE_SOURCE_FILE for cat-src and of $FAKE_TARGET_FILE for any
# other ID, or "" when that file does not exist. catalog controls prints the
# dump of a catalog with one control, as `catalog controls --json` does.
set -euo pipefail

echo "$*" >>"${FAKE_ARGV_LOG:?}"
fixtures="${FAKE_FIXTURES:?}"

case "$*" in
*"catalog import"*)
	printf '{"catalog_id":"cat-%s","document_id":"doc"}\n' "$(wc -l <"${FAKE_ARGV_LOG}" | tr -d ' ')"
	;;
*"catalog inspect"*)
	file="${FAKE_TARGET_FILE:-}"
	if [ "${!#}" = "cat-src" ]; then
		file="${FAKE_SOURCE_FILE:-}"
	fi
	hash=""
	if [ -f "${file}" ]; then
		hash="$(sha256sum "${file}" | cut -d' ' -f1)"
	fi
	jq -n --arg id "${!#}" --arg h "${hash}" '{catalog_id: $id, content_hash: $h}'
	;;
*"catalog controls"*)
	jq -n --arg id "${!#}" '{catalog_id: $id, controls: [{control_id: "c-1", title: "T", statement: "S"}]}'
	;;
*"run start"*)
	printf '{"job_id":"job-synth","status":"JOB_STATUS_PENDING"}\n'
	;;
*"run status"*)
	if [ "$(grep -c 'run status' "${FAKE_ARGV_LOG}")" -le "${FAKE_RUNNING_POLLS:-0}" ]; then
		if [ "${FAKE_RUNNING_KEEPS_ANALYSIS:-0}" = "1" ]; then
			jq '.status = "JOB_STATUS_RUNNING"' "${fixtures}/status.json"
		else
			jq '.status = "JOB_STATUS_RUNNING" | del(.analysis)' "${fixtures}/status.json"
		fi
		exit 0
	fi
	jq --arg s "${FAKE_JOB_STATUS:-JOB_STATUS_COMPLETED}" --arg e "${FAKE_JOB_ERROR:-}" \
		'.status = $s | if $e != "" then .error = {message: $e} else .error = null end | '"${FAKE_STATUS_JQ:-.}" \
		"${fixtures}/status.json"
	;;
*"results export"*) cat "${fixtures}/mappings.json" ;;
*"results candidates"*) cat "${fixtures}/candidates.json" ;;
*)
	echo "fake-crosscodex: unexpected call: $*" >&2
	exit 64
	;;
esac
