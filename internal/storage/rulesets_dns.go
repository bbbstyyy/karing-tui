package storage

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/config"
)

// --- 规则集 ---

// CreateRuleSet 新建规则集。
func (d *DB) CreateRuleSet(rs *config.RuleSet) error {
	return d.createRuleSet(d.db, rs)
}

// CreateRuleSetTx 是 CreateRuleSet 的事务内版本（C14-AUDIT）。
func (d *DB) CreateRuleSetTx(tx *sql.Tx, rs *config.RuleSet) error {
	return d.createRuleSet(tx, rs)
}

func (d *DB) createRuleSet(q querier, rs *config.RuleSet) error {
	if err := validateRuleSetTag(rs.Tag); err != nil {
		return err
	}
	now := time.Now()
	res, err := q.Exec(
		`INSERT INTO rulesets (name, tag, source_type, format, url, enabled, cached_path, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		rs.Name, rs.Tag, rs.SourceType, rs.Format, rs.URL, boolInt(rs.Enabled), rs.CachedPath, now,
	)
	if err != nil {
		return fmt.Errorf("创建规则集 %q 失败: %w", rs.Name, err)
	}
	if rs.ID, err = res.LastInsertId(); err != nil {
		return fmt.Errorf("获取规则集自增 ID 失败: %w", err)
	}
	rs.UpdatedAt = now
	return nil
}

// UpdateRuleSet 更新规则集。
func (d *DB) UpdateRuleSet(rs *config.RuleSet) error {
	if err := validateRuleSetTag(rs.Tag); err != nil {
		return err
	}
	_, err := d.db.Exec(
		`UPDATE rulesets SET name=?, tag=?, source_type=?, format=?, url=?, enabled=?, cached_path=?, updated_at=? WHERE id=?`,
		rs.Name, rs.Tag, rs.SourceType, rs.Format, rs.URL, boolInt(rs.Enabled), rs.CachedPath, time.Now(), rs.ID,
	)
	if err != nil {
		return fmt.Errorf("更新规则集 %d 失败: %w", rs.ID, err)
	}
	return nil
}

func validateRuleSetTag(tag string) error {
	tag = strings.TrimSpace(tag)
	if tag == "" || tag == "." || tag == ".." || strings.ContainsAny(tag, "/\\:\x00") {
		return fmt.Errorf("规则集 Tag 非法：不得为空或包含路径分隔符、冒号和 \"..\"")
	}
	return nil
}

// DeleteRuleSet 删除规则集。
func (d *DB) DeleteRuleSet(id int64) error {
	_, err := d.db.Exec(`DELETE FROM rulesets WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("删除规则集 %d 失败: %w", id, err)
	}
	return nil
}

// ListRuleSets 返回全部规则集，按名称排序。
func (d *DB) ListRuleSets() ([]*config.RuleSet, error) {
	return d.listRuleSets(d.db)
}

// ListRuleSetsTx 是 ListRuleSets 的事务内版本（C14-AUDIT）。
func (d *DB) ListRuleSetsTx(tx *sql.Tx) ([]*config.RuleSet, error) {
	return d.listRuleSets(tx)
}

func (d *DB) listRuleSets(q querier) ([]*config.RuleSet, error) {
	rows, err := q.Query(
		`SELECT id, name, tag, source_type, format, url, enabled, cached_path, updated_at FROM rulesets ORDER BY name`,
	)
	if err != nil {
		return nil, fmt.Errorf("查询规则集列表失败: %w", err)
	}
	defer rows.Close()

	var out []*config.RuleSet
	for rows.Next() {
		var rs config.RuleSet
		var enabled int
		if err := rows.Scan(&rs.ID, &rs.Name, &rs.Tag, &rs.SourceType, &rs.Format, &rs.URL, &enabled, &rs.CachedPath, &rs.UpdatedAt); err != nil {
			return nil, fmt.Errorf("扫描规则集失败: %w", err)
		}
		rs.Enabled = enabled != 0
		out = append(out, &rs)
	}
	return out, rows.Err()
}

// --- DNS ---

// CreateDNSServer 新建 DNS 服务器条目。
func (d *DB) CreateDNSServer(s *config.DNSServer) error {
	return d.createDNSServer(d.db, s)
}

