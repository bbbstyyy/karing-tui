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

	for _, dir := range []string{root, p.Runtime, p.Cache, p.Logs, filepath.Dir(p.CoreBin)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("创建目录 %s 失败: %w", dir, err)
		}
		// These directories contain credentials, generated configuration, and
		// runtime state. Tighten existing installations as well as new ones.
		if err := os.Chmod(dir, 0o700); err != nil {
			return nil, fmt.Errorf("设置目录 %s 权限失败: %w", dir, err)
		}
	}
	return p, nil
}
