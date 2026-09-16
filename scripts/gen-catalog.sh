#!/usr/bin/env bash
# 重新生成 / 校验 internal/catalog/data/ 下的内置分类码清单。
#
# 事实源是**内嵌快照** internal/catalog/data/rulesets/<kind>/*.srs —— 它就是发布物本身
# （编译期 go:embed 进二进制、离线可用、与 karing 的规则包一致）。码表若与它不同源，
# 就会出现「.srs 已随二进制发布、却因为码表没收录而无法引用」的悬空分类：
# catalog.Parse 判定为未知码，rule_set 规则在写入时即被 checkRuleSetTags 拒绝。
#
# 上游仓库（meta-rules-dat / karing-ruleset）只用于发现**无内嵌 .srs 的候选码**：
# 这类码仍可引用，只是首次生成时要联网下载，脚本把它们记为「需下载」候选。
#
# 用法:
#   scripts/gen-catalog.sh                       # 按内嵌快照重新生成码表 + 重判 acl 的 IP 属性
#   scripts/gen-catalog.sh --check               # 只校验不写入（提交前自查）
#   scripts/gen-catalog.sh --ip-all              # 额外扫描 geosite/geoip 的 IP 属性（慢，约 2400 次反编译）
#   scripts/gen-catalog.sh <sing-box 路径>        # 显式指定 sing-box（缺省取 PATH）
#
# 退出码: 0 成功；1 校验失败（快照有码表缺 = 悬空分类，必须修）；2 用法错误。
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
out_dir="$repo_root/internal/catalog/data"
snapshot_root="$out_dir/rulesets"

check_only=0
ip_all=0
singbox=""
for arg in "$@"; do
	case "$arg" in
		--check) check_only=1 ;;
		--ip-all) ip_all=1 ;;
		-h|--help) sed -n '2,22p' "${BASH_SOURCE[0]}"; exit 0 ;;
		-*) echo "未知参数 $arg" >&2; exit 2 ;;
		*) singbox="$arg" ;;
	esac
done
if [[ -z "$singbox" ]]; then
	singbox=$(command -v sing-box || true)
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

kinds=(geosite geoip acl)
status=0

echo "==> 以内嵌快照为准核对码表"
for kind in "${kinds[@]}"; do
	snap_dir="$snapshot_root/$kind"
	if [[ ! -d "$snap_dir" ]]; then
		echo "    !! 快照目录不存在: $snap_dir" >&2
		status=1
		continue
	fi
	# 快照码：文件名即分类码
	find "$snap_dir" -maxdepth 1 -name '*.srs' -exec basename {} .srs \; | sort > "$work/snap-$kind.txt"
	grep -v '^#' "$out_dir/$kind.txt" 2>/dev/null | grep -v '^[[:space:]]*$' | sort -u > "$work/code-$kind.txt" || true

	snap_n=$(wc -l < "$work/snap-$kind.txt" | tr -d ' ')
	missing=$(comm -23 "$work/snap-$kind.txt" "$work/code-$kind.txt")
	extra=$(comm -13 "$work/snap-$kind.txt" "$work/code-$kind.txt")
	extra_n=0
	[[ -n "$extra" ]] && extra_n=$(wc -l <<< "$extra" | tr -d ' ')

	if [[ -n "$missing" ]]; then
		miss_n=$(wc -l <<< "$missing" | tr -d ' ')
		echo "    ${kind}: 快照 ${snap_n} 个码，其中 ${miss_n} 个**码表缺失**（悬空分类：.srs 已随二进制发布却无法引用）"
		echo "$missing" | sed 's/^/        - /'
		status=1
	fi

	if [[ "$check_only" == "1" ]]; then
		[[ -z "$missing" ]] && echo "    ${kind}: 快照 ${snap_n} 个码，码表已全部收录（另有无内嵌资产的候选码 ${extra_n} 个）"
		continue
	fi

	# 生成：快照码 ∪ 现有码表（保留无内嵌资产的上游候选码，不清空既有可用引用）。
	# 先写临时文件再原子替换：中途失败不会把码表截断成空表。
	{
		echo "# ${kind} 分类码（内嵌快照 internal/catalog/data/rulesets/${kind}，来源 Karing 规则集包）"
		echo "# 由 scripts/gen-catalog.sh 生成，请勿手工编辑"
		if [[ -n "$extra" ]]; then
			echo "# 快照之外的 ${extra_n} 个码（上游仍提供、无内嵌 .srs，引用时按需下载）: $(tr '\n' ' ' <<< "$extra")"
		fi
		cat "$work/snap-$kind.txt" "$work/code-$kind.txt" | sort -u
	} > "$work/$kind.txt"
	mv "$work/$kind.txt" "$out_dir/$kind.txt"
	total=$(grep -vc '^#' "$out_dir/$kind.txt")
	echo "    ${kind}: 快照 ${snap_n} + 需下载候选 ${extra_n} = ${total} 个码"