// CreateDNSServerTx 是 CreateDNSServer 的事务内版本（V7-3）。
func (d *DB) CreateDNSServerTx(tx *sql.Tx, s *config.DNSServer) error {
	return d.createDNSServer(tx, s)
}

func (d *DB) createDNSServer(q querier, s *config.DNSServer) error {
	if s.Type == "" {
		s.Type = "udp"
	}
	res, err := q.Exec(
		`INSERT INTO dns_servers (tag, type, address, address_resolver, detour, enabled, position) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		s.Tag, s.Type, s.Address, s.AddressResolver, s.Detour, boolInt(s.Enabled), s.Position,
	)
	if err != nil {
		return fmt.Errorf("创建 DNS 服务器 %q 失败: %w", s.Tag, err)
	}
	if s.ID, err = res.LastInsertId(); err != nil {
		return fmt.Errorf("获取 DNS 服务器自增 ID 失败: %w", err)
	}
	return nil
}

// UpdateDNSServer 更新 DNS 服务器条目。
func (d *DB) UpdateDNSServer(s *config.DNSServer) error {
	return d.updateDNSServer(d.db, s)
}

// UpdateDNSServerTx 是 UpdateDNSServer 的事务内版本（V7-3）。
func (d *DB) UpdateDNSServerTx(tx *sql.Tx, s *config.DNSServer) error {
	return d.updateDNSServer(tx, s)
}

func (d *DB) updateDNSServer(q querier, s *config.DNSServer) error {
	_, err := q.Exec(
		`UPDATE dns_servers SET tag=?, type=?, address=?, address_resolver=?, detour=?, enabled=?, position=? WHERE id=?`,
		s.Tag, s.Type, s.Address, s.AddressResolver, s.Detour, boolInt(s.Enabled), s.Position, s.ID,
	)
	if err != nil {
		return fmt.Errorf("更新 DNS 服务器 %d 失败: %w", s.ID, err)
	}
	return nil
}

// UpdateDNSServerRenamed updates a DNS server and all persisted references atomically.
func (d *DB) UpdateDNSServerRenamed(s *config.DNSServer, oldTag string) error {
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("开启 DNS 更新事务失败: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`UPDATE dns_servers SET tag=?, type=?, address=?, address_resolver=?, detour=?, enabled=?, position=? WHERE id=?`,
		s.Tag, s.Type, s.Address, s.AddressResolver, s.Detour, boolInt(s.Enabled), s.Position, s.ID,
	); err != nil {
		return fmt.Errorf("更新 DNS 服务器 %d 失败: %w", s.ID, err)
	}
	if _, err := tx.Exec(`UPDATE dns_rules SET server=? WHERE server=?`, s.Tag, oldTag); err != nil {
		return fmt.Errorf("更新 DNS 规则引用失败: %w", err)
	}
	if _, err := tx.Exec(`UPDATE dns_servers SET address_resolver=? WHERE address_resolver=?`, s.Tag, oldTag); err != nil {
		return fmt.Errorf("更新 DNS 解析器引用失败: %w", err)
	}
	if _, err := tx.Exec(`UPDATE settings SET value=?, updated_at=? WHERE key='dns_final' AND value=?`, s.Tag, time.Now(), oldTag); err != nil {
		return fmt.Errorf("更新 DNS 默认引用失败: %w", err)
	}
	return tx.Commit()
}

// DeleteDNSServer 删除 DNS 服务器条目。
func (d *DB) DeleteDNSServer(id int64) error {
	_, err := d.db.Exec(`DELETE FROM dns_servers WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("删除 DNS 服务器 %d 失败: %w", id, err)
	}
	return nil
}

// ListDNSServers 返回全部 DNS 服务器条目，按 position、id 排序。
func (d *DB) ListDNSServers() ([]*config.DNSServer, error) {
	return d.listDNSServers(d.db)
}

// ListDNSServersTx 是 ListDNSServers 的事务内版本（V7-3）：
// 事务内必须读到自己刚写的行，不能经 d.db 另取连接（C14：单连接池会自锁）。
func (d *DB) ListDNSServersTx(tx *sql.Tx) ([]*config.DNSServer, error) {
	return d.listDNSServers(tx)
}

