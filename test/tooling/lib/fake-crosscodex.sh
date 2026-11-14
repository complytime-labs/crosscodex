#!/usr/bin/env bash
# Stand-in for the crosscodex CLI in the eval-crosswalk tooling suite. It
# records each call's argv to $FAKE_ARGV_LOG and replays the canned run in
# $FAKE_FIXTURES (mappings.json, candidates.json, status.json). $FAKE_JOB_STATUS
# overrides the job status and $FAKE_JOB_ERROR sets the job's error message.
set -euo pipefail

echo "$*" >>"${FAKE_ARGV_LOG:?}"
fixtures="${FAKE_FIXTURES:?}"

case "$*" in
*"catalog import"*)
	printf '{"catalog_id":"cat-%s","document_id":"doc"}\n' "$(wc -l <"${FAKE_ARGV_LOG}" | tr -d ' ')"
	;;
*"run start"*)
	printf '{"job_id":"job-synth","status":"JOB_STATUS_PENDING"}\n'
	;;
*"run status"*)
	jq --arg s "${FAKE_JOB_STATUS:-JOB_STATUS_COMPLETED}" --arg e "${FAKE_JOB_ERROR:-}" \
		'.status = $s | if $e != "" then .error = {message: $e} else .error = null end' \
		"${fixtures}/status.json"
	;;
*"results export"*) cat "${fixtures}/mappings.json" ;;
*"results candidates"*) cat "${fixtures}/candidates.json" ;;
*)
	echo "fake-crosscodex: unexpected call: $*" >&2
	exit 64
	;;
esac
