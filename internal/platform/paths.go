// Package platform 处理 Unix 平台的数据目录定位与文件系统布局。
package platform

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// AppName 是应用在文件系统中使用的目录名。
const AppName = "karing-tui"

// dbFileName 是主数据库文件名。归属判定与路径拼装共用同一个字面量，
// 避免两处漂移导致「按 A 名字判归属、按 B 名字建库」。
const dbFileName = "karing.db"

// sqliteHeader 是 SQLite 数据库文件的前 16 字节，用于确认一个名为
// karing.db 的文件确实是本应用的数据库，而不是恰好同名的无关文件。
const sqliteHeader = "SQLite format 3\x00"

// Paths 描述应用的完整文件系统布局。
// 所有路径在 New 时确定，运行期只读。
type Paths struct {
	// Root 是应用数据根目录。
	Root string

	// DB 是 SQLite 数据库文件路径。
	DB string
	// Runtime 存放运行时状态（如 sing-box 工作目录）。
	Runtime string
	// Cache 存放可再生的缓存数据（订阅原文、规则集等）。
	Cache string
	// Logs 存放程序与 sing-box 日志。
	Logs string
	// Config 是生成的 sing-box 配置文件路径。
	Config string
	// CoreBin 是 sing-box 二进制的存放路径。
	CoreBin string

	// RootWarning 非空时表示 KARING_HOME 指向了一个既有且与本应用布局
	// 无关的非空目录：应用可以使用它，但没有收紧该目录权限（C17 权限
	// 边界）。若其中已有的 runtime/cache/logs/runtime/bin 是真实目录，
	// 会在提示里写明「正在复用、权限保持不变」（V5-3）。调用方（TUI/CLI）
	// 可在合适的位置把这条提示展示给用户。
	RootWarning string
}

// DataRoot 返回当前平台的应用数据根目录：
//   - macOS:   ~/Library/Application Support/karing-tui
//   - Linux:   $XDG_DATA_HOME/karing-tui，缺省 ~/.local/share/karing-tui
//
// 环境变量 KARING_HOME 优先于平台默认值，便于测试与便携部署。
func DataRoot() (string, error) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return "", fmt.Errorf("不支持的平台 %q（仅支持 Linux 和 macOS）", runtime.GOOS)
	}
	if home := os.Getenv("KARING_HOME"); home != "" {
		return filepath.Abs(home)
	}

	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("获取用户主目录失败: %w", err)
		}
		return filepath.Join(home, "Library", "Application Support", AppName), nil
	case "linux":
		if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
			return filepath.Join(xdg, AppName), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("获取用户主目录失败: %w", err)
		}
		return filepath.Join(home, ".local", "share", AppName), nil
	default:
		// The supported-platform check above keeps this unreachable. Keep a
		// defensive error here so future changes cannot silently choose a path.
		return "", fmt.Errorf("不支持的平台 %q（仅支持 Linux 和 macOS）", runtime.GOOS)
	}
}

// rootSentinelFile 是根目录的归属标记：存在该文件表示目录由本应用创建，
// 或已被识别为历史安装目录，对它收紧权限是安全的。
const rootSentinelFile = ".karing-tui-root"

// NewPaths 基于 DataRoot 计算完整布局并创建所需目录。
//
// 顺序是硬约束（V5-3）：inspect → decide → modify。归属未确定之前不 chmod
// 任何既有目录、不写 sentinel、不创建任何应用子目录。
func NewPaths() (*Paths, error) {
	root, err := DataRoot()
	if err != nil {
		return nil, err
	}

	p := &Paths{
		Root:    root,
		DB:      filepath.Join(root, dbFileName),
		Runtime: filepath.Join(root, "runtime"),
		Cache:   filepath.Join(root, "cache"),
		Logs:    filepath.Join(root, "logs"),
		Config:  filepath.Join(root, "runtime", "config.json"),
		CoreBin: filepath.Join(root, "runtime", "bin", "sing-box"),
	}

	rootOwned, err := ensureRoot(root)
	if err != nil {
		return nil, err
	}

	var reused []string
	for _, dir := range []struct{ path, name string }{
		{p.Runtime, "runtime"},
		{p.Cache, "cache"},
		{p.Logs, "logs"},
		{filepath.Dir(p.CoreBin), "runtime/bin"},
	} {
		got, err := ensureAppDir(dir.path, rootOwned)
		if err != nil {
			return nil, err
		}
		if got {
			reused = append(reused, dir.name)
		}
	}

	if rootOwned {
		// These directories contain credentials, generated configuration, and
		// runtime state. Tighten existing installations as well as new ones.
		if err := os.Chmod(root, 0o700); err != nil {
			return nil, fmt.Errorf("设置目录 %s 权限失败: %w", root, err)
		}
	} else {
		p.RootWarning = foreignRootWarning(root, reused)
	}
	return p, nil
}

// foreignRootWarning 组装 foreign root 的提示：基础说明（没有收紧该目录
// 权限）加上「复用但不接管」的已有子目录清单，合并成一条，避免每个目录
// 各刷一行。
func foreignRootWarning(root string, reused []string) string {
	w := "数据目录 " + root + " 已存在且不像是本应用的目录，为避免影响其中内容，未修改该目录的权限。"
	if len(reused) == 0 {
		return w
	}
	return w + "正在复用其中已有的 " + strings.Join(reused, "、") + "，这些子目录的权限保持不变。"
}

