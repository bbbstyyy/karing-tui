package application

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// findBackupArtifacts 返回目录下遗留的备份临时物（staging 目录 / 原子写临时文件）。
func findBackupArtifacts(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	var leaked []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".backup-") || strings.HasPrefix(e.Name(), ".karing-backup-") {
			leaked = append(leaked, e.Name())
		}
	}
	return leaked
}

func fileBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return b
}

// TestBackupRejectsProtectedDestinations 是 V5-1 的判决性测试。
// 旧实现里 dest 直接决定快照路径并对 dest 做 O_TRUNC，因此
// `karing backup export $KARING_HOME/karing.db` 会把**正处于打开状态**的
// 活动数据库截断成 ZIP，并留下 -wal/-shm 残骸。任何受保护目标都必须
// 在动手之前被拒绝，且数据库事后仍可正常查询。
func TestBackupRejectsProtectedDestinations(t *testing.T) {
	app, paths := setupApp(t)
	ctx := context.Background()
	if _, err := app.Subs.Add(ctx, "保护目标", "http://example.com/sub", ""); err != nil {
		t.Fatalf("Add: %v", err)
	}

	cases := []struct{ name, dest string }{
		{"数据库", paths.DB},
		{"WAL", paths.DB + "-wal"},
		{"SHM", paths.DB + "-shm"},
		{"生成配置", paths.Config},
		{"核心二进制", paths.CoreBin},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := app.Backup(tc.dest)
			if err == nil {
				t.Fatalf("Backup(%s) 必须被拒绝", tc.dest)
			}
			if !strings.Contains(err.Error(), "重叠") {
				t.Fatalf("错误文案应说明路径重叠，实际: %v", err)
			}
			// 判决性：旧实现走到这里数据库已损坏，这一步必然读不出来。
			subs, err := app.DB.ListSubscriptions()
			if err != nil {
				t.Fatalf("数据库仍应可读: %v", err)
			}
			if len(subs) != 1 {
				t.Fatalf("数据库内容应完好，实际订阅数 %d", len(subs))
			}
		})
	}
}

// TestBackupRejectsAliasedDestinations 覆盖路径别名之外的 inode 别名与
// symlink：判定必须同时看 Lstat 类型与 os.SameFile。
func TestBackupRejectsAliasedDestinations(t *testing.T) {
	app, paths := setupApp(t)
	dir := t.TempDir()

	link := filepath.Join(dir, "db-link.zip")
	if err := os.Symlink(paths.DB, link); err != nil {
		t.Skipf("符号链接不可用: %v", err)
	}
	if _, err := app.Backup(link); err == nil {
		t.Fatal("指向活动数据库的 symlink 目标必须被拒绝")
	}
	fi, err := os.Lstat(link)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink 本身不应被覆盖: %v %v", fi, err)
	}
	if got, err := os.Readlink(link); err != nil || got != paths.DB {
		t.Fatalf("symlink 指向不应改变: %q %v", got, err)
	}

	hard := filepath.Join(dir, "db-hard.zip")
	if err := os.Link(paths.DB, hard); err != nil {
		t.Skipf("硬链接不可用: %v", err)
	}
	if _, err := app.Backup(hard); err == nil {
		t.Fatal("指向活动数据库的 hardlink 目标必须被拒绝（os.SameFile）")
	}
	dbInfo, err := os.Stat(paths.DB)
	if err != nil {
		t.Fatalf("Stat(DB): %v", err)
	}
	hardInfo, err := os.Stat(hard)
	if err != nil {
		t.Fatalf("Stat(hardlink): %v", err)
	}
	if !os.SameFile(dbInfo, hardInfo) {
		t.Fatal("hardlink 不应被替换成新文件")
	}
	if _, err := app.DB.ListSubscriptions(); err != nil {
		t.Fatalf("数据库仍应可读: %v", err)
	}
}

// TestWriteBackupAtomicallyKeepsExistingOnFailure 钉住「失败不破坏旧备份」：
// 注入一个写一半就报错的 callback，旧 dest 必须逐字节不变，临时文件必须清理。
func TestWriteBackupAtomicallyKeepsExistingOnFailure(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "backup.zip")
	old := []byte("OLD-BACKUP-BYTES")
	if err := os.WriteFile(dest, old, 0o600); err != nil {
		t.Fatal(err)
	}

	sentinel := errors.New("写入中断")
	err := writeBackupAtomically(dest, func(w io.Writer) error {
		if _, err := w.Write([]byte("HALF")); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("应原样透传写入错误，实际: %v", err)
	}
	if got := fileBytes(t, dest); !bytes.Equal(got, old) {
		t.Fatalf("旧备份应逐字节不变，实际 %q", got)
	}
	if leaked := findBackupArtifacts(t, dir); len(leaked) != 0 {
		t.Fatalf("失败路径必须清理临时文件，遗留: %v", leaked)
	}
}

