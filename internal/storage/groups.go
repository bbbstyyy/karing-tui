package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/config"
)

// CreateProxyGroup 新建代理组（含成员）。
func (d *DB) CreateProxyGroup(g *config.ProxyGroup) error {
	now := time.Now()
	res, err := d.db.Exec(
		`INSERT INTO proxy_groups (name, type, test_url, interval_s, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		g.Name, g.Type, g.TestURL, g.IntervalS, now, now,
	)
	if err != nil {
		return fmt.Errorf("创建代理组 %q 失败: %w", g.Name, err)
	}
	if g.ID, err = res.LastInsertId(); err != nil {
		return fmt.Errorf("获取代理组自增 ID 失败: %w", err)
	}
	g.CreatedAt, g.UpdatedAt = now, now
	return d.replaceGroupMembers(g)
}

// UpdateProxyGroup 更新代理组（含成员，整体替换）。
func (d *DB) UpdateProxyGroup(g *config.ProxyGroup) error {
	_, err := d.db.Exec(
		`UPDATE proxy_groups SET name=?, type=?, test_url=?, interval_s=?, updated_at=? WHERE id=?`,
		g.Name, g.Type, g.TestURL, g.IntervalS, time.Now(), g.ID,
	)
	if err != nil {
		return fmt.Errorf("更新代理组 %d 失败: %w", g.ID, err)
	}
	return d.replaceGroupMembers(g)
}

// UpdateProxyGroupRenamed updates a proxy group and all routing references atomically.
func (d *DB) UpdateProxyGroupRenamed(g *config.ProxyGroup, oldName string) error {
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("开启代理组更新事务失败: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`UPDATE proxy_groups SET name=?, type=?, test_url=?, interval_s=?, updated_at=? WHERE id=?`,
		g.Name, g.Type, g.TestURL, g.IntervalS, time.Now(), g.ID,
	); err != nil {
		return fmt.Errorf("更新代理组 %d 失败: %w", g.ID, err)
	}
	if _, err := tx.Exec(`UPDATE routing_groups SET target=? WHERE target=?`, g.Name, oldName); err != nil {
		return fmt.Errorf("更新代理组引用失败: %w", err)
	}
	if err := replaceGroupMembersTx(tx, g); err != nil {
		return err
	}
	return tx.Commit()
}

// replaceGroupMembers 整体替换组成员。
func (d *DB) replaceGroupMembers(g *config.ProxyGroup) error {
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("开启组成员事务失败: %w", err)
	}
	defer tx.Rollback()
	if err := replaceGroupMembersTx(tx, g); err != nil {
		return err
	}
	return tx.Commit()
}

func replaceGroupMembersTx(tx *sql.Tx, g *config.ProxyGroup) error {

	if _, err := tx.Exec(`DELETE FROM proxy_group_members WHERE group_id=?`, g.ID); err != nil {
		return fmt.Errorf("清空代理组 %d 成员失败: %w", g.ID, err)
	}
	for i, m := range g.Members {
		if _, err := tx.Exec(
			`INSERT INTO proxy_group_members (group_id, member_type, member_id, position) VALUES (?, ?, ?, ?)`,
			g.ID, m.Type, m.ID, i,
		); err != nil {
			return fmt.Errorf("写入代理组 %d 成员失败: %w", g.ID, err)
		}
	}
	return nil
}

// DeleteProxyGroup 删除代理组。
func (d *DB) DeleteProxyGroup(id int64) error {
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("开启代理组删除事务失败: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM proxy_group_members WHERE member_type='group' AND member_id=?`, id); err != nil {
		return fmt.Errorf("清理代理组 %d 的嵌套引用失败: %w", id, err)
	}
	if _, err := tx.Exec(`UPDATE proxy_groups SET selected='' WHERE selected=?`, "group:"+strconv.FormatInt(id, 10)); err != nil {
		return fmt.Errorf("清理代理组 %d 的选中引用失败: %w", id, err)
	}
	if _, err := tx.Exec(`DELETE FROM proxy_groups WHERE id=?`, id); err != nil {
		return fmt.Errorf("删除代理组 %d 失败: %w", id, err)
	}
	return tx.Commit()
}

