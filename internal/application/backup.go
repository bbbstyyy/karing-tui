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
//
// 执行顺序是硬约束（V5-1）：**先校验目标 → 再建 staging → 最后原子替换**。
// 任何失败路径都不得修改当前数据库，也不得破坏已有备份。
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
	// 校验必须先于任何破坏性动作：此时还没有 staging、没有 VACUUM INTO、
	// 没有临时 ZIP。见 validateBackupDestination 的判决性说明。
	if err := validateBackupDestination(a.Paths, dest); err != nil {
		return "", err
	}

	// 数据库一致性快照固定写应用私有 staging，路径与用户可见目标完全解耦。
	// V5-1 之前这里是 dest + ".snapshot.db"，等于让目标反向决定快照落点。
	staging, err := os.MkdirTemp(a.Paths.Runtime, ".backup-")
	if err != nil {
		return "", fmt.Errorf("创建快照临时目录失败: %w", err)
	}
	defer os.RemoveAll(staging)
	snapshot := filepath.Join(staging, "snapshot.db")
	if err := a.DB.VacuumInto(snapshot); err != nil {
		return "", err
	}
	if err := os.Chmod(snapshot, 0o600); err != nil {
		return "", fmt.Errorf("设置数据库快照权限失败: %w", err)
	}

	if err := writeBackupAtomically(dest, func(w io.Writer) error {
		return writeBackupArchive(w, snapshot, a.Paths.Config)
	}); err != nil {
		return "", err
	}
	a.logf("备份已导出: %s", dest)
	return dest, nil
}

// backupProtectedPaths 列出不得作为备份导出目标的关键运行文件。
func backupProtectedPaths(paths *platform.Paths) []string {
	return []string{
		paths.DB,
		paths.DB + "-wal",
		paths.DB + "-shm",
		paths.Config,
		paths.CoreBin,
	}
}

// backupOverlapError 是受保护目标命中时的统一错误文案。
func backupOverlapError(dest string) error {
	return fmt.Errorf("拒绝导出备份：目标路径与当前数据库或关键运行文件重叠: %s", dest)
}

// validateBackupDestination 在创建、截断或替换任何文件之前，拒绝与关键
// 运行文件重叠的导出目标。
//
// 判决性回归（旧实现实测）：`karing backup export <home>/karing.db` 曾在
// 数据库**处于打开状态**时成功返回 0，并把 karing.db 原地替换成 ZIP
// （破坏后首 16 字节为 "PK\x03\x04"），同时留下 -wal/-shm 残骸。
//
// 判定必须同时覆盖三类别名，缺一即可绕过：
//
//	Lstat 类型   —— 目标本身是 symlink 时直接拒绝，不跟随写入
//	路径别名     —— 解析父目录符号链接后做 Clean 归一化比较
//	inode 别名   —— Stat + os.SameFile，捕获 hardlink 与父目录 symlink 换名
func validateBackupDestination(paths *platform.Paths, dest string) error {
	if fi, err := os.Lstat(dest); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("拒绝导出备份：目标 %s 是符号链接，可能与当前数据库或关键运行文件重叠，不跟随写入", dest)
	}

	destNorm := normalizeForCompare(dest)
	var destInfo os.FileInfo
	if fi, err := os.Stat(dest); err == nil {
		destInfo = fi
	}
	for _, p := range backupProtectedPaths(paths) {
		if normalizeForCompare(p) == destNorm {
			return backupOverlapError(dest)
		}
		if destInfo == nil {
			continue
		}
		pInfo, err := os.Stat(p)
		if err != nil {
			continue
		}
		if os.SameFile(destInfo, pInfo) {
			return backupOverlapError(dest)
		}
	}
	return nil
}

// normalizeForCompare 归一化路径用于重叠比较：解析父目录的真实路径（跟随
// 符号链接）后拼回文件名，使 `link/karing.db` 与 `real/karing.db` 判定为
// 同一路径。父目录不存在时退回 filepath.Clean。
//
// 大小写不敏感文件系统（macOS/APFS 默认）上的同文件异名不在此处处理：
// 只要目标已存在，Lstat/Stat 就能看到真实文件，由 os.SameFile 兜住；
// 目标不存在时同名异写也不会命中受保护文件本身。
func normalizeForCompare(p string) string {
	p = filepath.Clean(p)
	resolved, err := filepath.EvalSymlinks(filepath.Dir(p))
	if err != nil {
		return p
	}
	return filepath.Join(resolved, filepath.Base(p))
}

// writeBackupAtomically 以「同目录临时文件 + 原子替换」的方式生成 dest。
//
// 顺序不可调换（沿用 app.go 的 config.json 原子写模式，并复用平台层
// replaceFile）：CreateTemp → Chmod 0600 → write → Sync → Close → replaceFile。
// 注意 Sync 必须在 fd 仍打开时执行，写不出 Close → Sync 这种顺序。
//
// write 通过 io.Writer 注入而不是 package global，失败时临时文件被删除且
// **不触碰 dest**，测试也不需要给 -race 增加共享可变状态。
func writeBackupAtomically(dest string, write func(io.Writer) error) error {
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".karing-backup-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时备份文件失败: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // 成功后已被 rename 走，此处失败无害
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("设置备份文件权限失败: %w", err)
	}
	if err := write(tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("同步备份文件失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭备份文件失败: %w", err)
	}
	if err := replaceFile(tmpPath, dest); err != nil {
		return fmt.Errorf("替换备份文件失败: %w", err)
	}
	return nil
}

// writeBackupArchive 打包数据库快照与配置文件（后者缺失时跳过）。
// 只负责「往 w 里写完整归档」，不关心落盘与替换。
func writeBackupArchive(w io.Writer, snapshotDB, configFile string) error {
	zw := zip.NewWriter(w)

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
	return nil
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
