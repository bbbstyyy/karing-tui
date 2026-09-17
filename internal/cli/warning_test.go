package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func requireModeOf(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := fi.Mode().Perm(); got != want {
		t.Errorf("%s mode = %o, want %o", path, got, want)
	}
}

func requireNoSentinel(t *testing.T, root string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(root, ".karing-tui-root")); !os.IsNotExist(err) {
		t.Fatalf("不应写归属标记: %v", err)
	}
}

// TestCLISurfacesForeignRootWarning 覆盖 V5-4：KARING_HOME 指向无关的
// 既有非空目录时，告警必须真的出现在用户能看到的地方（stderr），并且只出现
// 一次。展示告警本身不得引入任何写入：不建库（C13）、不改权限、不写标记。
func TestCLISurfacesForeignRootWarning(t *testing.T) {
	root := filepath.Join(t.TempDir(), "shared")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "user-notes.txt"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KARING_HOME", root)

	for i := 1; i <= 2; i++ {
		var stdout, stderr bytes.Buffer
		Run([]string{"status", "--json"}, &stdout, &stderr)
		out := stderr.String()
		if strings.Contains(out, "初始化数据目录失败") {
			t.Fatalf("第 %d 次路径层不应失败: %q", i, out)
		}
		if n := strings.Count(out, "警告: "); n != 1 {
			t.Fatalf("第 %d 次告警应恰好出现一次，实际 %d 次: %q", i, n, out)
		}
		if !strings.Contains(out, "不像是本应用的目录") {
			t.Fatalf("第 %d 次告警内容不符: %q", i, out)
		}
	}

	// C13 语义保持：只读命令不创建/迁移数据库。
	if _, err := os.Stat(filepath.Join(root, "karing.db")); !os.IsNotExist(err) {
		t.Errorf("只读命令不应创建数据库: %v", err)
	}
	requireModeOf(t, root, 0o755)
	requireModeOf(t, filepath.Join(root, "user-notes.txt"), 0o644)
	requireNoSentinel(t, root)
	got, err := os.ReadFile(filepath.Join(root, "user-notes.txt"))
	if err != nil || string(got) != "mine\n" {
		t.Fatalf("用户内容应原样保留: %q %v", got, err)
	}
}

// TestCLIForeignRootReuseIsReported 与 V5-3 的「复用但不接管」对齐：
// 已有真实子目录时告警要说明正在复用且权限保持不变。
func TestCLIForeignRootReuseIsReported(t *testing.T) {
	root := filepath.Join(t.TempDir(), "shared")
	if err := os.MkdirAll(filepath.Join(root, "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "user-notes.txt"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KARING_HOME", root)

	for i := 1; i <= 2; i++ {
		var stdout, stderr bytes.Buffer
		Run([]string{"status", "--json"}, &stdout, &stderr)
		out := stderr.String()
		if strings.Contains(out, "初始化数据目录失败") {
			t.Fatalf("第 %d 次路径层不应失败: %q", i, out)
		}
		if !strings.Contains(out, "复用") || !strings.Contains(out, "权限保持不变") {
			t.Fatalf("第 %d 次告警应说明复用且权限不变: %q", i, out)
		}
	}

	requireModeOf(t, root, 0o755)
	requireModeOf(t, filepath.Join(root, "cache"), 0o755)
	requireNoSentinel(t, root)
}

func TestCLINoWarningForOwnedHome(t *testing.T) {
	setupHome(t)
	var stdout, stderr bytes.Buffer
	Run([]string{"status", "--json"}, &stdout, &stderr)
	if strings.Contains(stderr.String(), "警告") {
		t.Fatalf("归本应用所有的数据目录不应有告警: %q", stderr.String())
	}
}

// TestCLISecurityErrorsAreNotMaskedByWarning 钉住边界：symlink / 非目录
// 冲突与 root symlink 是安全错误，必须直接失败，不能被 warning 掩盖。
func TestCLISecurityErrorsAreNotMaskedByWarning(t *testing.T) {
	t.Run("root 是 symlink", func(t *testing.T) {
		base := t.TempDir()
		target := filepath.Join(base, "target")
		if err := os.MkdirAll(target, 0o755); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(base, "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		t.Setenv("KARING_HOME", link)

		var stdout, stderr bytes.Buffer
		if code := Run([]string{"status", "--json"}, &stdout, &stderr); code == 0 {
			t.Fatal("root 是符号链接时必须失败")
		}
		out := stderr.String()
		if !strings.Contains(out, "符号链接") {
			t.Fatalf("应给出安全错误: %q", out)
		}
		if strings.Contains(out, "警告: ") {
			t.Fatalf("安全错误不应用 warning 掩盖: %q", out)
		}
		requireModeOf(t, target, 0o755)
	})

	t.Run("foreign cache 是 symlink", func(t *testing.T) {
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
		t.Setenv("KARING_HOME", root)

		var stdout, stderr bytes.Buffer
		if code := Run([]string{"status", "--json"}, &stdout, &stderr); code == 0 {
			t.Fatal("foreign runtime 是符号链接时必须失败")
		}
		out := stderr.String()
		if !strings.Contains(out, "符号链接") {
			t.Fatalf("应给出安全错误: %q", out)
		}
		if strings.Contains(out, "警告: ") {
			t.Fatalf("安全错误不应用 warning 掩盖: %q", out)
		}
		entries, err := os.ReadDir(outside)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("不跟随 symlink，target 必须为空: %v", entries)
		}
	})
}
