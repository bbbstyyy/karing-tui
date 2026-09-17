// Package storage 提供 SQLite 持久化：建表、schema 迁移与各领域对象的读写。
package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动

	"github.com/bbbstyyy/karing-tui/internal/platform"
)

// 只读（query-only）打开策略。
//
// 术语：这里的「只读」指**应用/SQL 逻辑只读 + 不创建主数据库文件**，不是
// 「文件系统绝对零写入」——SQLite 的 WAL reader 仍可能需要在数据目录创建或
// 维护 -shm/-wal sidecar，这不属于应用的业务写操作。
var (
	// ErrNotInitialized 表示数据库不存在、为 0 字节，或从未被主程序迁移过。
	ErrNotInitialized = errors.New("数据库尚未初始化")
	// ErrSchemaTooOld 表示数据库 schema 版本低于本程序支持的版本。
	ErrSchemaTooOld = errors.New("数据库 schema 版本较旧")
	// ErrSchemaTooNew 表示数据库 schema 版本高于本程序支持的版本。
	ErrSchemaTooNew = errors.New("数据库 schema 版本较新")
	// ErrDataDirNotWritable 表示数据目录不允许 SQLite 创建 sidecar/锁文件。
	ErrDataDirNotWritable = errors.New("数据目录不可写")
)

// ReadOnlyUnavailable 报告错误是否属于「只读打开不可用」族：未初始化、
// schema 版本不匹配、数据目录不可写。这些错误的文案本身就是可操作指引，
// 调用方应原样呈现，不要套「初始化失败」之类的通用前缀。
func ReadOnlyUnavailable(err error) bool {
	return errors.Is(err, ErrNotInitialized) || errors.Is(err, ErrSchemaTooOld) ||
		errors.Is(err, ErrSchemaTooNew) || errors.Is(err, ErrDataDirNotWritable)
}

// writableDSNPragmas 可写连接的 DSN 参数。
//
// _time_format=sqlite 让时间类型按 SQLite 原生存储，便于跨驱动读取；
// foreign_keys(1) 启用外键级联（SQLite 默认关闭）。
const writableDSNPragmas = "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_time_format=sqlite"

// queryOnlyDSNPragmas 只读连接的 DSN 参数**白名单**。
//
// 显式列举，而不是「复制可写 DSN 再删掉危险项」：可写侧以后新增 pragma 时不得
// 自动继承到这里。实测（探针记录见 CHECKLIST-v4 的 C13 节）`_pragma=journal_mode(WAL)`
// 在 `_query_only` 生效**之前**执行，能把非 WAL 库持久改成 WAL——只读连接会因此
// 留下一条真实的写入路径。
//
//	mode=ro              打开阶段不允许创建数据库文件（库不存在即失败）。
//	                     只加 _query_only 时，库不存在会被 SQLite 创建成 0 字节文件，
//	                     从而掩盖「KARING_HOME 指错路径」这类问题。
//	_query_only=1        连接建立后拒绝 INSERT/UPDATE/DDL/`PRAGMA user_version=`
//	busy_timeout(5000)   与可写连接一致的锁等待行为
//	foreign_keys(1)      与可写连接一致的外键语义
//	_time_format=sqlite  **必须保留**，DATETIME 列的扫描行为依赖它
//
// 禁止出现 `_pragma=journal_mode(...)` / `_auto_vacuum` 以及任何会在 connection
// init 阶段改写数据库文件/header/schema 的参数。
const queryOnlyDSNPragmas = "mode=ro&_query_only=1&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_time_format=sqlite"

// fileDSN 拼出 file: DSN。query 由调用方以上述常量显式给出（见白名单注释）。
func fileDSN(path, query string) string {
	return (&url.URL{Scheme: "file", Path: path}).String() + "?" + query
}

// DB 封装 SQLite 连接，提供迁移与各领域读写方法。
type DB struct {
	db    *sql.DB
	Paths *platform.Paths
}

