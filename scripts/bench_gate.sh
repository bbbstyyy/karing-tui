#!/usr/bin/env bash
# bench_gate.sh — C-BENCH-CI 建议 4：按 allocs/op 与 B/op 的相对退化做硬门禁。
# （背景见 CHECKLIST-v4.md 的 C-BENCH-CI 条目与 §7 进展记录）
#
# 为什么只拦 allocs/B 不拦时间：sec/op 跨 runner 绝对值抖动大，只能观察；
# 而 allocs/op 与 B/op 由基准代码路径唯一决定，同一 runner、同一 Go 版本下
# base→head 的差异即真实差异，不会自己漂移，适合做门禁。
# 特别地，base=0 且 head>0（0 分配路径新增分配）一律视为退化：本仓库的
# 热路径 0-alloc 成果（C5/C6/C8/C10/C11/C19）靠它守住。
#
# 用法:
#   bench_gate.sh <base.txt> <head.txt>
#   两份文件都是 `go test -bench=. -benchmem` 的原始输出（CI 用 -count=6）。
#   base 文件不存在（如首次推送没有 base 提交）时只打印 head 概要并放行。
#
# 判定（同名基准，各取 count 轮的中位数）:
#   allocs/op、B/op:  base=0 且 head>0            → FAIL
#                     base>0 且 head > base*(1+T) → FAIL（超过阈值才拦）
#   ns/op:            退化超过 T 只打 WARN（时间抖动大，不拦截）
#   仅 base / 仅 head: WARN（基准被改名/删除/新增，不拦截）
#
# 完整性判据只认内存指标（nmem = allocs/op 或 B/op 存在可比值的基准数）:
# 若误删 -benchmem，allocs/op 与 B/op 全为 n/a 而 ns/op 仍有值，旧判据会把
# 它算成「有可比数据」而放行。因此 nmem == 0 一律判完整性失败（exit 2）。
# ns/op 只用于观察，**不能**用来证明 -benchmem 已启用。
#
# 环境变量:
#   BENCH_GATE_THRESHOLD  退化阈值百分比（默认 10）
#   BENCH_GATE=off        观察模式：照常输出报告，但恒放行
#
# 退出码: 0 通过/放行；1 有退化被拦截；2 用法或完整性错误（如基准运行 FAIL）

set -uo pipefail

die() { printf 'bench_gate: %s\n' "$1" >&2; exit 2; }
usage() { printf '用法: bench_gate.sh <base.txt> <head.txt>\n' >&2; exit 2; }

[ $# -eq 2 ] || usage
base_file=$1
head_file=$2
[ -f "$head_file" ] || die "head 基准输出不存在: $head_file"

threshold="${BENCH_GATE_THRESHOLD:-10}"
case "$threshold" in
  '' | *[!0-9]*) die "BENCH_GATE_THRESHOLD 必须是非负整数百分比，得到: $threshold" ;;
esac
mode="${BENCH_GATE:-on}"
if [ "$mode" != "on" ] && [ "$mode" != "off" ]; then
  die "BENCH_GATE 只能是 on 或 off，得到: $mode"
fi

if grep -qE -- '--- FAIL' "$head_file"; then
  die "head 基准运行里出现 FAIL，先修失败项，门禁数据不可信"
fi

tmpdir=$(mktemp -d) || die "mktemp 失败"
trap 'rm -rf "$tmpdir"' EXIT

# ── 解析：go test -bench 原始输出 → TSV(name, allocs, bytes, ns) ─────────────
# 每个指标取全部轮次的中位数；基准名剥掉 GOMAXPROCS 后缀（-8）；
# KB/MB 与 us/ms/s 归一化为 B 与 ns。缺 -benchmem 时对应列为空。
parse_bench() { # $1=输入文件 $2=输出TSV
  awk '
    function med(A, name, cnt,   k, arr, j, tmp, m) {
      for (k = 1; k <= cnt; k++) arr[k] = A[name, k]
      for (j = 2; j <= cnt; j++) {
        tmp = arr[j]
        for (k = j - 1; k >= 1 && arr[k] > tmp; k--) arr[k + 1] = arr[k]
        arr[k + 1] = tmp
      }
      return arr[int((cnt + 1) / 2)]
    }
    $1 !~ /^Benchmark/ { next }
    {
      name = $1
      sub(/-[0-9]+$/, "", name)
      seen[name] = 1
      for (i = 2; i < NF; i++) {
        unit = $(i + 1); val = $i + 0
        if      (unit == "allocs/op")                             { ai[name]++; av[name, ai[name]] = val }
        else if (unit == "B/op")                                  { bi[name]++; bv[name, bi[name]] = val }
        else if (unit == "KB/op")                                 { bi[name]++; bv[name, bi[name]] = val * 1024 }
        else if (unit == "MB/op")                                 { bi[name]++; bv[name, bi[name]] = val * 1048576 }
        else if (unit == "ns/op")                                 { ti[name]++; tv[name, ti[name]] = val }
        else if (unit == "us/op" || unit == "µs/op")              { ti[name]++; tv[name, ti[name]] = val * 1000 }
        else if (unit == "ms/op")                                 { ti[name]++; tv[name, ti[name]] = val * 1000000 }
        else if (unit == "s/op")                                  { ti[name]++; tv[name, ti[name]] = val * 1000000000 }
      }
    }
    END {
      for (name in seen) {
        a = (name in ai) ? med(av, name, ai[name]) : ""
        b = (name in bi) ? med(bv, name, bi[name]) : ""
        t = (name in ti) ? med(tv, name, ti[name]) : ""
        printf "%s\t%s\t%s\t%s\n", name, a, b, t
      }
    }
  ' "$1" > "$2"
}

