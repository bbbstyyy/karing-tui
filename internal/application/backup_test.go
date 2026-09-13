package application

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bbbstyyy/karing-tui/internal/storage"
)

func TestBackupAndRestore(t *testing.T) {
	app, paths := setupApp(t)
	ctx := context.Background()

	// 造数据：一个订阅
	if _, err := app.Subs.Add(ctx, "备份测试", "http://example.com/sub", ""); err != nil {
		t.Fatalf("Add: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "backup.zip")
	got, err := app.Backup(dest)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if got != dest {
		t.Errorf("Backup 返回 %q, 期望 %q", got, dest)
	}

	// 归档内容校验
	r, err := zip.OpenReader(dest)
	if err != nil {
		t.Fatalf("打开归档: %v", err)
	}
	defer r.Close()
	names := map[string]bool{}
	for _, f := range r.File {
		names[f.Name] = true
	}
	if !names["karing.db"] || !names["meta.txt"] {
		t.Errorf("归档缺少必要文件: %v", names)
	}

	// 数据分叉：再添加一个订阅（不应出现在恢复后的库中）
	if _, err := app.Subs.Add(ctx, "恢复前新增", "http://example.com/other", ""); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// 恢复：停核心（未运行）→ 关库 → RestoreArchive
	app.Core.Stop()
	if err := app.DB.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := RestoreArchive(paths, dest); err != nil {
		t.Fatalf("RestoreArchive: %v", err)
	}

	// 重新打开：应只剩备份时的 1 个订阅
	db, err := storage.Open(paths)
	if err != nil {
		t.Fatalf("重新打开: %v", err)
	}
	defer db.Close()
	subs, err := db.ListSubscriptions()
	if err != nil {
		t.Fatalf("ListSubscriptions: %v", err)
	}
	if len(subs) != 1 || subs[0].Name != "备份测试" {
		t.Errorf("恢复后订阅不符: %+v", subs)
	}

	// 当前库应已备份为 pre-restore
	if _, err := os.Stat(paths.DB + ".pre-restore"); err != nil {
		t.Errorf("pre-restore 备份不存在: %v", err)
	}
}

func TestBackupDefaultPath(t *testing.T) {
	app, paths := setupApp(t)
	dest, err := app.Backup("")
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if filepath.Dir(dest) != filepath.Join(paths.Root, "backups") {
		t.Errorf("默认备份目录不符: %s", dest)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Errorf("备份文件不存在: %v", err)
	}
}

func TestRestoreInvalidArchive(t *testing.T) {
	app, paths := setupApp(t)
	defer app.DB.Close()

	bad := filepath.Join(t.TempDir(), "bad.zip")
	f, err := os.Create(bad)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, _ := zw.Create("karing.db")
	w.Write([]byte("not a sqlite file"))
	zw.Close()
	f.Close()

	if err := RestoreArchive(paths, bad); err == nil {
		t.Error("无效 SQLite 内容应返回错误")
	}
	// 原库不受影响
	if _, err := app.DB.ListSubscriptions(); err != nil {
		t.Fatalf("原库仍可读: %v", err)
	}
}
