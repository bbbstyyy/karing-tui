package core

import "testing"

// C11：version 是增量缓存的失效键，必须在每次 AppendLine 后自增——
// 包括触发回绕的那次写入（回绕只影响 drop/first，两者语义独立）。
func TestLogBufVersionCountsEveryAppend(t *testing.T) {
	b := NewLogBuf(3)

	// 反向核对：无写入时 version 稳定，证明下面的自增断言不是永真。
	_, v0, _ := b.Snapshot()
	_, v0again, _ := b.Snapshot()
	if v0again != v0 {
		t.Fatalf("无写入时 version 变化: %d -> %d", v0, v0again)
	}

	b.AppendLine("a")
	first, v1, lines := b.Snapshot()
	if v1 != v0+1 {
		t.Errorf("一次写入后 version = %d, 期望 %d", v1, v0+1)
	}
	if first != 0 || len(lines) != 1 || lines[0] != "a" {
		t.Errorf("first = %d, lines = %v; 期望 0 与 [a]", first, lines)
	}

	// 写满 3 行触发回绕：version 仍逐次自增，first（绝对行号基准）照旧前进。
	for _, s := range []string{"b", "c", "d"} {
		b.AppendLine(s)
	}
	first2, v4, lines2 := b.Snapshot()
	if v4 != v0+4 {
		t.Errorf("四次写入（含回绕）后 version = %d, 期望 %d", v4, v0+4)
	}
	if first2 != 1 {
		t.Errorf("回绕后 first = %d, 期望 1（绝对行号基准语义不得破坏）", first2)
	}
	if len(lines2) != 3 || lines2[0] != "b" || lines2[2] != "d" {
		t.Errorf("回绕后 lines = %v, 期望 [b c d]", lines2)
	}

	// Write() 拆行写入同样计入 version（io.Writer 路径与 AppendLine 一致）。
	b2 := NewLogBuf(10)
	_, vbase, _ := b2.Snapshot()
	_, _ = b2.Write([]byte("x\ny\n"))
	if _, vgot, _ := b2.Snapshot(); vgot != vbase+2 {
		t.Errorf("Write 两行后 version = %d, 期望 %d", vgot, vbase+2)
	}
}
