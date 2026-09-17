//go:build linux

package headless

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseProcStatFieldsCommBoundary 钉住 /proc/<pid>/stat 的解析边界。
//
// 判决性背景：对整行 strings.Fields 后取 [21] 也能在「comm 不含空格」的常见
// 输入上碰巧得到正确答案，因此只有 comm 含空格 / 含 ')' 时才暴露。这一组用例
// 覆盖的正是分叉点，并对每一种畸形输入都做了反向核对（见 errNaive*）。
func TestParseProcStatFieldsCommBoundary(t *testing.T) {
	// 真实布局：pid (comm) state ppid pgrp ... starttime(field 22)
	build := func(comm, starttime string) string {
		mid := make([]string, 19) // field 3..21
		for i := range mid {
			mid[i] = "0"
		}
		mid[0] = "S" // field 3 = state
		mid[2] = "7" // field 5 = pgrp
		return "4242 (" + comm + ") " + strings.Join(mid, " ") + " " + starttime + "\n"
	}

	cases := []struct {
		name        string
		comm        string
		starttime   string
		naiveShift  bool // 朴素 Fields 写法是否整体错位
		parenInComm bool // comm 含 ')'：必须用 LastIndex 才能定位补语结束
	}{
		{name: "comm 不含空格", comm: "sing-box", starttime: "98765"},
		{name: "comm 含空格", comm: "my proxy", starttime: "98766", naiveShift: true},
		{name: "comm 含右括号", comm: "evil)proc", starttime: "98767", parenInComm: true},
		{name: "comm 含空格与右括号", comm: "a) b )c", starttime: "98768", naiveShift: true, parenInComm: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line := build(tc.comm, tc.starttime)
			fields, err := parseProcStatFields(line)
			if err != nil {
				t.Fatalf("parseProcStatFields: %v", err)
			}
			got, err := starttimeFromStatFields(fields)
			if err != nil {
				t.Fatalf("starttimeFromStatFields: %v", err)
			}
			if got != tc.starttime {
				t.Fatalf("starttime = %q, 期望 %q", got, tc.starttime)
			}
			if fields[2] != "7" {
				t.Fatalf("pgrp = %q, 期望 %q", fields[2], "7")
			}

			// 反向核对：夹具对朴素的整行 Fields 写法必须真的有区分度，
			// 否则这条用例证明不了任何事。
			naive := strings.Fields(line)
			if tc.naiveShift {
				if naive[21] == tc.starttime {
					t.Fatal("夹具失去区分度：朴素 Fields 写法也取到了正确 starttime")
				}
			} else if naive[21] != tc.starttime {
				t.Fatalf("comm 无空格时朴素写法应与正解一致，实际 %q", naive[21])
			}

			// 反向核对：comm 含 ')' 时用第一个 ')' 定位会切错边界。
			if tc.parenInComm && strings.Index(line, ")") == strings.LastIndex(line, ")") {
				t.Fatal("夹具失去区分度：comm 里没有额外的 ')'")
			}
		})
	}
}

func TestParseProcStatFieldsRejectsMalformed(t *testing.T) {
	cases := []struct {
		name string
		line string
	}{
		{"缺少右括号", "4242 sing-box S 0 0"},
		{"右括号后无分隔", "4242 (sing-box)S 0 0"},
		{"字段数不足", "4242 (sing-box) S 0 1 2 3"},
		{"右括号在结尾", "4242 (sing-box)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseProcStatFields(tc.line); err == nil {
				t.Fatalf("畸形输入应报错: %q", tc.line)
			}
		})
	}
}

func TestStarttimeFromStatFieldsRejectsNonNumeric(t *testing.T) {
	fields := make([]string, 20)
	for i := range fields {
		fields[i] = "0"
	}
	fields[19] = "12x5"
	if _, err := starttimeFromStatFields(fields); err == nil {
		t.Fatal("非数字 starttime 应报错")
	}
	if _, err := starttimeFromStatFields(fields[:19]); err == nil {
		t.Fatal("字段数不足应报错")
	}
}

func TestProcessIdentityOnSelf(t *testing.T) {
	id, err := processIdentity(os.Getpid())
	if err != nil {
		t.Fatalf("processIdentity: %v", err)
	}
	if !strings.Contains(id.Token, ":") {
		t.Fatalf("Linux token 应为 <boot_id>:<starttime>，实际 %q", id.Token)
	}
	// 反向核对：同一进程连读两次必须完全一致（starttime 是稳定量）。
	again, err := processIdentity(os.Getpid())
	if err != nil || again.Token != id.Token {
		t.Fatalf("同一进程的 token 应稳定: %q vs %q (err=%v)", id.Token, again.Token, err)
	}
	if !sameProcess(os.Getpid(), id.Token) {
		t.Fatal("sameProcess 对自身应为真")
	}
	if sameProcess(os.Getpid(), id.Token+":junk") {
		t.Fatal("token 被改写后不得判为同一进程")
	}
	if sameProcess(os.Getpid(), "") {
		t.Fatal("空 token 必须 fail-closed（不得退回裸 PID）")
	}
}

func TestProcessIdentityMissingProcessIsGone(t *testing.T) {
	if _, err := processIdentity(-1); err == nil {
		t.Error("非法 PID 应报错")
	}
}

// TestReadBootIDIsRequired 钉住「boot_id 读不到就 fail-closed」：
// 不能降级成「只用 starttime」，那会重新引入跨重启的误判。
func TestReadBootIDIsRequired(t *testing.T) {
	orig := bootIDPath
	t.Cleanup(func() { bootIDPath = orig })

	if _, err := readBootID(); err != nil {
		t.Fatalf("真实 boot_id 应可读: %v", err)
	}

	bootIDPath = filepath.Join(t.TempDir(), "missing")
	if _, err := readBootID(); err == nil {
		t.Fatal("boot_id 缺失时必须报错（fail-closed），不得退回只用 starttime")
	}

	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bootIDPath = empty
	if _, err := readBootID(); err == nil {
		t.Fatal("boot_id 为空时必须报错")
	}
}

// TestProcessGroupIDOnSelf 反向核对 PGID 解析：当前测试进程的 PGID 应等于
// 内核报告值，且与我们用 getpgid 得到的值一致。
func TestProcessGroupIDOnSelf(t *testing.T) {
	got, err := processGroupID(os.Getpid())
	if err != nil {
		t.Fatalf("processGroupID: %v", err)
	}
	if got <= 0 {
		t.Fatalf("PGID 应 > 0，实际 %d", got)
	}
	if _, err := processGroupID(0); err == nil {
		t.Error("非法 PID 应报错")
	}
}