parse_bench "$head_file" "$tmpdir/head.tsv"
[ -s "$tmpdir/head.tsv" ] || die "head 输出里没有解析到任何基准行"

if [ ! -f "$base_file" ]; then
  echo "bench_gate: 无 base 基准输出（$base_file 不存在），仅记录 head，不做对比"
  awk -F'\t' '{
    printf "  %-42s allocs=%s  B/op=%s  ns=%s\n", $1, \
      ($2 == "" ? "n/a" : sprintf("%.0f", $2)), \
      ($3 == "" ? "n/a" : sprintf("%.0f", $3)), \
      ($4 == "" ? "n/a" : sprintf("%.0f", $4))
  }' "$tmpdir/head.tsv"
  exit 0
fi

parse_bench "$base_file" "$tmpdir/base.tsv"
[ -s "$tmpdir/base.tsv" ] || die "base 输出里没有解析到任何基准行"

# ── 对比与判定 ────────────────────────────────────────────────────────────────
# fmtcmp 返回单元格文本，并把判定写到全局 g_flag（kind 指定该指标超阈值时算
# "FAIL" 还是 "WARN"——allocs/B 拦截，时间只告警）。
gate_rc=0
awk -F'\t' -v threshold="$threshold" -v mode="$mode" '
  function fmtcmp(b, h, kind,   res) {
    g_flag = ""
    if (b == "" || h == "") return "n/a"
    b += 0; h += 0
    if (h > b) {
      if (b == 0) { g_flag = kind; return sprintf("%.0f → %.0f（base 为 0）", b, h) }
      res = sprintf("%.0f → %.0f (+%.1f%%)", b, h, (h / b - 1) * 100)
      if (h > b * (1 + threshold / 100)) g_flag = kind
      return res
    }
    if (h < b) return sprintf("%.0f → %.0f (-%.1f%%)", b, h, (1 - h / b) * 100)
    return sprintf("%.0f 持平", h)
  }
  NR == FNR { bhave[$1] = 1; ba[$1] = $2; bb[$1] = $3; bt[$1] = $4; next }
  { hhave[$1] = 1; ha[$1] = $2; hb[$1] = $3; ht[$1] = $4 }
  END {
    n = 0
    for (name in hhave) { all[name] = 1; ord[++n] = name }
    for (name in bhave) if (!(name in all)) { all[name] = 1; ord[++n] = name }
    for (i = 2; i <= n; i++) {
      tmp = ord[i]
      for (j = i - 1; j >= 1 && ord[j] > tmp; j--) ord[j + 1] = ord[j]
      ord[j + 1] = tmp
    }
    printf "bench_gate：门禁 allocs/op 与 B/op（阈值 %s%%，超阈值即拦），模式=%s\n", threshold, mode
    printf "%-42s %-22s %-22s %-24s %s\n", "benchmark", "allocs/op", "B/op", "ns/op（不拦截）", "判定"
    nfail = 0; nwarn = 0; nok = 0; nmem = 0
    for (k = 1; k <= n; k++) {
      name = ord[k]
      inb = (name in bhave); inh = (name in hhave)
      if (inb && !inh) {
        nwarn++
        printf "%-42s %-22s %-22s %-24s %s\n", name, "仅 base 有", "-", "-", "WARN：基准被改名或删除？"
        continue
      }
      if (!inb && inh) {
        nwarn++
        printf "%-42s %-22s %-22s %-24s %s\n", name, "新增", "-", "-", "WARN：无 base 可比"
        continue
      }
      cella = fmtcmp(ba[name], ha[name], "FAIL"); fa = (g_flag == "FAIL")
      cellb = fmtcmp(bb[name], hb[name], "FAIL"); fb = (g_flag == "FAIL")
      cellt = fmtcmp(bt[name], ht[name], "WARN"); wt = (g_flag == "WARN")
      # 只按内存指标统计可比对数：ns/op 有值不能证明 -benchmem 已启用。
      if (cella != "n/a" || cellb != "n/a") nmem++
      if (fa || fb)      { nfail++; verdict = "FAIL" }
      else if (wt)       { nwarn++; verdict = "WARN(时间)" }
      else               { nok++;   verdict = "OK" }
      printf "%-42s %-22s %-22s %-24s %s\n", name, cella, cellb, cellt, verdict
    }
    printf "\n"
    if (nmem == 0) {
      print "错误：base/head 之间没有任何可对比的 allocs/op 或 B/op 数据（-benchmem 未开？）" > "/dev/stderr"
      exit 3
    }
    if (nfail > 0 && mode == "on") {
      printf "结论：拦截——%d 个基准的 allocs/op 或 B/op 退化超过 %s%%\n", nfail, threshold
      exit 1
    }
    if (nfail > 0) printf "结论：观察模式（BENCH_GATE=off）——检测到 %d 个基准退化，未拦截", nfail
    else printf "结论：通过——allocs/op 与 B/op 无超阈值退化"
    if (nwarn > 0) printf "；另有 %d 条 WARN 不拦截（时间退化 / 基准增删）\n", nwarn
    else printf "\n"
    exit 0
  }
' "$tmpdir/base.tsv" "$tmpdir/head.tsv" || gate_rc=$?

if [ "$gate_rc" -eq 3 ]; then
  die "没有任何可对比的 allocs/op 或 B/op 数据（-benchmem 未开？）"
fi
if [ "$gate_rc" -ne 0 ] && [ "$gate_rc" -ne 1 ]; then
  die "对比阶段异常退出（rc=$gate_rc）"
fi
exit "$gate_rc"
