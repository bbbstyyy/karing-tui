#!/usr/bin/env bash
# 重新生成 internal/catalog/data/ 下的内置分类码清单。
#
# 分类码即上游仓库里 .srs 文件的文件名，故直接取仓库文件树而非手工维护清单——
# 这样码与下载 URL 必然对应。acl-ip.txt 则需要看规则集内容：ACL4SSR 有大量
# 名字不含 "ip" 却含 ip_cidr 的分类（ChinaDomain、Apple、Telegram…），仅凭命名
# 判断会漏判，进而让 4.1 的 resolve 规则插不进去、IP 条件对域名静默失效。
#
# 用法: scripts/gen-catalog.sh [sing-box 二进制路径]
# 访问 GitHub 需要代理时设置 https_proxy/http_proxy。
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
out_dir="$repo_root/internal/catalog/data"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

SINGBOX=${1:-$(command -v sing-box || true)}

META_REPO=https://github.com/MetaCubeX/meta-rules-dat.git   # geosite / geoip（上游权威）
KR_REPO=https://github.com/KaringX/karing-ruleset.git        # ACL4SSR（唯一 sing 格式源）

echo "==> 拉取仓库文件树"
git clone -q --filter=blob:none --no-checkout --depth=1 --branch sing "$META_REPO" "$work/meta"
git clone -q --filter=blob:none --no-checkout --depth=1 --branch sing "$KR_REPO" "$work/kr"
git -C "$work/meta" ls-tree -r --name-only HEAD > "$work/meta-files.txt"
git -C "$work/kr"   ls-tree -r --name-only HEAD > "$work/kr-files.txt"

emit() { # emit <清单文件> <输出名> <说明>
  { echo "# $3"; echo "# 由 scripts/gen-catalog.sh 生成，请勿手工编辑"; cat "$1"; } > "$out_dir/$2.txt"
  echo "    $2.txt: $(grep -vc '^#' "$out_dir/$2.txt") 个分类码"
}

echo "==> 生成分类码清单"
grep '^geo/geosite/.*\.srs$' "$work/meta-files.txt" | sed 's|^geo/geosite/||; s|\.srs$||' | sort > "$work/geosite.txt"
grep '^geo/geoip/.*\.srs$'   "$work/meta-files.txt" | sed 's|^geo/geoip/||;   s|\.srs$||' | sort > "$work/geoip.txt"
# ACL4SSR 的 Ruleset/ 子目录是顶层文件的副本，只取顶层
grep '^ACL4SSR/[^/]*\.srs$'  "$work/kr-files.txt"   | sed 's|^ACL4SSR/||;     s|\.srs$||' | sort > "$work/acl.txt"

emit "$work/geosite.txt" geosite "geosite 分类码（MetaCubeX/meta-rules-dat sing 分支 geo/geosite）"
emit "$work/geoip.txt"   geoip   "geoip 分类码（MetaCubeX/meta-rules-dat sing 分支 geo/geoip）"
emit "$work/acl.txt"     acl     "ACL4SSR 分类码（KaringX/karing-ruleset sing 分支 ACL4SSR 顶层）"

if [ -z "$SINGBOX" ] || [ ! -x "$SINGBOX" ]; then
  echo "==> 跳过 acl-ip.txt：未找到 sing-box 二进制（传参或放进 PATH）"
  echo "    注意 data/acl-ip.txt 未更新，新增的 IP 类 acl 分类不会触发 resolve"
  exit 0
fi

echo "==> 判定 acl 分类的 IP 属性（sing-box rule-set decompile）"
git -C "$work/kr" sparse-checkout init --cone >/dev/null 2>&1
git -C "$work/kr" sparse-checkout set ACL4SSR >/dev/null 2>&1
git -C "$work/kr" checkout -q HEAD

: > "$work/acl-ip.txt"
fail=0
while read -r code; do
  srs="$work/kr/ACL4SSR/$code.srs"
  json="$work/dec-$code.json"
  if ! "$SINGBOX" rule-set decompile --output "$json" "$srs" >/dev/null 2>&1; then
    echo "    !! 解析失败: $code" >&2
    fail=$((fail+1))
    continue
  fi
  if python3 -c "
import json,sys
ks=set()
for r in json.load(open('$json')).get('rules',[]):
    ks.update(r.keys())
sys.exit(0 if ({'ip_cidr','ip_is_private'} & ks) else 1)
"; then
    echo "$code" >> "$work/acl-ip.txt"
  fi
  rm -f "$json"
done < "$work/acl.txt"

{
  echo "# 含 IP 类条件（ip_cidr/ip_is_private）的 ACL4SSR 分类码"
  echo "# 由 scripts/gen-catalog.sh 经 sing-box rule-set decompile 逐个判定生成，请勿手工编辑"
  echo "# geosite 全量实测均不含 IP 条件；geoip 一律含 IP 条件——两者无需清单"
  cat "$work/acl-ip.txt"
} > "$out_dir/acl-ip.txt"
echo "    acl-ip.txt: $(grep -vc '^#' "$out_dir/acl-ip.txt") 个 IP 类分类（解析失败 $fail 个）"

echo "==> 完成。请运行 go test ./internal/catalog/ 并同步其中的数量断言。"
