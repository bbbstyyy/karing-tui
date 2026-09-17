// Package platform 处理 Unix 平台的数据目录定位与文件系统布局。
package platform

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// AppName 是应用在文件系统中使用的目录名。
const AppName = "karing-tui"

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
	// 边界）。调用方（TUI/CLI）可在合适的位置把这条提示展示给用户。
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
func NewPaths() (*Paths, error) {
	root, err := DataRoot()
	if err != nil {
		return nil, err
	}

	p := &Paths{
		Root:    root,
		DB:      filepath.Join(root, "karing.db"),
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
	if !rootOwned {
		p.RootWarning = "数据目录 " + root + " 已存在且不像是本应用的目录，为避免影响其中内容，未修改该目录的权限。"
	}

	for _, dir := range []string{p.Runtime, p.Cache, p.Logs, filepath.Dir(p.CoreBin)} {
		if err := ensurePrivateDir(dir); err != nil {
			return nil, err
		}
	}
	if rootOwned {
		// These directories contain credentials, generated configuration, and
		// runtime state. Tighten existing installations as well as new ones.
		if err := os.Chmod(root, 0o700); err != nil {
			return nil, fmt.Errorf("设置目录 %s 权限失败: %w", root, err)
		}
	}
	return p, nil
}

// ensurePrivateDir 创建应用自有的子目录并收紧到 0700。
// 这些目录名由应用定义（runtime/cache/logs/…），即使 KARING_HOME 指向了
// 一个无关目录，在其中新建这些子目录并收紧权限也是安全的。
func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建目录 %s 失败: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("设置目录 %s 权限失败: %w", dir, err)
	}
	return nil
}

// ensureRoot 创建（或接管）应用根目录，返回是否可对 root 执行 chmod 0700。
//
// KARING_HOME 语义保持不变：指向哪里，哪里就是应用根目录。但权限收紧有边界
// （C17）：只对下列目录收紧——
//   - 本次新建的 root（写入归属标记）；
//   - 已有归属标记的 root；
//   - 空目录（视为预先建好的应用目录，写入归属标记）；
//   - 符合历史安装布局的 root（老版本不写标记，按 karing.db / runtime/ 等
//     特征识别，识别后补写标记）。
//
// 其余既有非空目录（用户可能拿它复用）保持原权限，不动也不写标记。
func ensureRoot(root string) (bool, error) {
	info, statErr := os.Stat(root)
	switch {
	case os.IsNotExist(statErr):
		if err := os.MkdirAll(root, 0o700); err != nil {
			return false, fmt.Errorf("创建目录 %s 失败: %w", root, err)
		}
		if err := writeRootSentinel(root); err != nil {
			return false, err
		}
		return true, nil
	case statErr != nil:
		return false, fmt.Errorf("检查目录 %s 失败: %w", root, statErr)
	case !info.IsDir():
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
		if e.Name() == rootSentinelFile && !e.IsDir() {
			return true, nil
		}
	}
	if legacyRootLayout(entries) {
		// 老用户目录：历史上由应用创建（当时无条件 chmod 0700），补写标记。
		if err := writeRootSentinel(root); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

// legacyRootLayout 按历史安装布局的特征识别 root：
// 主库及其 WAL sidecar，或应用定义的子目录。
func legacyRootLayout(entries []os.DirEntry) bool {
	legacyNames := map[string]bool{
		"karing.db":     true,
		"karing.db-wal": true,
		"karing.db-shm": true,
		"runtime":       true,
		"cache":         true,
		"logs":          true,
	}
	for _, e := range entries {
		if legacyNames[e.Name()] {
			return true
		}
	}
	return false
}

func writeRootSentinel(root string) error {
	path := filepath.Join(root, rootSentinelFile)
	if err := os.WriteFile(path, []byte("karing-tui application data root\n"), 0o600); err != nil {
		return fmt.Errorf("写入目录归属标记 %s 失败: %w", path, err)
	}
	return nil
}