// TestWriteBackupAtomicallyReplacesOnSuccess 是上一条的反向核对：
// 成功路径必须真的替换目标，而不是「什么都不做所以旧文件没坏」。
func TestWriteBackupAtomicallyReplacesOnSuccess(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "backup.zip")
	if err := os.WriteFile(dest, []byte("OLD"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := []byte("NEW-BACKUP-BYTES")
	if err := writeBackupAtomically(dest, func(w io.Writer) error {
		_, err := w.Write(want)
		return err
	}); err != nil {
		t.Fatalf("writeBackupAtomically: %v", err)
	}
	if got := fileBytes(t, dest); !bytes.Equal(got, want) {
		t.Fatalf("目标应被替换，实际 %q", got)
	}
	if leaked := findBackupArtifacts(t, dir); len(leaked) != 0 {
		t.Fatalf("成功路径也应清理临时文件，遗留: %v", leaked)
	}
	fi, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("备份文件权限应为 0600，实际 %o", perm)
	}
}

// TestBackupArchiveFailureLeavesExistingBackupIntact 覆盖归档内容生成失败
// （快照缺失）时旧备份不受影响。
func TestBackupArchiveFailureLeavesExistingBackupIntact(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "backup.zip")
	old := []byte("OLD-BACKUP")
	if err := os.WriteFile(dest, old, 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "not-there.db")
	err := writeBackupAtomically(dest, func(w io.Writer) error {
		return writeBackupArchive(w, missing, missing)
	})
	if err == nil {
		t.Fatal("快照缺失时归档生成应失败")
	}
	if got := fileBytes(t, dest); !bytes.Equal(got, old) {
		t.Fatalf("旧备份应逐字节不变，实际 %q", got)
	}
	if leaked := findBackupArtifacts(t, dir); len(leaked) != 0 {
		t.Fatalf("临时文件应被清理，遗留: %v", leaked)
	}
}

// TestBackupStagingFailureLeavesDestUntouched 覆盖「快照创建失败」：
// dest 不得被创建，更不能被修改。
func TestBackupStagingFailureLeavesDestUntouched(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 下目录权限位不生效，无法构造不可写 staging")
	}
	app, paths := setupApp(t)
	dest := filepath.Join(t.TempDir(), "backup.zip")
	old := []byte("OLD-BACKUP")
	if err := os.WriteFile(dest, old, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(paths.Runtime, 0o500); err != nil {
		t.Fatal(err)
	}
	// 注意注册顺序：cleanup 后进先出，权限恢复必须晚于 t.TempDir() 的清理
	// 才会先执行（否则临时目录自身无法被删除）。
	t.Cleanup(func() { _ = os.Chmod(paths.Runtime, 0o700) })

	if _, err := app.Backup(dest); err == nil {
		t.Fatal("staging 不可写时 Backup 应失败")
	}
	if got := fileBytes(t, dest); !bytes.Equal(got, old) {
		t.Fatalf("失败时 dest 应逐字节不变，实际 %q", got)
	}

	app2dest := filepath.Join(t.TempDir(), "fresh.zip")
	if _, err := app.Backup(app2dest); err == nil {
		t.Fatal("staging 不可写时 Backup 应失败")
	}
	if _, err := os.Stat(app2dest); !os.IsNotExist(err) {
		t.Fatalf("失败时不应创建 dest: %v", err)
	}
}

// TestBackupLeavesNoSnapshotOrStagingArtifacts 钉住：快照不再出现在用户
// 可见的目标目录附近，staging 与临时文件全部回收。
func TestBackupLeavesNoSnapshotOrStagingArtifacts(t *testing.T) {
	app, paths := setupApp(t)
	dest := filepath.Join(t.TempDir(), "backup.zip")
	if _, err := app.Backup(dest); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if _, err := os.Stat(dest + ".snapshot.db"); !os.IsNotExist(err) {
		t.Errorf("不应在目标附近留下 .snapshot.db（err=%v）", err)
	}
	entries, err := os.ReadDir(filepath.Dir(dest))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "backup.zip" {
			t.Errorf("目标目录出现了多余产物: %s", e.Name())
		}
	}
	if leaked := findBackupArtifacts(t, paths.Runtime); len(leaked) != 0 {
		t.Errorf("staging 未清理: %v", leaked)
	}
}

// TestBackupRelativePathResolvesAbsolute 保证相对路径仍按 CWD 归一化。
func TestBackupRelativePathResolvesAbsolute(t *testing.T) {
	app, _ := setupApp(t)
	dir := t.TempDir()
	t.Chdir(dir)

	got, err := app.Backup("rel-backup.zip")
	if err != nil {
		t.Fatalf("Backup(相对路径): %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("应返回绝对路径，实际 %q", got)
	}
	want := filepath.Join(mustAbs(t, dir), "rel-backup.zip")
	if got != want {
		t.Fatalf("返回路径不符: got %q want %q", got, want)
	}
	if _, err := os.Stat(got); err != nil {
		t.Fatalf("备份文件不存在: %v", err)
	}
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}
