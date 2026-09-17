package platform

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func setHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("KARING_HOME", dir)
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}

func requireMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	if got := mode(t, path); got != want {
		t.Errorf("%s mode = %o, want %o", path, got, want)
	}
}

func writeForeignContent(t *testing.T, root string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "user-notes.txt"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "photos"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestNewPathsFreshRootCreatesAndTightens(t *testing.T) {
	setHome(t, filepath.Join(t.TempDir(), "fresh"))
	p, err := NewPaths()
	if err != nil {
		t.Fatal(err)
	}
	requireMode(t, p.Root, 0o700)
	if _, err := os.Stat(filepath.Join(p.Root, rootSentinelFile)); err != nil {
		t.Fatalf("新目录应写入归属标记: %v", err)
	}
	if p.RootWarning != "" {
		t.Errorf("全新目录不应有警告: %q", p.RootWarning)
	}

	// 二次进入：有标记，仍可收紧且无警告
	if _, err := NewPaths(); err != nil {
		t.Fatal(err)
	}
	requireMode(t, p.Root, 0o700)
}

func TestNewPathsEmptyExistingRootIsTakenOver(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "prepared")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	setHome(t, root)
	p, err := NewPaths()
	if err != nil {
		t.Fatal(err)
	}
	// 空目录视为预建的应用目录：接管（补标记）并收紧。
	requireMode(t, root, 0o700)
	if _, err := os.Stat(filepath.Join(root, rootSentinelFile)); err != nil {
		t.Fatalf("接管空目录应补归属标记: %v", err)
	}
	if p.RootWarning != "" {
		t.Errorf("空目录接管不应有警告: %q", p.RootWarning)
	}
}

// sqliteFixture 写入带真实 SQLite 文件头的夹具。归属判定会校验文件头，
// 因此这里不能为了省事写 0 字节文件（否则夹具一改就会静默失去意义）。
func sqliteFixture(t *testing.T, path string) {
	t.Helper()
	buf := make([]byte, 4096)
	copy(buf, sqliteHeader)
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestNewPathsLegacyEvidenceRequiresRealSQLiteDB(t *testing.T) {
	root := filepath.Join(t.TempDir(), "legacy")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	sqliteFixture(t, filepath.Join(root, "karing.db"))
	setHome(t, root)

	p, err := NewPaths()
	if err != nil {
		t.Fatal(err)
	}
	// 真实数据库是可信的历史安装证据：接管（补标记）并收紧。
	requireMode(t, root, 0o700)
	if _, err := os.Stat(filepath.Join(root, rootSentinelFile)); err != nil {
		t.Fatalf("legacy 目录应补归属标记: %v", err)
	}
	if p.RootWarning != "" {
		t.Errorf("legacy 接管不应有警告: %q", p.RootWarning)
	}
}

// TestNewPathsWeakMarkersAreNotOwnershipEvidence 是 V5-3 的判决性翻转。
// 旧实现把 runtime/cache/logs 与 karing.db-wal/-shm 当作 legacy 正例，
// 于是 `mkdir -p /tmp/shared/cache` 就足以让 /tmp/shared 被认领并 chmod 0700。
func TestNewPathsWeakMarkersAreNotOwnershipEvidence(t *testing.T) {
	emptyFile := func(name string) func(*testing.T, string) {
		return func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, name), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	dir := func(name string) func(*testing.T, string) {
		return func(t *testing.T, root string) {
			if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	cases := []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{"空 karing.db", emptyFile("karing.db")},
		{"非 SQLite 内容的 karing.db", func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, "karing.db"), []byte("just a text file, not a database"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"karing.db 是目录", dir("karing.db")},
		{"karing.db-wal", emptyFile("karing.db-wal")},
		{"karing.db-shm", emptyFile("karing.db-shm")},
		{"runtime", dir("runtime")},
		{"cache", dir("cache")},
		{"logs", dir("logs")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "shared")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			tc.setup(t, root)
			setHome(t, root)

			p, err := NewPaths()
			if err != nil {
				t.Fatalf("NewPaths: %v", err)
			}
			requireMode(t, root, 0o755)
			if _, err := os.Stat(filepath.Join(root, rootSentinelFile)); !os.IsNotExist(err) {
				t.Fatalf("弱证据不应写归属标记: %v", err)
			}
			if p.RootWarning == "" {
				t.Error("未被认领的 root 必须产生警告")
			}
		})
	}
}

func TestNewPathsSymlinkDBIsNotLegacyEvidence(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside.db")
	sqliteFixture(t, outside)
	root := filepath.Join(base, "shared")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "karing.db")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	setHome(t, root)

	p, err := NewPaths()
	if err != nil {
		t.Fatal(err)
	}
	// 指向别处的 symlink 不能作为归属证据，也不能被跟随。
	requireMode(t, root, 0o755)
	requireMode(t, outside, 0o644)
	if p.RootWarning == "" {
		t.Error("symlink karing.db 不应被认领")
	}
}

func TestNewPathsRejectsSymlinkRoot(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "real-target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	setHome(t, link)

	if _, err := NewPaths(); err == nil {
		t.Fatal("KARING_HOME 本身是符号链接时必须直接拒绝")
	}
	// target 不被 chmod、不被写标记、不被创建任何应用子目录。
	requireMode(t, target, 0o755)
	if _, err := os.Stat(filepath.Join(target, rootSentinelFile)); !os.IsNotExist(err) {
		t.Fatalf("不得穿过 symlink 在 target 写标记: %v", err)
	}
	for _, name := range []string{"runtime", "cache", "logs"} {
		if _, err := os.Stat(filepath.Join(target, name)); !os.IsNotExist(err) {
			t.Fatalf("不得穿过 symlink 在 target 创建 %s: %v", name, err)
		}
	}
}