func (d *DB) listDNSServers(q querier) ([]*config.DNSServer, error) {
	rows, err := q.Query(
		`SELECT id, tag, type, address, address_resolver, detour, enabled, position FROM dns_servers ORDER BY position, id`,
	)
	if err != nil {
		return nil, fmt.Errorf("查询 DNS 服务器列表失败: %w", err)
	}
	defer rows.Close()

	var out []*config.DNSServer
	for rows.Next() {
		var s config.DNSServer
		var enabled int
		if err := rows.Scan(&s.ID, &s.Tag, &s.Type, &s.Address, &s.AddressResolver, &s.Detour, &enabled, &s.Position); err != nil {
			return nil, fmt.Errorf("扫描 DNS 服务器失败: %w", err)
		}
		s.Enabled = enabled != 0
		out = append(out, &s)
	}
	return out, rows.Err()
}

// --- DNS 规则 ---

// CreateDNSRule 新建 DNS 规则。
func (d *DB) CreateDNSRule(r *config.DNSRule) error {
	return d.createDNSRule(d.db, r)
}

// CreateDNSRuleTx 是 CreateDNSRule 的事务内版本（V7-3）。
func (d *DB) CreateDNSRuleTx(tx *sql.Tx, r *config.DNSRule) error {
	return d.createDNSRule(tx, r)
}

func (d *DB) createDNSRule(q querier, r *config.DNSRule) error {
	res, err := q.Exec(
		`INSERT INTO dns_rules (rule_type, value, server, enabled, position) VALUES (?, ?, ?, ?, ?)`,
		r.Type, r.Value, r.Server, boolInt(r.Enabled), r.Position,
	)
	if err != nil {
		return fmt.Errorf("创建 DNS 规则失败: %w", err)
	}
	if r.ID, err = res.LastInsertId(); err != nil {
		return fmt.Errorf("获取 DNS 规则自增 ID 失败: %w", err)
	}
	return nil
}

// UpdateDNSRule 更新 DNS 规则。
func (d *DB) UpdateDNSRule(r *config.DNSRule) error {
	return d.updateDNSRule(d.db, r)
}

// UpdateDNSRuleTx 是 UpdateDNSRule 的事务内版本（V7-3）。
func (d *DB) UpdateDNSRuleTx(tx *sql.Tx, r *config.DNSRule) error {
	return d.updateDNSRule(tx, r)
}

func (d *DB) updateDNSRule(q querier, r *config.DNSRule) error {
	_, err := q.Exec(
		`UPDATE dns_rules SET rule_type=?, value=?, server=?, enabled=?, position=? WHERE id=?`,
		r.Type, r.Value, r.Server, boolInt(r.Enabled), r.Position, r.ID,
	)
	if err != nil {
		return fmt.Errorf("更新 DNS 规则 %d 失败: %w", r.ID, err)
	}
	return nil
}

// DeleteDNSRule 删除 DNS 规则。
func (d *DB) DeleteDNSRule(id int64) error {
	_, err := d.db.Exec(`DELETE FROM dns_rules WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("删除 DNS 规则 %d 失败: %w", id, err)
	}
	return nil
}

// ListDNSRules 返回全部 DNS 规则，按 position、id 排序。
func (d *DB) ListDNSRules() ([]*config.DNSRule, error) {
	return d.listDNSRules(d.db)
}

// ListDNSRulesTx 是 ListDNSRules 的事务内版本（V7-3）。
func (d *DB) ListDNSRulesTx(tx *sql.Tx) ([]*config.DNSRule, error) {
	return d.listDNSRules(tx)
}

func (d *DB) listDNSRules(q querier) ([]*config.DNSRule, error) {
	rows, err := q.Query(
		`SELECT id, rule_type, value, server, enabled, position FROM dns_rules ORDER BY position, id`,
	)
	if err != nil {
		return nil, fmt.Errorf("查询 DNS 规则列表失败: %w", err)
	}
	defer rows.Close()

	var out []*config.DNSRule
	for rows.Next() {
		var r config.DNSRule
		var enabled int
		if err := rows.Scan(&r.ID, &r.Type, &r.Value, &r.Server, &enabled, &r.Position); err != nil {
			return nil, fmt.Errorf("扫描 DNS 规则失败: %w", err)
		}
		r.Enabled = enabled != 0
		out = append(out, &r)
	}
	return out, rows.Err()
}

// --- settings 帮助函数 ---

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}
