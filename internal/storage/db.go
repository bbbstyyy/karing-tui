// Package storage 提供 SQLite 持久化：建表、schema 迁移与各领域对象的读写。
package storage

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动

	"github.com/bbbstyyy/karing-tui/internal/platform"
)

// DB 封装 SQLite 连接，提供迁移与各领域读写方法。
type DB struct {
	db    *sql.DB
	Paths *platform.Paths
}

// Open 打开（必要时创建）数据库并执行 schema 迁移。
func Open(paths *platform.Paths) (*DB, error) {
	// _time_format=sqlite 让时间类型按 SQLite 原生存储，便于跨驱动读取；
	// foreign_keys(1) 启用外键级联（SQLite 默认关闭）。
	dsn := (&url.URL{Scheme: "file", Path: paths.DB}).String() + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_time_format=sqlite"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库 %s 失败: %w", paths.DB, err)
	}
	// modernc.org/sqlite 建议限制为单写连接，避免 SQLITE_BUSY。
	sqlDB.SetMaxOpenConns(1)

	wrapped := &DB{db: sqlDB, Paths: paths}
	if err := wrapped.migrate(); err != nil {
		sqlDB.Close()
		return nil, err
	}
	if err := os.Chmod(paths.DB, 0o600); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("设置数据库文件权限失败: %w", err)
	}
	return wrapped, nil
}

// Close 关闭数据库连接。
func (d *DB) Close() error {
	return d.db.Close()
}

// VacuumInto 将当前数据库的一致性快照写入 path（SQLite VACUUM INTO，
// 目标文件不能已存在）；用于备份。
func (d *DB) VacuumInto(path string) error {
	escaped := strings.ReplaceAll(path, "'", "''")
	if _, err := d.db.Exec("VACUUM INTO '" + escaped + "'"); err != nil {
		return fmt.Errorf("创建数据库快照失败: %w", err)
	}
	return nil
}

// EnsureIdle 确认没有写事务，并将 WAL 完整写回主文件后清空。
// 活动读事务可能允许 BEGIN EXCLUSIVE，却阻止 WAL 清空；恢复前必须拒绝
// 这种情况，否则仅复制主文件会丢失尚在 WAL 中的已提交数据。
func (d *DB) EnsureIdle() error {
	if _, err := d.db.Exec("BEGIN EXCLUSIVE"); err != nil {
		return fmt.Errorf("数据库可能正被其他实例使用: %w", err)
	}
	if _, err := d.db.Exec("COMMIT"); err != nil {
		return fmt.Errorf("结束独占事务失败: %w", err)
	}
	var busy, frames, checkpointed int
	if err := d.db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &frames, &checkpointed); err != nil {
		return fmt.Errorf("恢复前同步数据库失败: %w", err)
	}
	if busy != 0 || (frames >= 0 && frames != checkpointed) {
		return fmt.Errorf("数据库仍有读取事务，无法完成恢复前同步；请稍后重试，当前服务保持运行")
	}
	return nil
}

// TableCount 返回表行数；table 仅限包内常量调用（不做注入防护）。
func (d *DB) TableCount(table string) (int, error) {
	var n int
	if err := d.db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM %s`, table)).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计表 %s 行数失败: %w", table, err)
	}
	return n, nil
}

// migrate 按 schema 版本（PRAGMA user_version）依次执行未应用的迁移。
func (d *DB) migrate() error {
	lock, err := acquireMigrationLock(d.Paths)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()

	// BEGIN IMMEDIATE obtains SQLite's RESERVED write lock before reading
	// user_version. This prevents two processes from both observing the same
	// old version and applying the same ALTER TABLE migration.
	conn, err := d.db.Conn(context.Background())
	if err != nil {
		return fmt.Errorf("获取迁移数据库连接失败: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("开启迁移写事务失败: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	var version int
	if err := conn.QueryRowContext(context.Background(), "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("读取 schema 版本失败: %w", err)
	}
	if version > len(migrations) {
		return fmt.Errorf("数据库版本 v%d 高于本程序支持的 v%d", version, len(migrations))
	}

	for i, m := range migrations {
		v := i + 1
		if v <= version {
			continue
		}
		for _, stmt := range m {
			if _, err := conn.ExecContext(context.Background(), stmt); err != nil {
				return fmt.Errorf("执行迁移 v%d 失败: %w", v, err)
			}
		}
		if _, err := conn.ExecContext(context.Background(), fmt.Sprintf("PRAGMA user_version = %d", v)); err != nil {
			return fmt.Errorf("写入 schema 版本 v%d 失败: %w", v, err)
		}
	}
	if _, err := conn.ExecContext(context.Background(), "COMMIT"); err != nil {
		return fmt.Errorf("提交 schema 迁移失败: %w", err)
	}
	committed = true
	return nil
}

// CheckBackupFile verifies that an archive contains a Karing database before
// migrations can initialize missing tables in an unrelated SQLite file.
func CheckBackupFile(path string) error {
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String()+"?mode=ro")
	if err != nil {
		return err
	}
	defer db.Close()
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version < 1 || version > len(migrations) {
		return fmt.Errorf("不支持的备份数据库版本 v%d", version)
	}
	for _, table := range []string{"subscriptions", "nodes", "proxy_groups", "routing_groups", "rules", "settings"} {
		var count int
		if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("备份数据库缺少 %s 表", table)
		}
	}
	return nil
}

// IntegrityCheck covers page corruption and references after staged migration.
func (d *DB) IntegrityCheck() error {
	var result string
	if err := d.db.QueryRow("PRAGMA integrity_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("数据库完整性检查失败: %s", result)
	}
	rows, err := d.db.Query("PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		return fmt.Errorf("数据库存在无效的外键引用")
	}
	return rows.Err()
}
