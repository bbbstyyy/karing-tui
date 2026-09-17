package application

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"

	_ "modernc.org/sqlite" // 只为了在测试里直接改写 user_version

	"github.com/bbbstyyy/karing-tui/internal/platform"
	"github.com/bbbstyyy/karing-tui/internal/storage"
)

// C13：只读入口（CLI 的 status / profile list / route list / diagnose 等）走
// query-only 打开策略——库不存在时失败，且**不创建**数据库文件。
//
// 反向核对：同一个 paths 上用可写入口 New 必须能建库，证明上面的「不存在」断言
// 不是空转（例如断言写错了路径）。
func TestNewReadOnlyDoesNotCreateDatabase(t *testing.T) {
	t.Setenv("KARING_HOME", t.TempDir())
	paths, err := platform.NewPaths()
	if err != nil {
		t.Fatal(err)
	}

	app, err := NewReadOnly(paths)
	if app != nil {
		t.Fatal("库不存在时不应返回可用应用实例")
	}
	if !errors.Is(err, storage.ErrNotInitialized) {
		t.Fatalf("期望 ErrNotInitialized，得到 %v", err)
	}
	if !storage.ReadOnlyUnavailable(err) {
		t.Fatal("未初始化必须被识别为「只读不可用」族")
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, statErr := os.Stat(paths.DB + suffix); !os.IsNotExist(statErr) {
			t.Fatalf("只读入口在 %q 上留下了文件", paths.DB+suffix)
		}
	}

	// 可写入口仍然建库，且建完库后只读入口可用。
	writable, err := New(paths)
	if err != nil {
		t.Fatalf("可写入口: %v", err)
	}
	if _, err := os.Stat(paths.DB); err != nil {
		t.Fatalf("可写入口没有建库：%v", err)
	}
	if err := writable.Close(); err != nil {
		t.Fatal(err)
	}

	ro, err := NewReadOnly(paths)
	if err != nil {
		t.Fatalf("已初始化的库上只读入口应可用: %v", err)
	}
	defer ro.Close()
}

// C13：只读入口不得因为 schema 版本比程序旧就自行迁移——必须让用户先跑主程序，
// 否则只读命令会读到迁移中途的表结构。
func TestNewReadOnlyRefusesStaleSchema(t *testing.T) {
	t.Setenv("KARING_HOME", t.TempDir())
	paths, err := platform.NewPaths()
	if err != nil {
		t.Fatal(err)
	}
	app, err := New(paths)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	downgradeSchema(t, paths, 1)

	ro, err := NewReadOnly(paths)
	if ro != nil {
		t.Fatal("旧 schema 上不应返回可用实例")
	}
	if !errors.Is(err, storage.ErrSchemaTooOld) {
		t.Fatalf("期望 ErrSchemaTooOld，得到 %v", err)
	}
	if !storage.ReadOnlyUnavailable(err) {
		t.Fatal("旧 schema 属于「只读不可用」族")
	}
}

// downgradeSchema 直接把库的 user_version 写成 version，
// 模拟「二进制已升级、主程序尚未迁移」。
func downgradeSchema(t *testing.T, paths *platform.Paths, version int) {
	t.Helper()
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: paths.DB}).String()+"?mode=rw")
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	defer db.Close()
	// PRAGMA 不接受绑定参数，只能拼字面量（version 来自测试常量）。
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		t.Fatalf("写入 user_version: %v", err)
	}
}
