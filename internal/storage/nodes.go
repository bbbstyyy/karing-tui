package storage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/config"
)

// CreateNode 新建节点。
func (d *DB) CreateNode(n *config.Node) error {
	return d.createNode(d.db, n)
}

// CreateNodeTx 是 CreateNode 的事务内版本（C14-AUDIT）。
func (d *DB) CreateNodeTx(tx *sql.Tx, n *config.Node) error {
	return d.createNode(tx, n)
}

func (d *DB) createNode(q querier, n *config.Node) error {
	meta, err := json.Marshal(n.Metadata)
	if err != nil {
		return fmt.Errorf("序列化节点 %q 元数据失败: %w", n.Name, err)
	}
	now := time.Now()
	res, err := q.Exec(
		`INSERT INTO nodes (subscription_id, name, protocol, server, port, tls, transport, enabled, metadata, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		nullInt64(n.SubscriptionID), n.Name, n.Protocol, n.Server, n.Port,
		boolInt(n.TLS), n.Transport, boolInt(n.Enabled), string(meta), now, now,
	)
	if err != nil {
		return fmt.Errorf("创建节点 %q 失败: %w", n.Name, err)
	}
	if n.ID, err = res.LastInsertId(); err != nil {
		return fmt.Errorf("获取节点自增 ID 失败: %w", err)
	}
	n.CreatedAt, n.UpdatedAt = now, now
	return nil
}

// UpdateNode 更新节点可编辑字段。
func (d *DB) UpdateNode(n *config.Node) error {
	meta, err := json.Marshal(n.Metadata)
	if err != nil {
		return fmt.Errorf("序列化节点 %q 元数据失败: %w", n.Name, err)
	}
	_, err = d.db.Exec(
		`UPDATE nodes SET name=?, protocol=?, server=?, port=?, tls=?, transport=?, enabled=?, metadata=?, updated_at=? WHERE id=?`,
		n.Name, n.Protocol, n.Server, n.Port, boolInt(n.TLS), n.Transport,
		boolInt(n.Enabled), string(meta), time.Now(), n.ID,
	)
	if err != nil {
		return fmt.Errorf("更新节点 %d 失败: %w", n.ID, err)
	}
	return nil
}

// UpdateNodeLatency 写入节点测速结果；latencyMS < 0 表示失败。
func (d *DB) UpdateNodeLatency(id int64, latencyMS int64, testedAt time.Time) error {
	_, err := d.db.Exec(`UPDATE nodes SET latency_ms=?, last_tested=? WHERE id=?`, latencyMS, testedAt, id)
	if err != nil {
		return fmt.Errorf("更新节点 %d 测速结果失败: %w", id, err)
	}
	return nil
}

// DeleteNode 删除节点。
func (d *DB) DeleteNode(id int64) error {
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("开启节点删除事务失败: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM proxy_group_members WHERE member_type='node' AND member_id=?`, id); err != nil {
		return fmt.Errorf("清理节点 %d 的代理组引用失败: %w", id, err)
	}
	if _, err := tx.Exec(`UPDATE proxy_groups SET selected='' WHERE selected=?`, "node:"+strconv.FormatInt(id, 10)); err != nil {
		return fmt.Errorf("清理节点 %d 的选中引用失败: %w", id, err)
	}
	if _, err := tx.Exec(`DELETE FROM nodes WHERE id=?`, id); err != nil {
		return fmt.Errorf("删除节点 %d 失败: %w", id, err)
	}
	return tx.Commit()
}

