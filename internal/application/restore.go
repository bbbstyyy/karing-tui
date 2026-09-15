package application

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/bbbstyyy/karing-tui/internal/platform"
	"github.com/bbbstyyy/karing-tui/internal/storage"
)

// PreparedRestore owns the exact, migrated database shown in the confirmation.
// Applying it never rereads the original archive, which may have since changed.
type PreparedRestore struct {
	dir, database        string
	Subscriptions, Nodes int
}

func (p *PreparedRestore) Close() error {
	if p == nil || p.dir == "" {
		return nil
	}
	err := os.RemoveAll(p.dir)
	if err == nil {
		p.dir = ""
		p.database = ""
	}
	return err
}

// PrepareRestore touches only a private staging directory on the same filesystem.
// The active database, core and listening ports are left available on every error.
func PrepareRestore(paths *platform.Paths, archive string) (_ *PreparedRestore, err error) {
	z, err := zip.OpenReader(archive)
	if err != nil {
		return nil, fmt.Errorf("打开备份归档失败: %w", err)
	}
	defer z.Close()
	var dbFile *zip.File
	for _, entry := range z.File {
		if entry.Name == "karing.db" {
			if dbFile != nil {
				return nil, fmt.Errorf("归档中重复的 karing.db")
			}
			dbFile = entry
		}
	}
	if dbFile == nil {
		return nil, fmt.Errorf("归档中缺少 karing.db")
	}
	if dbFile.UncompressedSize64 > maxRestoreDatabaseSize {
		return nil, fmt.Errorf("归档内数据库超过 %d MiB 上限", maxRestoreDatabaseSize>>20)
	}
	dir, err := os.MkdirTemp(filepath.Dir(paths.DB), ".restore-")
	if err != nil {
		return nil, err
	}
	prepared := &PreparedRestore{dir: dir, database: filepath.Join(dir, "karing.db")}
	defer func() {
		if err != nil {
			_ = prepared.Close()
		}
	}()
	src, err := dbFile.Open()
	if err != nil {
		return nil, err
	}
	defer src.Close()
	dst, err := os.OpenFile(prepared.database, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	n, copyErr := io.Copy(dst, io.LimitReader(src, maxRestoreDatabaseSize+1))
	closeErr := dst.Close()
	if copyErr != nil {
		return nil, fmt.Errorf("解压备份数据库失败: %w", copyErr)
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if n > maxRestoreDatabaseSize {
		return nil, fmt.Errorf("备份数据库超过大小上限")
	}
	if err := storage.CheckBackupFile(prepared.database); err != nil {
		return nil, fmt.Errorf("备份数据库预检失败: %w", err)
	}
	staged := *paths
	staged.DB, staged.Runtime = prepared.database, dir
	db, err := storage.Open(&staged)
	if err != nil {
		return nil, fmt.Errorf("备份迁移预检失败: %w", err)
	}
	defer db.Close()
	if err := db.IntegrityCheck(); err != nil {
		return nil, fmt.Errorf("备份完整性预检失败: %w", err)
	}
	// Read through the model APIs as well: a valid SQLite file can still have an
	// incomplete or incompatible application schema.
	if _, err := db.LoadSettings(); err != nil {
		return nil, err
	}
	if _, err := db.ListProxyGroups(); err != nil {
		return nil, err
	}
	if _, err := db.ListRoutingGroups(); err != nil {
		return nil, err
	}
	if _, err := db.ListRuleSets(); err != nil {
		return nil, err
	}
	if _, err := db.ListDNSServers(); err != nil {
		return nil, err
	}
	if _, err := db.ListDNSRules(); err != nil {
		return nil, err
	}
	subs, err := db.ListSubscriptions()
	if err != nil {
		return nil, err
	}
	nodes, err := db.ListNodes(0)
	if err != nil {
		return nil, err
	}
	prepared.Subscriptions, prepared.Nodes = len(subs), len(nodes)
	if err := db.Close(); err != nil {
		return nil, err
	}
	return prepared, nil
}

// Apply requires the current database to be closed. The caller owns instance
// locks and core coordination; RestorePrepared supplies that for a live App.
func (p *PreparedRestore) Apply(paths *platform.Paths) error {
	if p == nil || p.database == "" {
		return fmt.Errorf("恢复预检已失效，请重新选择备份")
	}
	backup := paths.DB + ".pre-restore"
	hadCurrent := false
	if _, err := os.Stat(paths.DB); err == nil {
		if err := copyFile(backup, paths.DB); err != nil {
			return fmt.Errorf("备份当前数据库失败: %w", err)
		}
		hadCurrent = true
	} else if !os.IsNotExist(err) {
		return err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(paths.DB + suffix); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.Rename(p.database, paths.DB); err != nil {
		return fmt.Errorf("替换数据库失败，原数据库保留: %w", err)
	}
	p.database = ""
	check, err := storage.Open(paths)
	if err == nil {
		err = check.IntegrityCheck()
		closeErr := check.Close()
		if err == nil {
			err = closeErr
		}
	}
	if err == nil {
		return nil
	}
	// No connection may retain the replacement's WAL when rolling back.
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Remove(paths.DB + suffix)
	}
	if hadCurrent {
		rollback := filepath.Join(p.dir, "rollback.db")
		if copyErr := copyFile(rollback, backup); copyErr != nil {
			return fmt.Errorf("恢复失败: %v；回滚失败: %w，原数据库保存在 %s", err, copyErr, backup)
		}
		if renameErr := os.Rename(rollback, paths.DB); renameErr != nil {
			return fmt.Errorf("恢复失败: %v；回滚失败: %w，原数据库保存在 %s", err, renameErr, backup)
		}
		return fmt.Errorf("恢复失败，已回滚原数据库: %w", err)
	}
	_ = os.Remove(paths.DB)
	return fmt.Errorf("恢复失败: %w", err)
}

// BeginOperation prevents restore from closing a database used by a background
// download/backup/apply or automatic update. Restore reports a busy operation
// rather than interrupting it or waiting on the TUI event loop.
func (a *App) BeginOperation() (func(), error) {
	a.operationMu.RLock()
	if a.restored {
		a.operationMu.RUnlock()
		return nil, fmt.Errorf("应用正在退出或已恢复，请重新启动")
	}
	return a.operationMu.RUnlock, nil
}

func (a *App) RestorePrepared(ctx context.Context, p *PreparedRestore) error {
	if p == nil {
		return fmt.Errorf("请先完成备份预检")
	}
	if !a.operationMu.TryLock() {
		return fmt.Errorf("后台任务仍在进行，请等待完成后再恢复；当前服务保持运行")
	}
	defer a.operationMu.Unlock()
	if err := a.DB.EnsureIdle(); err != nil {
		return err
	}
	wasRunning := a.Core.IsRunning()
	if err := a.StopCore(); err != nil {
		return err
	}
	if err := a.DB.Close(); err != nil {
		return err
	}
	if err := p.Apply(a.Paths); err != nil {
		if reopenErr := a.ReopenDB(); reopenErr != nil {
			return fmt.Errorf("%v；重新打开原数据库失败: %w", err, reopenErr)
		}
		if wasRunning {
			// Restart the exact previous generated config, including changes that
			// were saved but deliberately not applied before the attempted restore.
			if restartErr := a.resumePreviousCore(ctx); restartErr != nil {
				return fmt.Errorf("%v；原核心恢复运行失败: %w", err, restartErr)
			}
			return fmt.Errorf("%v；原核心已恢复运行", err)
		}
		return err
	}
	a.restored = true
	return nil
}

func (a *App) resumePreviousCore(ctx context.Context) error {
	a.configStateMu.RLock()
	previous := append([]byte(nil), a.runningConfig...)
	a.configStateMu.RUnlock()
	if len(previous) == 0 {
		return a.Core.Start(ctx, a.Paths.Config)
	}
	f, err := os.CreateTemp(a.Paths.Runtime, ".resume-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(previous); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return a.Core.Start(ctx, f.Name())
}
