package application

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// V7-2 判决性用例：`.pre-restore` 的写入必须对「当前数据库」与「symlink 目标」
// 都零影响。旧实现的 copyFile 用 O_CREATE|O_WRONLY|O_TRUNC 直接写最终目标，
// 目标与源是同一 inode 时会在备份刚开始的瞬间把当前库截断成 0 字节。

// assertHasSubscription 直接打开指定的数据库文件（只读）确认某订阅存在。
// 刻意不复用 App 的连接：判据要落在磁盘上的那个文件。
func assertHasSubscription(path, name string) error {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return err
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM subscriptions WHERE name=?`, name).Scan(&n); err != nil {
		return err
	}
	if n != 1 {
		return &countMismatch{name: name, got: n}
	}
	return nil
}

type countMismatch struct {
	name string
	got  int
}

func (e *countMismatch) Error() string {
	return "订阅 " + e.name + " 未找到（命中 " + itoa(e.got) + " 行）"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// setupRestoreWithCurrentDB 造出「当前库有数据 + 归档已预检通过」的现场。
func setupRestoreWithCurrentDB(t *testing.T) (*App, *PreparedRestore, string) {
	t.Helper()
	app, paths := setupApp(t)
	if _, err := app.Subs.Add(context.Background(), "before-restore", "http://example.invalid", ""); err != nil {
		t.Fatal(err)
	}
	archive, err := app.Backup(filepath.Join(t.TempDir(), "valid.zip"))
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareRestore(paths, archive)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = prepared.Close() })
	return app, prepared, paths.DB + ".pre-restore"
}

// `.pre-restore` 与当前库是 hardlink 时：
//   - 备份阶段不得截断当前库；
//   - `.pre-restore` 必须是一份真正可读的「恢复前数据库」；
//   - 恢复结束后两者必须是不同的 inode。
func TestRestorePreBackupHardlinkCannotTruncateCurrentDB(t *testing.T) {
	app, prepared, preRestore := setupRestoreWithCurrentDB(t)

	if err := os.Remove(preRestore); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Link(app.Paths.DB, preRestore); err != nil {
		t.Fatalf("创建 hardlink 夹具失败: %v", err)
	}
	linked, err := os.Stat(preRestore)
	if err != nil {
		t.Fatal(err)
	}
	dbInfo, err := os.Stat(app.Paths.DB)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(linked, dbInfo) {
		t.Fatal("夹具无效：两个路径应为同一 inode")
	}

	if err := app.RestorePrepared(context.Background(), prepared); err != nil {
		t.Fatalf("RestorePrepared: %v", err)
	}

	backup, err := os.Stat(preRestore)
	if err != nil {
		t.Fatal(err)
	}
	if backup.Size() == 0 {
		t.Fatal("`.pre-restore` 是 0 字节：备份当前库时把共享 inode 截断了")
	}
	if err := assertHasSubscription(preRestore, "before-restore"); err != nil {
		t.Fatalf("`.pre-restore` 不是恢复前的数据库: %v", err)
	}
	after, err := os.Stat(app.Paths.DB)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(backup, after) {
		t.Fatal("恢复后 `.pre-restore` 仍与当前库共享 inode")
	}
}

// `.pre-restore` 是指向无关文件的 symlink 时，绝不能跟随它写入。
func TestRestorePreBackupSymlinkDoesNotFollowTarget(t *testing.T) {
	app, prepared, preRestore := setupRestoreWithCurrentDB(t)

	victim := filepath.Join(t.TempDir(), "victim.txt")
	const sentinel = "DO NOT TOUCH"
	if err := os.WriteFile(victim, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(preRestore); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, preRestore); err != nil {
		t.Fatal(err)
	}

	if err := app.RestorePrepared(context.Background(), prepared); err != nil {
		t.Fatalf("RestorePrepared: %v", err)
	}

	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != sentinel {
		t.Fatalf("symlink 被跟随：victim.txt 内容被改写为 %q", got)
	}
	fi, err := os.Lstat(preRestore)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("`.pre-restore` 仍是 symlink，备份没有真正落盘")
	}
	if err := assertHasSubscription(preRestore, "before-restore"); err != nil {
		t.Fatalf("`.pre-restore` 不是恢复前的数据库: %v", err)
	}
}

// 复制中途失败时，已有的 `.pre-restore` 必须逐字节不变，且不留临时文件残骸。
//
// 注入方式是「src 可打开但读取失败」：os.Open 一个目录在 Unix 上会成功，
// io.Copy 的 Read 才返回 EISDIR。这比「磁盘满」之类的条件稳定得多，
// 且失败点正好落在「temp 已建、dest 还没被碰」的窗口里。
func TestAtomicCopyFailureLeavesPreviousBackupUntouched(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "karing.db.pre-restore")
	const oldGood = "OLD-GOOD"
	if err := os.WriteFile(dest, []byte(oldGood), 0o600); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(dir, "karing.db")
	if err := os.WriteFile(unrelated, []byte("CURRENT-DB"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "srcdir")
	if err := os.Mkdir(src, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := atomicCopyFile(dest, src, 0o600); err == nil {
		t.Fatal("读取目录作为源应当失败")
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != oldGood {
		t.Fatalf("失败后旧备份被改动: %q", got)
	}
	unchanged, err := os.ReadFile(unrelated)
	if err != nil {
		t.Fatal(err)
	}
	if string(unchanged) != "CURRENT-DB" {
		t.Fatalf("失败后当前库被改动: %q", unchanged)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".atomic-copy-") {
			t.Errorf("失败路径残留临时文件: %s", e.Name())
		}
	}
}
