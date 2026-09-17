package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/platform"
)

// --- 辅助 ---

// probePaths 返回一个隔离的 KARING_HOME（数据库由各用例自行构造）。
func probePaths(t *testing.T) *platform.Paths {
	t.Helper()
	t.Setenv("KARING_HOME", t.TempDir())
	paths, err := platform.NewPaths()
	if err != nil {
		t.Fatalf("NewPaths: %v", err)
	}
	return paths
}

// rawOpen 用显式 DSN 参数打开原始连接，绕过 Open/OpenQueryOnly 的语义，
// 便于构造「库存在但版本不同 / 非 WAL」这类前置状态。
func rawOpen(t *testing.T, path, query string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", fileDSN(path, query))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// seedSyntheticDB 造一个最小库：schema 版本可控、含 DATETIME 列。
// 真实 schema 由 migration 建立；这里只关心「只读打开策略」的行为，故刻意最小化。
func seedSyntheticDB(t *testing.T, path string, version int, wal bool) {
	t.Helper()
	query := "_pragma=busy_timeout(5000)&_time_format=sqlite"
	if wal {
		query = writableDSNPragmas
	}
	db := rawOpen(t, path, query)
	if _, err := db.Exec("CREATE TABLE probe(x INTEGER, d DATETIME)"); err != nil {
		t.Fatalf("建表: %v", err)
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		t.Fatalf("写 user_version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关闭种子连接: %v", err)
	}
}

// --- P1 / P6：库不存在或为空时必须失败且不留文件 ---

func TestOpenQueryOnlyMissingDatabase(t *testing.T) {
	paths := probePaths(t)
	db, err := OpenQueryOnly(paths)
	if db != nil {
		t.Fatal("库不存在时不应返回可用句柄")
	}
	if !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("期望 ErrNotInitialized，得到 %v", err)
	}
	if !ReadOnlyUnavailable(err) {
		t.Fatal("未初始化必须被识别为「只读不可用」族")
	}
	if !strings.Contains(err.Error(), "主程序") {
		t.Fatalf("错误文案没有指向「先运行主程序」：%v", err)
	}
	// 不得留下主库文件，也不得留下 WAL sidecar（那会掩盖 KARING_HOME 指错路径）。
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, statErr := os.Stat(paths.DB + suffix); !os.IsNotExist(statErr) {
			t.Fatalf("只读打开在路径上产生了 %q", paths.DB+suffix)
		}
	}
}

func TestOpenQueryOnlyEmptyFile(t *testing.T) {
	paths := probePaths(t)
	if err := os.WriteFile(paths.DB, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenQueryOnly(paths); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("0 字节库期望 ErrNotInitialized，得到 %v", err)
	}
	if fi, err := os.Stat(paths.DB); err != nil || fi.Size() != 0 {
		t.Fatalf("0 字节库被改写了：fi=%v err=%v", fi, err)
	}
}

// --- P2：query-only 连接拒绝一切 SQL 写 ---

func TestQueryOnlyRejectsWrites(t *testing.T) {
	db := newTestDB(t)
	var before int
	if err := db.db.QueryRow("PRAGMA user_version").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec("CREATE TABLE probe_write(x INTEGER)"); err != nil {
		t.Fatal(err)
	}

	ro, err := OpenQueryOnly(db.Paths)
	if err != nil {
		t.Fatalf("OpenQueryOnly: %v", err)
	}
	defer ro.Close()

	for _, stmt := range []string{
		"INSERT INTO probe_write VALUES (1)",
		"UPDATE probe_write SET x = 2",
		"CREATE TABLE probe_ddl(y INTEGER)",
		"DROP TABLE probe_write",
		"PRAGMA user_version = 7",
	} {
		if _, err := ro.db.Exec(stmt); err == nil {
			t.Fatalf("query-only 连接竟然执行成功: %s", stmt)
		} else if !strings.Contains(err.Error(), "readonly") {
			t.Fatalf("%s 的失败原因不是只读限制：%v", stmt, err)
		}
	}

	var after int
	if err := ro.db.QueryRow("PRAGMA user_version").Scan(&after); err != nil {
		t.Fatalf("只读连接读 user_version: %v", err)
	}
	if after != before {
		t.Fatalf("只读期间 user_version 从 %d 变成 %d", before, after)
	}
	// 读路径本身必须可用（否则上面的「写被拒」可能只是因为连接根本没建起来）。
	var count int
	if err := ro.db.QueryRow("SELECT count(*) FROM probe_write").Scan(&count); err != nil {
		t.Fatalf("只读连接读取失败: %v", err)
	}
}

