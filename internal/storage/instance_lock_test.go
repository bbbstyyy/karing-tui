package storage

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bbbstyyy/karing-tui/internal/platform"
)

func TestInstanceLockSerializesOwners(t *testing.T) {
	t.Setenv("KARING_HOME", t.TempDir())
	paths, err := platform.NewPaths()
	if err != nil {
		t.Fatal(err)
	}
	first, err := AcquireInstance(paths)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireInstance(paths); err == nil {
		t.Fatal("第二个实例不应获取锁")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := AcquireInstance(paths)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(paths.Runtime, instanceLockName)); !os.IsNotExist(err) {
		t.Fatalf("锁文件未清理: %v", err)
	}
}

func TestInstanceLockRemovesDeadOwner(t *testing.T) {
	t.Setenv("KARING_HOME", t.TempDir())
	paths, err := platform.NewPaths()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(paths.Runtime, instanceLockName)
	if err := os.WriteFile(path, []byte("2147483647\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := AcquireInstance(paths)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}