// TestNewPathsForeignRootReusesExistingAppDirs 覆盖 v0.4.0 兼容性要求：
// 老版本在用户指定的 KARING_HOME 里留下过 runtime/cache/logs/runtime/bin
// 却没有 sentinel。升级后必须能继续使用这些目录，且绝不 chmod、绝不写
// sentinel；连续两次调用也不能因为第一次创建的目录而自锁。
func TestNewPathsForeignRootReusesExistingAppDirs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "shared")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"runtime", "runtime/bin", "cache", "logs"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeForeignContent(t, root)
	setHome(t, root)

	for i := 1; i <= 2; i++ {
		p, err := NewPaths()
		if err != nil {
			t.Fatalf("第 %d 次 NewPaths: %v", i, err)
		}
		if p.RootWarning == "" {
			t.Fatalf("第 %d 次应产生 foreign root 警告", i)
		}
		if !strings.Contains(p.RootWarning, "复用") || !strings.Contains(p.RootWarning, "权限保持不变") {
			t.Fatalf("第 %d 次警告应说明复用了已有目录且权限不变: %q", i, p.RootWarning)
		}
	}

	for _, d := range []string{"", "runtime", "runtime/bin", "cache", "logs", "photos"} {
		requireMode(t, filepath.Join(root, d), 0o755)
	}
	if _, err := os.Stat(filepath.Join(root, rootSentinelFile)); !os.IsNotExist(err) {
		t.Fatalf("复用不是 ownership 转移，不应写标记: %v", err)
	}
	for _, f := range []string{"user-notes.txt", "photos"} {
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			t.Errorf("用户内容应原样保留: %v", err)
		}
	}
}

func TestNewPathsForeignSubdirConflictsFailClosed(t *testing.T) {
	t.Run("runtime 是 symlink", func(t *testing.T) {
		base := t.TempDir()
		root := filepath.Join(base, "shared")
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(base, "outside")
		if err := os.MkdirAll(outside, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "runtime")); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		setHome(t, root)

		if _, err := NewPaths(); err == nil {
			t.Fatal("foreign runtime 是符号链接时必须 fail-closed")
		}
		// runtime 是循环里的第一个，因此此时不应有任何目录被创建到 target。
		requireMode(t, outside, 0o755)
		entries, err := os.ReadDir(outside)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("不跟随 symlink：target 必须是空的，实际 %v", entries)
		}
		requireMode(t, root, 0o755)
		if _, err := os.Stat(filepath.Join(root, "cache")); !os.IsNotExist(err) {
			t.Fatalf("出错路径不应继续创建其它子目录: %v", err)
		}
	})

	t.Run("cache 是普通文件", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "shared")
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "cache"), []byte("mine\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		setHome(t, root)

		if _, err := NewPaths(); err == nil {
			t.Fatal("foreign cache 是普通文件时必须 fail-closed")
		}
		got, err := os.ReadFile(filepath.Join(root, "cache"))
		if err != nil {
			t.Fatalf("同名文件不应被删除或覆盖: %v", err)
		}
		if string(got) != "mine\n" {
			t.Fatalf("同名文件内容应保持不变: %q", got)
		}
		requireMode(t, filepath.Join(root, "cache"), 0o644)
	})

	t.Run("owned root 下 runtime 是 symlink", func(t *testing.T) {
		base := t.TempDir()
		root := filepath.Join(base, "owned")
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, rootSentinelFile), []byte("sentinel"), 0o600); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(base, "outside")
		if err := os.MkdirAll(outside, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "runtime")); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		setHome(t, root)

		if _, err := NewPaths(); err == nil {
			t.Fatal("owned root 下的符号链接子目录同样必须拒绝")
		}
		requireMode(t, outside, 0o755)
	})
}

func TestNewPathsForeignRootKeepsPermissions(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "some-existing-nonempty-dir")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeForeignContent(t, root)
	setHome(t, root)
	p, err := NewPaths()
	if err != nil {
		t.Fatal(err)
	}
	// 验收：非空通用目录的权限不被改变，也不写归属标记。
	requireMode(t, root, 0o755)
	if _, err := os.Stat(filepath.Join(root, rootSentinelFile)); !os.IsNotExist(err) {
		t.Fatalf("无关目录不应写归属标记: %v", err)
	}
	if p.RootWarning == "" {
		t.Error("无关目录应产生警告文案")
	}
	// 应用自建子目录仍收紧到 0700。
	for _, dir := range []string{p.Runtime, p.Cache, p.Logs, filepath.Dir(p.CoreBin)} {
		requireMode(t, dir, 0o700)
	}
	// 无关内容原样保留。
	if _, err := os.Stat(filepath.Join(root, "user-notes.txt")); err != nil {
		t.Errorf("无关文件应原样保留: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "photos")); err != nil {
		t.Errorf("无关子目录应原样保留: %v", err)
	}
}

func TestNewPathsRootIsFileFails(t *testing.T) {
	root := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(root, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	setHome(t, root)
	if _, err := NewPaths(); err == nil {
		t.Fatal("root 是文件时应返回错误")
	}
}

func TestNewPathsDefaultSubdirModes(t *testing.T) {
	setHome(t, filepath.Join(t.TempDir(), "fresh"))
	p, err := NewPaths()
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{p.Runtime, p.Cache, p.Logs, filepath.Dir(p.CoreBin)} {
		requireMode(t, dir, 0o700)
	}
	// 布局契约回归：路径拼接不被误改。
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		if filepath.Base(p.CoreBin) != "sing-box" || filepath.Base(p.DB) != "karing.db" {
			t.Errorf("布局契约被改变: DB=%s CoreBin=%s", p.DB, p.CoreBin)
		}
	}
}