// DeleteNodesBySubscription 删除某订阅的全部节点（订阅刷新前清空旧节点）。
func (d *DB) DeleteNodesBySubscription(subscriptionID int64) error {
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("开启订阅 %d 节点删除事务失败: %w", subscriptionID, err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM proxy_group_members
		WHERE member_type='node' AND member_id IN
		(SELECT id FROM nodes WHERE subscription_id=?)`, subscriptionID); err != nil {
		return fmt.Errorf("清理订阅 %d 的代理组引用失败: %w", subscriptionID, err)
	}
	if _, err := tx.Exec(`UPDATE proxy_groups SET selected=''
		WHERE selected IN (SELECT 'node:' || id FROM nodes WHERE subscription_id=?)`, subscriptionID); err != nil {
		return fmt.Errorf("清理订阅 %d 的选中引用失败: %w", subscriptionID, err)
	}
	if _, err := tx.Exec(`DELETE FROM nodes WHERE subscription_id=?`, subscriptionID); err != nil {
		return fmt.Errorf("删除订阅 %d 的节点失败: %w", subscriptionID, err)
	}
	return tx.Commit()
}

// DeleteFailedNodesBySubscription 删除指定订阅中测速失败的节点。
func (d *DB) DeleteFailedNodesBySubscription(subscriptionID int64) (int64, error) {
	tx, err := d.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("开启订阅 %d 失效节点清理事务失败: %w", subscriptionID, err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM proxy_group_members
		WHERE member_type='node' AND member_id IN
		(SELECT id FROM nodes WHERE subscription_id=? AND last_tested IS NOT NULL AND latency_ms < 0)`, subscriptionID); err != nil {
		return 0, fmt.Errorf("清理订阅 %d 的失效节点引用失败: %w", subscriptionID, err)
	}
	if _, err := tx.Exec(`UPDATE proxy_groups SET selected=''
		WHERE selected IN (SELECT 'node:' || id FROM nodes WHERE subscription_id=? AND last_tested IS NOT NULL AND latency_ms < 0)`, subscriptionID); err != nil {
		return 0, fmt.Errorf("清理订阅 %d 的失效选中引用失败: %w", subscriptionID, err)
	}
	r, err := tx.Exec(`DELETE FROM nodes WHERE subscription_id=? AND last_tested IS NOT NULL AND latency_ms < 0`, subscriptionID)
	if err != nil {
		return 0, fmt.Errorf("清理订阅 %d 失效节点失败: %w", subscriptionID, err)
	}
	n, _ := r.RowsAffected()
	if _, err := tx.Exec(`UPDATE subscriptions SET node_count=(SELECT COUNT(*) FROM nodes WHERE subscription_id=?) WHERE id=?`, subscriptionID, subscriptionID); err != nil {
		return 0, fmt.Errorf("更新订阅 %d 节点数失败: %w", subscriptionID, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("提交订阅 %d 失效节点清理失败: %w", subscriptionID, err)
	}
	return n, nil
}

// ReplaceSubscriptionNodes 原子替换订阅节点并更新订阅状态。
// 节点写入或状态更新任一步失败都会回滚，保留原节点池。
//
// 入参约束（V7-1）：nodes 中不得出现两个携带同一非零 ID 的节点。下方用于保留
// 组引用的 usedIDs 只能保证「同一个旧 ID 不会被 UPDATE 两次」，无法说明这种输入
// 是对的；让第二个节点静默落成新行会把上层身份算法的异常变成一次无痕的插入。
// 因此这里 fail-closed，且检查放在 BEGIN 之前——拒绝时不产生任何破坏性动作。
func (d *DB) ReplaceSubscriptionNodes(subscriptionID int64, nodes []*config.Node, lastUpdated time.Time) error {
	if err := validateReplacedNodeIDs(nodes); err != nil {
		return err
	}
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("开启订阅节点替换事务失败: %w", err)
	}
	defer tx.Rollback()

	// 保留匹配节点的 ID，避免 proxy_group_members 中的显式节点引用失效。
	deleteSQL := `DELETE FROM nodes WHERE subscription_id=?`
	deleteArgs := []any{subscriptionID}
	staleClause := ""
	staleArgs := []any{subscriptionID}
	if len(nodes) > 0 {
		ids := make([]string, 0, len(nodes))
		for _, n := range nodes {
			if n.ID > 0 {
				ids = append(ids, "?")
				deleteArgs = append(deleteArgs, n.ID)
			}
		}
		if len(ids) > 0 {
			deleteSQL += ` AND id NOT IN (` + strings.Join(ids, ",") + `)`
			staleClause = ` AND id NOT IN (` + strings.Join(ids, ",") + `)`
			staleArgs = append(staleArgs, deleteArgs[1:]...)
		}
	}
	// 清理即将删除节点的多态代理组引用，避免刷新订阅后留下失效成员。
	if _, err := tx.Exec(`DELETE FROM proxy_group_members
		WHERE member_type='node' AND member_id IN
		(SELECT id FROM nodes WHERE subscription_id=?`+staleClause+`)`, staleArgs...); err != nil {
		return fmt.Errorf("清理订阅 %d 的旧节点引用失败: %w", subscriptionID, err)
	}
	if _, err := tx.Exec(`UPDATE proxy_groups SET selected=''
		WHERE selected IN (SELECT 'node:' || id FROM nodes WHERE subscription_id=?`+staleClause+`)`, staleArgs...); err != nil {
		return fmt.Errorf("清理订阅 %d 的旧选中引用失败: %w", subscriptionID, err)
	}
	if _, err := tx.Exec(deleteSQL, deleteArgs...); err != nil {
		return fmt.Errorf("删除订阅 %d 的旧节点失败: %w", subscriptionID, err)
	}
	const insert = `INSERT INTO nodes
		(subscription_id, name, protocol, server, port, tls, transport, enabled, latency_ms, last_tested, metadata, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	usedIDs := make(map[int64]bool, len(nodes))
	for _, n := range nodes {
		meta, err := json.Marshal(n.Metadata)
		if err != nil {
			return fmt.Errorf("序列化节点 %q 元数据失败: %w", n.Name, err)
		}
		now := time.Now()
		var latency any
		var tested any
		if !n.LastTested.IsZero() {
			latency = n.LatencyMS
			tested = n.LastTested
		}
		if n.ID > 0 && !usedIDs[n.ID] {
			usedIDs[n.ID] = true
			res, err := tx.Exec(`UPDATE nodes SET name=?, protocol=?, server=?, port=?, tls=?, transport=?, enabled=?, latency_ms=?, last_tested=?, metadata=?, updated_at=? WHERE id=? AND subscription_id=?`,
				n.Name, n.Protocol, n.Server, n.Port, boolInt(n.TLS), n.Transport, boolInt(n.Enabled), latency, tested, string(meta), now, n.ID, subscriptionID)
			if err != nil {
				return fmt.Errorf("更新订阅 %d 节点 %q 失败: %w", subscriptionID, n.Name, err)
			}
			if affected, _ := res.RowsAffected(); affected > 0 {
				n.SubscriptionID = subscriptionID
				n.UpdatedAt = now
				continue
			}
		}
		res, err := tx.Exec(insert, subscriptionID, n.Name, n.Protocol, n.Server, n.Port,
			boolInt(n.TLS), n.Transport, boolInt(n.Enabled), latency, tested, string(meta), now, now)
		if err != nil {
			return fmt.Errorf("写入订阅 %d 节点 %q 失败: %w", subscriptionID, n.Name, err)
		}
		if n.ID, err = res.LastInsertId(); err != nil {
			return fmt.Errorf("获取节点自增 ID 失败: %w", err)
		}
		n.SubscriptionID = subscriptionID
		n.CreatedAt, n.UpdatedAt = now, now
	}
	if _, err := tx.Exec(`UPDATE subscriptions SET last_updated=?, node_count=? WHERE id=?`,
		lastUpdated, len(nodes), subscriptionID); err != nil {
		return fmt.Errorf("更新订阅 %d 状态失败: %w", subscriptionID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交订阅 %d 节点替换失败: %w", subscriptionID, err)
	}
	return nil
}

// validateReplacedNodeIDs 拒绝携带重复非零 ID 的替换输入（V7-1 防御性检查）。
// ID 为 0 表示「新节点」，可以出现任意次；非零 ID 每个最多一次。
func validateReplacedNodeIDs(nodes []*config.Node) error {
	seen := make(map[int64]bool, len(nodes))
	for _, n := range nodes {
		if n == nil || n.ID <= 0 {
			continue
		}
		if seen[n.ID] {
			return fmt.Errorf("订阅节点替换包含重复的非零 ID %d，拒绝写入", n.ID)
		}
		seen[n.ID] = true
	}
	return nil
}

// GetNode 按 ID 查询节点。
func (d *DB) GetNode(id int64) (*config.Node, error) {
	row := d.db.QueryRow(`SELECT `+nodeColumns+` FROM nodes WHERE id=?`, id)
	n, err := scanNode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return n, err
}

// ListNodes 返回节点池；subscriptionID > 0 时按订阅过滤。
func (d *DB) ListNodes(subscriptionID int64) ([]*config.Node, error) {
	var rows *sql.Rows
	var err error
	if subscriptionID > 0 {
		rows, err = d.db.Query(`SELECT n.id, n.subscription_id, n.name, n.protocol, n.server, n.port,
			n.tls, n.transport, n.enabled, n.latency_ms, n.last_tested, n.metadata, n.created_at, n.updated_at FROM nodes n
			JOIN subscriptions s ON s.id=n.subscription_id
			WHERE n.subscription_id=?
			ORDER BY CASE WHEN s.sort_by='latency' AND n.latency_ms >= 0 THEN 0 ELSE 1 END,
			         CASE WHEN s.sort_by='latency' AND n.latency_ms >= 0 THEN n.latency_ms END,
			         n.name`, subscriptionID)
	} else {
		rows, err = d.db.Query(`SELECT ` + nodeColumns + ` FROM nodes ORDER BY name`)
	}
	if err != nil {
		return nil, fmt.Errorf("查询节点列表失败: %w", err)
	}
	defer rows.Close()

	var out []*config.Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

const nodeColumns = `id, subscription_id, name, protocol, server, port, tls, transport, enabled, latency_ms, last_tested, metadata, created_at, updated_at`

func scanNode(row interface{ Scan(...any) error }) (*config.Node, error) {
	var n config.Node
	var subID sql.NullInt64
	var enabled, tls int
	var latency sql.NullInt64
	var lastTested sql.NullTime
	var meta string
	if err := row.Scan(&n.ID, &subID, &n.Name, &n.Protocol, &n.Server, &n.Port,
		&tls, &n.Transport, &enabled, &latency, &lastTested, &meta, &n.CreatedAt, &n.UpdatedAt); err != nil {
		return nil, err
	}
	n.SubscriptionID = subID.Int64
	n.TLS = tls != 0
	n.Enabled = enabled != 0
	n.LatencyMS = latency.Int64
	n.LastTested = lastTested.Time
	if !lastTested.Valid {
		n.LatencyMS = -1
	}
	if err := json.Unmarshal([]byte(meta), &n.Metadata); err != nil {
		return nil, fmt.Errorf("解析节点 %d 元数据失败: %w", n.ID, err)
	}
	if n.Metadata == nil {
		n.Metadata = map[string]any{}
	}
	return &n, nil
}