// ensureAppDir 准备一个应用子目录（runtime / cache / logs / runtime/bin）。
//
//	不存在       → 创建 0700；这是应用本次创建的目录
//	symlink      → fail-closed，不跟随（否则会在链接目标里建目录甚至 chmod）
//	普通文件等   → fail-closed，不覆盖
//	owned 真实目录   → chmod 0700（应用自己的目录，收紧是安全的）
//	foreign 真实目录 → 允许复用，但**绝不 chmod**、不写 sentinel，
//	                   返回 reused=true 由调用方汇总成提示
//
// foreign 复用是兼容性要求而不是 ownership 转移：v0.4.0 已经会在用户指定
// 的 KARING_HOME 里创建 runtime/cache/logs/runtime/bin 却不写 sentinel，
// 若把「已存在真实目录」一律视为冲突，同一目录升级后的第二次启动就会自锁。
func ensureAppDir(dir string, owned bool) (bool, error) {
	fi, err := os.Lstat(dir)
	switch {
	case os.IsNotExist(err):
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return false, fmt.Errorf("创建目录 %s 失败: %w", dir, err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return false, fmt.Errorf("设置目录 %s 权限失败: %w", dir, err)
		}
		return false, nil
	case err != nil:
		return false, fmt.Errorf("检查目录 %s 失败: %w", dir, err)
	case fi.Mode()&os.ModeSymlink != 0:
		return false, fmt.Errorf("拒绝使用 %s：它是符号链接，本应用不跟随链接写入", dir)
	case !fi.IsDir():
		return false, fmt.Errorf("拒绝使用 %s：它不是目录", dir)
	case owned:
		if err := os.Chmod(dir, 0o700); err != nil {
			return false, fmt.Errorf("设置目录 %s 权限失败: %w", dir, err)
		}
		return false, nil
	default:
		return true, nil
	}
}

// ensureRoot 创建（或接管）应用根目录，返回是否可以对该 root 收紧权限并
// 写入归属标记。
//
// KARING_HOME 语义保持不变：指向哪里，哪里就是应用根目录。但权限收紧有
// 边界（C17 / V5-3）。只有下列情况才算「归属于本应用」：
//   - 本次新建的 root（写入归属标记）；
//   - 既有归属标记的 root；
//   - 空目录（视为预先建好的应用目录，写入归属标记）；
//   - 携带可信历史证据的 root：存在**真实、非 symlink、带 SQLite 文件头**
//     的 karing.db（老版本不写标记），识别后补写标记。
//
// 通用目录名（runtime/cache/logs）与 WAL sidecar（karing.db-wal/-shm）都
// **不构成归属证据**：它们太常见，曾经让 `mkdir -p /tmp/shared/cache` 就
// 把 /tmp/shared 认领并 chmod 0700。
//
// root 自身是符号链接时直接拒绝：不跟随、不 chmod、不写标记、不创建任何
// 子目录。否则后续 `root/runtime` 等创建仍会穿过链接写到外部目标。
func ensureRoot(root string) (bool, error) {
	lfi, err := os.Lstat(root)
	switch {
	case os.IsNotExist(err):
		if err := os.MkdirAll(root, 0o700); err != nil {
			return false, fmt.Errorf("创建目录 %s 失败: %w", root, err)
		}
		if err := writeRootSentinel(root); err != nil {
			return false, err
		}
		return true, nil
	case err != nil:
		return false, fmt.Errorf("检查目录 %s 失败: %w", root, err)
	case lfi.Mode()&os.ModeSymlink != 0:
		return false, fmt.Errorf("拒绝使用数据目录 %s：它是符号链接，本应用不跟随链接写入", root)
	case !lfi.IsDir():
		return false, fmt.Errorf("应用根目录 %s 不是目录", root)
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return false, fmt.Errorf("读取目录 %s 失败: %w", root, err)
	}
	if len(entries) == 0 {
		// 空目录：没有需要保护的内容，视为预建的应用目录接管。
		if err := writeRootSentinel(root); err != nil {
			return false, err
		}
		return true, nil
	}
	for _, e := range entries {
		// 只认真实文件：指向别处的 symlink 不能作为归属标记。
		if e.Name() == rootSentinelFile && e.Type().IsRegular() {
			return true, nil
		}
	}
	if legacyRootEvidence(root, entries) {
		// 老用户目录：历史上由应用创建，补写标记。
		if err := writeRootSentinel(root); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

// legacyRootEvidence 判断既有 root 是否携带可信的历史安装证据：
// 一个真实（非 symlink、非目录）且带 SQLite 文件头的 karing.db。
// 只有 -wal/-shm sidecar、或只有通用的 runtime/cache/logs 目录都不算。
func legacyRootEvidence(root string, entries []os.DirEntry) bool {
	for _, e := range entries {
		if e.Name() != dbFileName {
			continue
		}
		if !e.Type().IsRegular() {
			return false
		}
		return hasSQLiteHeader(filepath.Join(root, e.Name()))
	}
	return false
}

// hasSQLiteHeader 只读文件前 16 字节，排除「恰好叫 karing.db 的无关文件」
// 被当成历史安装证据。任何读取失败都视为无证据：那只会落到 foreign 分支
// （不修改用户目录权限，只多一条提示），不会误收紧权限。
func hasSQLiteHeader(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, len(sqliteHeader))
	if _, err := io.ReadFull(f, buf); err != nil {
		return false
	}
	return string(buf) == sqliteHeader
}

func writeRootSentinel(root string) error {
	path := filepath.Join(root, rootSentinelFile)
	if err := os.WriteFile(path, []byte("karing-tui application data root\n"), 0o600); err != nil {
		return fmt.Errorf("写入目录归属标记 %s 失败: %w", path, err)
	}
	return nil
}
