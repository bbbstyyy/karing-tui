#!/usr/bin/env bash
# bench_gate_test.sh — V5-5：用固定 fixture 钉住 bench_gate.sh 的完整性判据。
#
# 为什么不直接跑真实 benchmark：真实数据自己会抖动，写不出稳定断言；
# 固定 fixture 才能给出确定的期望值，也才能变成 CI 里可复跑的回归门禁。
#
# 判决性用例：只有 ns/op、没有 allocs/op 与 B/op 时（等价于误删 -benchmem）
# 必须判完整性失败（rc=2）。旧实现的 `ncmp` 把 ns/op 也算作「有可对比数据」，
# 于是这种情况会静默放行（rc=0）——请对照提交信息里的实测记录。
#
# 退出码: 0 全部通过；1 有失败用例

set -uo pipefail

script_dir="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
# BENCH_GATE_SCRIPT 允许把同一套 fixture 指向另一份 bench_gate.sh，
# 用于「回退实验」：对着改动前的脚本跑一遍，确认判决性用例确实会变红。
gate="${BENCH_GATE_SCRIPT:-$script_dir/bench_gate.sh}"
data="$script_dir/testdata/bench_gate"

[ -x "$gate" ] || { echo "bench_gate_test: 找不到可执行的 $gate" >&2; exit 1; }
[ -d "$data" ] || { echo "bench_gate_test: 找不到 fixture 目录 $data" >&2; exit 1; }

npass=0
nfail=0

# check <期望rc> <说明> <base fixture> <head fixture> [输出应包含的文本] [env...]
check() {
  local want_rc="$1" desc="$2" base="$3" head="$4" pattern="$5"
  shift 5
  local out rc
  out="$(env "$@" "$gate" "$base" "$head" 2>&1)"
  rc=$?
  if [ "$rc" != "$want_rc" ]; then
    printf 'FAIL  %s：期望 rc=%s，实际 rc=%s\n%s\n' "$desc" "$want_rc" "$rc" "$out" >&2
    nfail=$((nfail + 1))
    return
  fi
  if [ -n "$pattern" ] && ! printf '%s\n' "$out" | grep -q -- "$pattern"; then
    printf 'FAIL  %s：输出缺少 %s\n%s\n' "$desc" "$pattern" "$out" >&2
    nfail=$((nfail + 1))
    return
  fi
  printf 'PASS  %s（rc=%s）\n' "$desc" "$rc"
  npass=$((npass + 1))
}

# ①判决性：只有 ns/op（内存指标全不可解析）→ 完整性失败。
# 注意 die 固定 exit 2，所以这里断言的就是 2。
check 2 "只有 ns/op 时判完整性失败（等价于误删 -benchmem）" \
  "$data/base-ns-only.txt" "$data/head-ns-only.txt" "allocs/op 或 B/op"

# ②同一份 fixture 在观察模式下也必须失败：完整性是数据正确性问题，
# 与「是否拦截退化」是两个正交开关。
check 2 "观察模式下完整性判据依然生效" \
  "$data/base-ns-only.txt" "$data/head-ns-only.txt" "" BENCH_GATE=off

# ③完整 benchmem 且 base==head → 放行。
check 0 "完整 benchmem fixture 且 base==head 放行" \
  "$data/base-full.txt" "$data/head-full.txt" "结论：通过"

# ④有回归（B/op 16→48、allocs 1→3）→ 拦截。
check 1 "内存指标退化被拦截" \
  "$data/base-full.txt" "$data/head-regress.txt" "结论：拦截"

# ⑤观察模式保持既有语义：照常报告，但恒放行。
check 0 "BENCH_GATE=off 观察模式放行" \
  "$data/base-full.txt" "$data/head-regress.txt" "观察模式" BENCH_GATE=off

# ⑥基准被改名/删除只 WARN（另一个基准仍有内存指标可比，因此 nmem>0，
# 不能把它误判成完整性失败）。
check 0 "仅 base 有的基准只 WARN" \
  "$data/base-full.txt" "$data/head-missing-bench.txt" "仅 base 有"

printf '\nbench_gate_test: %d 通过, %d 失败\n' "$npass" "$nfail"
[ "$nfail" -eq 0 ] || exit 1
exit 0
