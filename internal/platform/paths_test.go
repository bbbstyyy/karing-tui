package platform

import (
	"os"
	"path/filepath"
	"runtime"
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

func TestNewPathsLegacyRootIsRecognized(t *testing.T) {
	for _, marker := range []string{"karing.db", "karing.db-wal", "runtime", "cache", "logs"} {
		t.Run(marker, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "legacy")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			switch marker {
			case "karing.db", "karing.db-wal":
				if err := os.WriteFile(filepath.Join(root, marker), []byte{}, 0o644); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.MkdirAll(filepath.Join(root, marker), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			setHome(t, root)
			p, err := NewPaths()
			if err != nil {
				t.Fatal(err)
			}
			// 老用户目录能自动识别并收紧权限 + 补 sentinel。
			requireMode(t, root, 0o700)
			if _, err := os.Stat(filepath.Join(root, rootSentinelFile)); err != nil {
				t.Fatalf("legacy 目录应补归属标记: %v", err)
			}
			if p.RootWarning != "" {
				t.Errorf("legacy 目录不应有警告: %q", p.RootWarning)
			}
		})
	}
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
