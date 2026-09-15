// 备份/恢复：数据库一致性快照与生成的 sing-box 配置打包导出、从归档恢复。
package application

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/platform"
)

const maxRestoreDatabaseSize = 512 << 20 // 512 MiB；防止恶意归档耗尽磁盘。

// BackupDirName 备份默认目录名（位于数据根目录下）。
const BackupDirName = "backups"

// Backup 将数据库一致性快照（VACUUM INTO）与生成的 sing-box 配置打包为 zip。
// dest 为空时写入 <数据根>/backups/karing-backup-<时间戳>.zip；返回实际路径。
func (a *App) Backup(dest string) (string, error) {
	if dest == "" {
		dir := filepath.Join(a.Paths.Root, BackupDirName)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", fmt.Errorf("创建备份目录失败: %w", err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return "", fmt.Errorf("设置备份目录权限失败: %w", err)
		}
		dest = filepath.Join(dir, "karing-backup-"+time.Now().Format("20060102-150405")+".zip")
	}
	if !filepath.IsAbs(dest) {
		abs, err := filepath.Abs(dest)
		if err != nil {
			return "", err
		}
		dest = abs
	}

	// 数据库一致性快照（VACUUM INTO 要求目标文件不存在）
	snapshot := dest + ".snapshot.db"
	_ = os.Remove(snapshot)
	if err := a.DB.VacuumInto(snapshot); err != nil {
		return "", err
	}
	defer os.Remove(snapshot)
	if err := os.Chmod(snapshot, 0o600); err != nil {
		return "", fmt.Errorf("设置数据库快照权限失败: %w", err)
	}

	if err := writeBackupZip(dest, snapshot, a.Paths.Config); err != nil {
		return "", err
	}
	a.logf("备份已导出: %s", dest)
	return dest, nil
}

// writeBackupZip 打包数据库快照与配置文件（后者缺失时跳过）。
func writeBackupZip(dest, snapshotDB, configFile string) error {
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("创建备份文件失败: %w", err)
	}
	defer out.Close()
	if err := out.Chmod(0o600); err != nil {
		return fmt.Errorf("设置备份文件权限失败: %w", err)
	}
	zw := zip.NewWriter(out)

	add := func(name, path string) error {
		f, err := zw.Create(name)
		if err != nil {
			return err
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		defer src.Close()
		_, err = io.Copy(f, src)
		return err
	}
	if err := add("karing.db", snapshotDB); err != nil {
		return fmt.Errorf("写入数据库快照失败: %w", err)
	}
	if _, err := os.Stat(configFile); err == nil {
		if err := add("config.json", configFile); err != nil {
			return fmt.Errorf("写入配置文件失败: %w", err)
		}
	}
	meta := fmt.Sprintf("karing-tui backup\ncreated: %s\n", time.Now().Format(time.RFC3339))
	if f, err := zw.Create("meta.txt"); err == nil {
		_, _ = f.Write([]byte(meta))
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("完成备份归档失败: %w", err)
	}
	return out.Close()
}

// RestoreArchive 从备份归档恢复数据库：覆盖 karing.db 并清除 wal/shm。
// 当前数据库先备份为 karing.db.pre-restore；恢复后立即打开校验（含迁移），
// 失败时回滚。要求调用前已停止核心并关闭数据库（无其他实例占用）。
func RestoreArchive(paths *platform.Paths, archive string) error {
	prepared, err := PrepareRestore(paths, archive)
	if err != nil {
		return err
	}
	defer prepared.Close()
	return prepared.Apply(paths)
}

func copyFile(dest, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	return out.Close()
}
