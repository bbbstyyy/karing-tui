//go:build linux || darwin

package application

import "os"

// replaceFile 在 Unix 上用 rename 原子替换目标文件。
func replaceFile(src, dst string) error {
	return os.Rename(src, dst)
}