// GetProxyGroup 按 ID 查询代理组（含成员）。
func (d *DB) GetProxyGroup(id int64) (*config.ProxyGroup, error) {
	row := d.db.QueryRow(`SELECT id, name, type, test_url, interval_s, selected, created_at, updated_at FROM proxy_groups WHERE id=?`, id)
	g, err := scanProxyGroup(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := d.loadGroupMembers(g); err != nil {
		return nil, err
	}
	return g, nil
}

// ListProxyGroups 返回全部代理组（含成员），按名称排序。
func (d *DB) ListProxyGroups() ([]*config.ProxyGroup, error) {
	rows, err := d.db.Query(`SELECT id, name, type, test_url, interval_s, selected, created_at, updated_at FROM proxy_groups ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("查询代理组列表失败: %w", err)
	}
	defer rows.Close()

	var out []*config.ProxyGroup
	for rows.Next() {
		g, err := scanProxyGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, g := range out {
		if err := d.loadGroupMembers(g); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// SetProxyGroupSelected 持久化 select 组的当前选中成员（"node:<id>" / "group:<id>"）。
func (d *DB) SetProxyGroupSelected(id int64, selected string) error {
	_, err := d.db.Exec(`UPDATE proxy_groups SET selected=?, updated_at=? WHERE id=?`, selected, time.Now(), id)
	if err != nil {
		return fmt.Errorf("更新代理组 %d 选中项失败: %w", id, err)
	}
	return nil
}

func scanProxyGroup(row interface{ Scan(...any) error }) (*config.ProxyGroup, error) {
	var g config.ProxyGroup
	if err := row.Scan(&g.ID, &g.Name, &g.Type, &g.TestURL, &g.IntervalS, &g.Selected, &g.CreatedAt, &g.UpdatedAt); err != nil {
		return nil, err
	}
	return &g, nil
}

func (d *DB) loadGroupMembers(g *config.ProxyGroup) error {
	rows, err := d.db.Query(
		`SELECT member_type, member_id, position FROM proxy_group_members WHERE group_id=? ORDER BY position`, g.ID,
	)
	if err != nil {
		return fmt.Errorf("查询代理组 %d 成员失败: %w", g.ID, err)
	}
	defer rows.Close()

	g.Members = nil
	for rows.Next() {
		var m config.ProxyGroupMember
		if err := rows.Scan(&m.Type, &m.ID, &m.Position); err != nil {
			return fmt.Errorf("扫描代理组 %d 成员失败: %w", g.ID, err)
		}
		g.Members = append(g.Members, m)
	}
	return rows.Err()
}

// --- 分流组与规则 ---

// CreateRoutingGroup 新建分流组（含规则）。
func (d *DB) CreateRoutingGroup(g *config.RoutingGroup) error {
	res, err := d.db.Exec(
		`INSERT INTO routing_groups (name, target, position, enabled) VALUES (?, ?, ?, ?)`,
		g.Name, g.Target, g.Position, boolInt(g.Enabled),
	)
	if err != nil {
		return fmt.Errorf("创建分流组 %q 失败: %w", g.Name, err)
	}
	if g.ID, err = res.LastInsertId(); err != nil {
		return fmt.Errorf("获取分流组自增 ID 失败: %w", err)
	}
	return d.replaceRules(g)
}

// UpdateRoutingGroup 更新分流组（含规则，整体替换）。
func (d *DB) UpdateRoutingGroup(g *config.RoutingGroup) error {
	_, err := d.db.Exec(
		`UPDATE routing_groups SET name=?, target=?, position=?, enabled=? WHERE id=?`,
		g.Name, g.Target, g.Position, boolInt(g.Enabled), g.ID,
	)
	if err != nil {
		return fmt.Errorf("更新分流组 %d 失败: %w", g.ID, err)
	}
	return d.replaceRules(g)
}

// replaceRules 整体替换分流组规则（含逻辑规则子条件）。
func (d *DB) replaceRules(g *config.RoutingGroup) error {
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("开启规则事务失败: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM rules WHERE routing_group_id=?`, g.ID); err != nil {
		return fmt.Errorf("清空分流组 %d 规则失败: %w", g.ID, err)
	}
	for i := range g.Rules {
		r := &g.Rules[i]
		r.RoutingGroupID = g.ID
		r.Position = i
		res, err := tx.Exec(
			`INSERT INTO rules (routing_group_id, rule_type, value, mode, invert, enabled, position) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			g.ID, r.Type, r.Value, r.Mode, boolInt(r.Invert), boolInt(r.Enabled), r.Position,
		)
		if err != nil {
			return fmt.Errorf("写入分流组 %d 规则失败: %w", g.ID, err)
		}
		ruleID, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("获取规则自增 ID 失败: %w", err)
		}
		r.ID = ruleID
		for ci := range r.Conditions {
			c := &r.Conditions[ci]
			if _, err := tx.Exec(
				`INSERT INTO rule_conditions (rule_id, cond_type, value, invert, position) VALUES (?, ?, ?, ?, ?)`,
				ruleID, c.Type, c.Value, boolInt(c.Invert), ci,
			); err != nil {
				return fmt.Errorf("写入规则子条件失败: %w", err)
			}
		}
	}
	return tx.Commit()
}

// GetRoutingGroup 按 ID 查询分流组（含规则）。
func (d *DB) GetRoutingGroup(id int64) (*config.RoutingGroup, error) {
	all, err := d.ListRoutingGroups()
	if err != nil {
		return nil, err
	}
	for _, g := range all {
		if g.ID == id {
			return g, nil
		}
	}
	return nil, ErrNotFound
}

// DeleteRoutingGroup 删除分流组；规则级联删除。
func (d *DB) DeleteRoutingGroup(id int64) error {
	_, err := d.db.Exec(`DELETE FROM routing_groups WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("删除分流组 %d 失败: %w", id, err)
	}
	return nil
}

// ListRoutingGroups 返回全部分流组（含规则），按 position、id 排序。
func (d *DB) ListRoutingGroups() ([]*config.RoutingGroup, error) {
	rows, err := d.db.Query(`SELECT id, name, target, position, enabled FROM routing_groups ORDER BY position, id`)
	if err != nil {
		return nil, fmt.Errorf("查询分流组列表失败: %w", err)
	}
	defer rows.Close()

	var out []*config.RoutingGroup
	for rows.Next() {
		var g config.RoutingGroup
		var enabled int
		if err := rows.Scan(&g.ID, &g.Name, &g.Target, &g.Position, &enabled); err != nil {
			return nil, fmt.Errorf("扫描分流组失败: %w", err)
		}
		g.Enabled = enabled != 0
		out = append(out, &g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, g := range out {
		if err := d.loadRules(g); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (d *DB) loadRules(g *config.RoutingGroup) error {
	rows, err := d.db.Query(
		`SELECT id, rule_type, value, mode, invert, enabled, position FROM rules WHERE routing_group_id=? ORDER BY position, id`, g.ID,
	)
	if err != nil {
		return fmt.Errorf("查询分流组 %d 规则失败: %w", g.ID, err)
	}
	defer rows.Close()

	g.Rules = nil
	for rows.Next() {
		var r config.Rule
		var invert, enabled int
		if err := rows.Scan(&r.ID, &r.Type, &r.Value, &r.Mode, &invert, &enabled, &r.Position); err != nil {
			return fmt.Errorf("扫描规则失败: %w", err)
		}
		r.RoutingGroupID = g.ID
		r.Invert = invert != 0
		r.Enabled = enabled != 0
		g.Rules = append(g.Rules, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return d.loadRuleConditions(g)
}

// loadRuleConditions 批量加载组内逻辑规则的子条件，按规则归组。
func (d *DB) loadRuleConditions(g *config.RoutingGroup) error {
	rows, err := d.db.Query(
		`SELECT rc.rule_id, rc.cond_type, rc.value, rc.invert FROM rule_conditions rc
		 JOIN rules r ON r.id = rc.rule_id
		 WHERE r.routing_group_id=? ORDER BY rc.rule_id, rc.position`, g.ID,
	)
	if err != nil {
		return fmt.Errorf("查询分流组 %d 规则子条件失败: %w", g.ID, err)
	}
	defer rows.Close()

	conds := map[int64][]config.RuleCondition{}
	for rows.Next() {
		var ruleID int64
		var c config.RuleCondition
		var invert int
		if err := rows.Scan(&ruleID, &c.Type, &c.Value, &invert); err != nil {
			return fmt.Errorf("扫描规则子条件失败: %w", err)
		}
		c.Invert = invert != 0
		conds[ruleID] = append(conds[ruleID], c)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range g.Rules {
		g.Rules[i].Conditions = conds[g.Rules[i].ID]
	}
	return nil
}
