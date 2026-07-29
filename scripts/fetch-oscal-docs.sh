#!/usr/bin/env bash
# fetch-oscal-docs.sh — download pinned OSCAL catalog documents.
#
# Usage: fetch-oscal-docs.sh [oscal-sources.yaml]
#
# Reads a YAML config listing OSCAL catalog documents to fetch.
# Existing files with valid OSCAL structure (top-level "catalog" key)
# are skipped, making the script safe to re-run.
set -euo pipefail

config_file="${1:-catalogs/oscal-sources.yaml}"
catalogs_dir="$(dirname "$config_file")"

if [[ ! -f "$config_file" ]]; then
	echo "ERROR: config file not found: $config_file" >&2
	exit 1
fi

# Parse the YAML config.  Prefer Python+PyYAML (available in most
# environments) but fall back to a line-oriented parser if pyyaml
# is missing.
parse_config() {
	if python3 -c "import yaml" 2>/dev/null; then
		python3 -c "
import yaml, json, sys
with open(sys.argv[1]) as f:
    cfg = yaml.safe_load(f)
print(cfg.get('base_url', ''))
for doc in cfg.get('documents', []):
    print(json.dumps(doc))
" "$config_file"
	else
		# Fallback: simple line parser for the flat YAML structure.
		local base_url=""
		local path="" local_name=""
		base_url=$(grep '^base_url:' "$config_file" | sed 's/^base_url:[[:space:]]*//')
		echo "$base_url"
		while IFS= read -r line; do
			case "$line" in
			*"path:"*)
				path="${line##*path: }"
				;;
			*"local:"*)
				local_name="${line##*local: }"
				if [[ -n "$path" && -n "$local_name" ]]; then
					printf '{"path":"%s","local":"%s"}\n' "$path" "$local_name"
				fi
				path=""
				local_name=""
				;;
			*)
				;;
			esac
		done <"$config_file"
	fi
}

# Validate that a file is non-empty JSON with a top-level "catalog" key.
is_valid_catalog() {
	local file="$1"
	[[ -s "$file" ]] && python3 -c "
import json, sys
with open(sys.argv[1]) as f:
    data = json.load(f)
sys.exit(0 if 'catalog' in data else 1)
" "$file" 2>/dev/null
}

downloaded=0
skipped=0
failed=0

# First line of output is base_url, remaining lines are JSON document entries.
config_output=$(parse_config)
mapfile -t lines <<<"$config_output"
base_url="${lines[0]}"

if [[ -z "$base_url" ]]; then
	echo "ERROR: base_url not found in $config_file" >&2
	exit 1
fi

for ((i = 1; i < ${#lines[@]}; i++)); do
	entry="${lines[$i]}"
	doc_path=$(echo "$entry" | python3 -c "import json,sys; print(json.load(sys.stdin)['path'])")
	local_name=$(echo "$entry" | python3 -c "import json,sys; print(json.load(sys.stdin)['local'])")
	dest="$catalogs_dir/$local_name"

	set +e
	is_valid_catalog "$dest"
	_valid=$?
	set -e
	if [[ "$_valid" -eq 0 ]]; then
		echo "SKIP  $local_name (already exists and valid)"
		skipped=$((skipped + 1))
		continue
	fi

	url="$base_url/$doc_path"
	echo "FETCH $local_name"
	echo "      <- $url"

	tmp="$(mktemp)"
	curl -fsSL --retry 3 --retry-delay 2 -o "$tmp" "$url" || {
		echo "ERROR failed to download $url" >&2
		rm -f "$tmp"
		failed=$((failed + 1))
		continue
	}

	set +e
	is_valid_catalog "$tmp"
	_valid=$?
	set -e
	if [[ "$_valid" -ne 0 ]]; then
		echo "ERROR downloaded file is not a valid OSCAL catalog: $local_name" >&2
		rm -f "$tmp"
		failed=$((failed + 1))
		continue
	fi

	mv "$tmp" "$dest"
	downloaded=$((downloaded + 1))
done

echo ""
echo "Done: $downloaded downloaded, $skipped skipped, $failed failed."
[[ "$failed" -eq 0 ]]
