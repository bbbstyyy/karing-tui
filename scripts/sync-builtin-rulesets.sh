#!/usr/bin/env bash
# Synchronize compile-time .srs assets from the reference Karing checkout.
# The Go binary embeds these files; users can refresh them in a later release
# by running this script and rebuilding.  Missing catalog entries are left for
# the normal remote-update fallback.
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
source_root="${1:-${repo_root}/karing/assets/datas}"
target_root="${repo_root}/internal/catalog/data/rulesets"

if [[ ! -d "${source_root}" ]]; then
	echo "Rule-set source directory does not exist: ${source_root}" >&2
	echo "Usage: scripts/sync-builtin-rulesets.sh [path/to/karing/assets/datas]" >&2
	exit 1
fi

for kind in geosite geoip acl; do
	list="${repo_root}/internal/catalog/data/${kind}.txt"
	source_dir="${source_root}/${kind}"
	target_dir="${target_root}/${kind}"
	mkdir -p "${target_dir}"
	while IFS= read -r code; do
		case "${code}" in
			''|'#'*) continue ;;
		esac
		src="${source_dir}/${code}.srs"
		if [[ -f "${src}" ]]; then
			cp "${src}" "${target_dir}/${code}.srs"
		fi
	done < "${list}"
done

echo "Synchronized embedded rule-set assets under ${target_root}"