done

if [[ ! -x "$singbox" ]]; then
	echo "==> 跳过 IP 类判定：未找到 sing-box 二进制（传参或放进 PATH）"
	echo "    未重新判定 data/acl-ip.txt；新增的 IP 类分类不会触发 resolve"
	echo "    如需重跑: scripts/gen-catalog.sh /path/to/sing-box"
	exit "$status"
fi

# IP 属性判定：ACL4SSR 有大量名字不含 "ip" 却含 ip_cidr 的分类（ChinaDomain、Apple、
# Telegram…），仅凭命名判断会漏判，进而让 resolve 规则插不进去、IP 条件对域名静默失效。
# 判定对象是内嵌快照（发布物本身），而非上游仓库当前状态。
ip_kinds=(acl)
[[ "$ip_all" == "1" ]] && ip_kinds=(geosite geoip acl)

echo "==> 用 $singbox 逐个反编译内嵌 .srs，判定是否含 IP 类条件（${ip_kinds[*]}）"
: > "$work/acl-new.txt"
geosite_all=0; geosite_ip=0
geoip_all=0;   geoip_ip=0
acl_all=0;     acl_ip=0
fail=0
is_ip() { # is_ip <json 文件>
	python3 -c "
import json,sys
ks=set()
def walk(rules):
    for r in rules:
        if r.get('type') == 'logical':
            walk(r.get('rules', []))
        else:
            ks.update(k for k in r.keys() if k != 'invert')
walk(json.load(open(sys.argv[1])).get('rules',[]))
sys.exit(0 if ({'ip_cidr','ip_is_private'} & ks) else 1)
" "$1"
}
for kind in "${ip_kinds[@]}"; do
	while IFS= read -r code; do
		eval "${kind}_all=\$(( ${kind}_all + 1 ))"
		json="$work/one.json"
		if ! "$singbox" rule-set decompile --output "$json" "$snapshot_root/$kind/$code.srs" >/dev/null 2>&1; then
			echo "    !! 反编译失败: ${kind}:${code}" >&2
			fail=$((fail+1))
			rm -f "$json"
			continue
		fi
		if is_ip "$json"; then
			eval "${kind}_ip=\$(( ${kind}_ip + 1 ))"
			[[ "$kind" == "acl" ]] && echo "$code" >> "$work/acl-new.txt"
		fi
		rm -f "$json"
	done < "$work/snap-$kind.txt"
done

report_ip_counts() {
	echo "    acl:     ${acl_all} 个分类，含 IP 条件 ${acl_ip} 个（反编译失败 ${fail} 个）"
	if [[ "$ip_all" == "1" ]]; then
		echo "    geosite: ${geosite_all} 个分类，含 IP 条件 ${geosite_ip} 个"
		echo "    geoip:   ${geoip_all} 个分类，含 IP 条件 ${geoip_ip} 个"
	fi
	return 0
}

if [[ "$check_only" == "1" ]]; then
	report_ip_counts
	exit "$status"
fi

{
	echo "# 含 IP 类条件（ip_cidr/ip_is_private）的 ACL4SSR 分类码"
	echo "# 由 scripts/gen-catalog.sh 用 sing-box rule-set decompile 逐个判定生成，请勿手工编辑"
	echo "# 判定对象是**内嵌快照**（发布物本身），而非上游仓库当前状态"
	echo "# geosite 全量实测均不含 IP 条件；geoip 一律含 IP 条件——两者无需清单"
	cat "$work/acl-new.txt"
} > "$work/acl-ip.new"
mv "$work/acl-ip.new" "$out_dir/acl-ip.txt"
echo "    acl-ip.txt: $(grep -vc '^#' "$out_dir/acl-ip.txt") 个 IP 类分类"
report_ip_counts

echo "==> 完成。请运行 go test ./internal/catalog/ ./internal/routing/ 并同步其中的数量断言。"
exit "$status"