// Open 打开（必要时创建）数据库并执行 schema 迁移。
func Open(paths *platform.Paths) (*DB, error) {
	sqlDB, err := sql.Open("sqlite", fileDSN(paths.DB, writableDSNPragmas))
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

// OpenQueryOnly 以只读方式打开**既有**数据库，供 CLI 只读命令使用。
//
// 与 Open 的差别：不迁移 schema、不写 user_version、不 chmod、不补默认数据，
// 且数据库不存在时打开失败而不是创建它（ErrNotInitialized）。
func OpenQueryOnly(paths *platform.Paths) (*DB, error) {
	sqlDB, err := sql.Open("sqlite", fileDSN(paths.DB, queryOnlyDSNPragmas))
	if err != nil {
		return nil, fmt.Errorf("打开数据库 %s 失败: %w", paths.DB, err)
	}
	// 与可写连接一致：单连接，避免 SQLITE_BUSY。
	sqlDB.SetMaxOpenConns(1)
	wrapped := &DB{db: sqlDB, Paths: paths}
	if err := wrapped.checkQueryOnlySchema(); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	return wrapped, nil
}

// checkQueryOnlySchema 在不写库的前提下确认 schema 版本与本程序一致。
//
// 严格匹配是刻意的：只读连接不能自己迁移，遇到旧 schema 必须让用户先跑主程序，
// 否则会读到迁移中途的表结构。
func (d *DB) checkQueryOnlySchema() error {
	var version int
	if err := d.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return classifyQueryOnlyOpenError(d.Paths.DB, err)
	}
	supported := len(migrations)
	switch {
	case version == 0:
		// 0 字节文件读出来也是 0：两种情况都等价于「主程序还没初始化过」。
		return fmt.Errorf("%w（%s）：请先运行 karing-tui 主程序完成初始化", ErrNotInitialized, d.Paths.DB)
	case version < supported:
		return fmt.Errorf("%w（数据库 v%d、程序支持 v%d）：请先运行 karing-tui 主程序完成迁移，再执行只读命令",
			ErrSchemaTooOld, version, supported)
	case version > supported:
		return fmt.Errorf("%w（数据库 v%d、程序支持 v%d）：请升级 karing-tui 后再执行只读命令",
			ErrSchemaTooNew, version, supported)
	}
	return nil
}

// classifyQueryOnlyOpenError 把驱动的打开期错误翻译成可操作的错误。
//
// 只读连接取不到连接有两类原因，处置完全不同：
//   - 库不存在（mode=ro 不创建文件）："unable to open database file (14)"；
//   - 库存在但数据目录不可写，SQLite 无法为 WAL reader 创建 -shm/-wal
//     sidecar：SQLITE_READONLY_DIRECTORY (1544)。
//
// 第二类**不得**归类成 ErrNotInitialized——库本身是好的，问题在目录权限。
func classifyQueryOnlyOpenError(path string, err error) error {
	switch {
	case strings.Contains(err.Error(), "unable to open database file"):
		return fmt.Errorf("%w（%s 不存在）：请先运行 karing-tui 主程序完成初始化", ErrNotInitialized, path)
	case strings.Contains(err.Error(), "1544"):
		return fmt.Errorf("%w（%s）：SQLite 的 WAL reader 需要在同一目录创建 -shm/-wal，请给该目录写权限",
			ErrDataDirNotWritable, filepath.Dir(path))
	}
	return fmt.Errorf("以只读方式打开数据库 %s 失败: %w", path, err)
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
		// 少数迁移除 DDL 之外还需要 Go 侧判定（如分流组分层的存量回填）。
		// 仍在该事务内执行，失败即整体回滚，不会留下半套 schema。
		if post := postMigrations[v]; post != nil {
			if err := post(context.Background(), conn); err != nil {
				return fmt.Errorf("执行迁移 v%d 回填失败: %w", v, err)
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