// --- P3：白名单不得把非 WAL 库改成 WAL ---

func TestQueryOnlyPreservesJournalMode(t *testing.T) {
	paths := probePaths(t)
	seedSyntheticDB(t, paths.DB, len(migrations), false)

	seed := rawOpen(t, paths.DB, "_pragma=busy_timeout(5000)&_time_format=sqlite")
	var before string
	if err := seed.QueryRow("PRAGMA journal_mode").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before == "wal" {
		t.Fatalf("前置状态错误：种子库已经是 %s", before)
	}

	ro, err := OpenQueryOnly(paths)
	if err != nil {
		t.Fatalf("OpenQueryOnly: %v", err)
	}
	defer ro.Close()
	if err := ro.db.QueryRow("SELECT count(*) FROM probe").Scan(new(int)); err != nil {
		t.Fatalf("只读查询: %v", err)
	}
	var after string
	if err := ro.db.QueryRow("PRAGMA journal_mode").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("只读连接把 journal_mode 从 %q 改成 %q", before, after)
	}
}

// --- P4：_time_format=sqlite 必须保留 ---

func TestQueryOnlyReadsDateTimeLikeWritable(t *testing.T) {
	paths := probePaths(t)
	seedSyntheticDB(t, paths.DB, len(migrations), false)

	writable := rawOpen(t, paths.DB, writableDSNPragmas)
	if _, err := writable.Exec("INSERT INTO probe(x, d) VALUES (1, '2026-09-17 03:20:52')"); err != nil {
		t.Fatal(err)
	}
	var want time.Time
	if err := writable.QueryRow("SELECT d FROM probe").Scan(&want); err != nil {
		t.Fatalf("可写连接读取 DATETIME: %v", err)
	}

	ro, err := OpenQueryOnly(paths)
	if err != nil {
		t.Fatalf("OpenQueryOnly: %v", err)
	}
	defer ro.Close()
	var got time.Time
	if err := ro.db.QueryRow("SELECT d FROM probe").Scan(&got); err != nil {
		t.Fatalf("只读连接读取 DATETIME: %v", err)
	}
	if !want.Equal(got) {
		t.Fatalf("DATETIME 读取行为不一致：可写 %v / 只读 %v", want, got)
	}
	// 反向核对：两边都退化成零值也能通过 Equal，这里钉住真实值。
	if want.Year() != 2026 || want.Hour() != 3 || want.Minute() != 20 {
		t.Fatalf("DATETIME 未被正确解析：%v（_time_format=sqlite 是否被移除？）", want)
	}
}

// --- P5：writer 并发 + query-only reader ---

func TestQueryOnlySeesWriterCommits(t *testing.T) {
	paths := probePaths(t)
	seedSyntheticDB(t, paths.DB, len(migrations), true)

	writer := rawOpen(t, paths.DB, writableDSNPragmas)
	ro, err := OpenQueryOnly(paths)
	if err != nil {
		t.Fatalf("OpenQueryOnly: %v", err)
	}
	defer ro.Close()

	if _, err := writer.Exec("INSERT INTO probe(x) VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := ro.db.QueryRow("SELECT count(*) FROM probe").Scan(&count); err != nil {
		t.Fatalf("只读连接读取: %v", err)
	}
	if count != 1 {
		t.Fatalf("只读连接读到 %d 行，期望 1（WAL 变更检测失效？）", count)
	}
	if _, err := writer.Exec("INSERT INTO probe(x) VALUES (2)"); err != nil {
		t.Fatal(err)
	}
	if err := ro.db.QueryRow("SELECT count(*) FROM probe").Scan(&count); err != nil {
		t.Fatalf("只读连接二次读取: %v", err)
	}
	if count != 2 {
		t.Fatalf("只读连接读到 %d 行，期望 2（陈旧快照？）", count)
	}
}

// --- P7：冷 WAL + 只读目录必须给出专门的错误 ---

func TestOpenQueryOnlyReadOnlyDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("以 root 运行时目录权限不构成限制，无法构造该场景")
	}
	dir := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	paths := probePaths(t)
	paths.DB = filepath.Join(dir, "karing.db")
	seedSyntheticDB(t, paths.DB, len(migrations), true)

	// 干净关闭后不应残留 sidecar——否则模式 ro 仍能打开，用例会失去意义。
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(paths.DB + suffix); !os.IsNotExist(err) {
			t.Fatalf("前置状态错误：残留 %s", paths.DB+suffix)
		}
	}
	if err := os.Chmod(paths.DB, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	_, err := OpenQueryOnly(paths)
	if !errors.Is(err, ErrDataDirNotWritable) {
		t.Fatalf("期望 ErrDataDirNotWritable，得到 %v", err)
	}
	if errors.Is(err, ErrNotInitialized) {
		t.Fatal("目录不可写被误判成「数据库未初始化」（库是好的，问题在目录权限）")
	}
	if !strings.Contains(err.Error(), filepath.Base(dir)) {
		t.Fatalf("错误文案没有指出目录：%v", err)
	}
}

// --- P8：old / new schema 的分类与文案 ---

func TestOpenQueryOnlySchemaVersionErrors(t *testing.T) {
	supported := len(migrations)
	if supported < 2 {
		t.Skip("迁移数不足以构造「旧于程序」的版本")
	}
	cases := []struct {
		name    string
		version int
		want    error
		hint    string
	}{
		{"旧 schema", supported - 1, ErrSchemaTooOld, "主程序"},
		{"新 schema", supported + 1, ErrSchemaTooNew, "升级"},
	}
	var seen []error
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			paths := probePaths(t)
			seedSyntheticDB(t, paths.DB, tc.version, false)
			_, err := OpenQueryOnly(paths)
			if !errors.Is(err, tc.want) {
				t.Fatalf("期望 %v，得到 %v", tc.want, err)
			}
			if errors.Is(err, ErrNotInitialized) {
				t.Fatalf("版本不匹配被误判成「未初始化」：%v", err)
			}
			if !strings.Contains(err.Error(), tc.hint) {
				t.Fatalf("文案缺少指引 %q：%v", tc.hint, err)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("v%d", tc.version)) {
				t.Fatalf("文案没有报出实际版本 v%d：%v", tc.version, err)
			}
			seen = append(seen, tc.want)
		})
	}
	if len(seen) == 2 && errors.Is(fmt.Errorf("%w", seen[0]), seen[1]) {
		t.Fatal("schema 过旧与过新必须是两个可区分的错误")
	}
}

// --- P9：query-only DSN 白名单 ---

func TestQueryOnlyDSNWhitelist(t *testing.T) {
	// 允许的参数名（键），以及 _pragma 里允许的 pragma 名。
	allowedKeys := map[string]bool{"mode": true, "_query_only": true, "_pragma": true, "_time_format": true}
	allowedPragmas := map[string]bool{"busy_timeout": true, "foreign_keys": true}

	for _, required := range []string{"mode=ro", "_query_only=1", "_time_format=sqlite"} {
		if !strings.Contains(queryOnlyDSNPragmas, required) {
			t.Fatalf("query-only DSN 缺少必需参数 %q（%s）", required, queryOnlyDSNPragmas)
		}
	}
	// 逐项列举，而不是「从可写 DSN 复制后删危险项」：可写侧新增 pragma 时不得自动继承。
	for _, kv := range strings.Split(queryOnlyDSNPragmas, "&") {
		key, value, ok := strings.Cut(kv, "=")
		if !ok || !allowedKeys[key] {
			t.Fatalf("query-only DSN 出现未列入白名单的参数 %q", kv)
		}
		if key != "_pragma" {
			continue
		}
		name, _, _ := strings.Cut(value, "(")
		if !allowedPragmas[name] {
			t.Fatalf("query-only DSN 出现未列入白名单的 pragma %q", name)
		}
	}
	// 会在 connection init 阶段持久改写数据库的参数一律禁止。
	for _, banned := range []string{"journal_mode", "auto_vacuum", "immutable", "mode=rw"} {
		if strings.Contains(queryOnlyDSNPragmas, banned) {
			t.Fatalf("query-only DSN 含被禁参数 %q：%s", banned, queryOnlyDSNPragmas)
		}
	}
	// 反向核对：可写 DSN 确实带 journal_mode(WAL)，证明上面那条否定断言不是空转。
	if !strings.Contains(writableDSNPragmas, "journal_mode(WAL)") {
		t.Fatal("可写 DSN 的 journal_mode 不见了，白名单断言失去对照")
	}
}
