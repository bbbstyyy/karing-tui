#!/usr/bin/env bash
# 从参考目录同步地区分流预置文件到 internal/routing/preset/。
#
# 预置文件是 karing 的资源（assets/datas/preset/<region>.json），本项目按「同构」
# 方式嵌入：保留原始 name/outbound/switch/rule_set_build_in 结构，不手工抄写，
# 这样参考目录更新后重跑本脚本即可，diff 可读。做法与 internal/catalog/data/ 一致。
#
# 用法: scripts/sync-preset.sh [来源目录] [地区...]
#   来源目录缺省 karing/assets/datas/preset
#   地区缺省当前已嵌入的全部地区（从目标目录推断）
# 幂等：内容一致时不改动文件（不产生虚假 diff）。
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
source_dir="${1:-${repo_root}/karing/assets/datas/preset}"
target_dir="${repo_root}/internal/routing/preset"
shift || true

if [[ ! -d "${source_dir}" ]]; then
	echo "预置来源目录不存在: ${source_dir}" >&2
	echo "用法: scripts/sync-preset.sh [来源目录] [地区...]" >&2
	exit 1
fi

regions=("$@")
if [[ ${#regions[@]} -eq 0 ]]; then
	# macOS 自带 bash 3.2 没有 mapfile，用 while read 兼容
	while IFS= read -r region; do
		[[ -n "${region}" ]] && regions+=("${region}")
	done < <(find "${target_dir}" -maxdepth 1 -name '*.json' -exec basename {} .json \; | sort)
fi
if [[ ${#regions[@]} -eq 0 ]]; then
	echo "没有可同步的地区（目标目录 ${target_dir} 为空）" >&2
	exit 1
fi

mkdir -p "${target_dir}"
status=0
for region in "${regions[@]}"; do
	src="${source_dir}/${region}.json"
	dst="${target_dir}/${region}.json"
	if [[ ! -f "${src}" ]]; then
		echo "  !! 来源缺少 ${region}.json（跳过，保留现有文件）" >&2
		status=1
		continue
	fi
	if [[ -f "${dst}" ]] && cmp -s "${src}" "${dst}"; then
		echo "  ${region}.json 已是最新"
		continue
	fi
	cp "${src}" "${dst}"
	echo "  ${region}.json 已同步（$(wc -c < "${dst}" | tr -d ' ') 字节）"
done

echo "预置文件位于 ${target_dir}（随二进制嵌入；发布包体积随之线性增长，可接受）"
exit "$status"
