package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/config"
)

// ErrNotFound 通用的“记录不存在”错误。
var ErrNotFound = errors.New("storage: record not found")

// CreateSubscription 新建订阅。
func (d *DB) CreateSubscription(s *config.Subscription) error {
	s.Enabled = true
	strategy, err := config.NormalizeDownloadStrategy(s.DownloadStrategy)
	if err != nil {
		return err
	}
	s.DownloadStrategy = strategy
	now := time.Now()
	res, err := d.db.Exec(
		`INSERT INTO subscriptions (name, url, enabled, user_agent, download_strategy, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		s.Name, s.URL, boolInt(s.Enabled), s.UserAgent, s.DownloadStrategy, now, now,
	)
	if err != nil {
		return fmt.Errorf("创建订阅 %q 失败: %w", s.Name, err)
	}
	if s.ID, err = res.LastInsertId(); err != nil {
		return fmt.Errorf("获取订阅自增 ID 失败: %w", err)
	}
	s.CreatedAt, s.UpdatedAt = now, now
	return nil
}

// UpdateSubscription 更新订阅的可编辑字段。
func (d *DB) UpdateSubscription(s *config.Subscription) error {
	strategy, err := config.NormalizeDownloadStrategy(s.DownloadStrategy)
	if err != nil {
		return err
	}
	s.DownloadStrategy = strategy
	_, err = d.db.Exec(
		`UPDATE subscriptions SET name=?, url=?, enabled=?, user_agent=?, download_strategy=?, node_filter=?, sort_by=?, auto_test=?, auto_clean=?, traffic_upload=?, traffic_download=?, traffic_total=?, expire_at=?, updated_at=? WHERE id=?`,
		s.Name, s.URL, boolInt(s.Enabled), s.UserAgent, s.DownloadStrategy, s.NodeFilter, subscriptionSort(s.SortBy), boolInt(s.AutoTest), boolInt(s.AutoClean), s.TrafficUpload, s.TrafficDownload, s.TrafficTotal, nullTime(s.ExpireAt), time.Now(), s.ID,
	)
	if err != nil {
		return fmt.Errorf("更新订阅 %d 失败: %w", s.ID, err)
	}
	return nil
}

// UpdateSubscriptionState 更新订阅的更新时间与节点数（订阅刷新后调用）。
func (d *DB) UpdateSubscriptionState(id int64, lastUpdated time.Time, nodeCount int) error {
	_, err := d.db.Exec(
		`UPDATE subscriptions SET last_updated=?, node_count=? WHERE id=?`,
		lastUpdated, nodeCount, id,
	)
	if err != nil {
		return fmt.Errorf("更新订阅 %d 状态失败: %w", id, err)
	}
	return nil
}

func (d *DB) UpdateSubscriptionTraffic(id int64, upload, download, total int64, expireAt time.Time) error {
	_, err := d.db.Exec(`UPDATE subscriptions SET traffic_upload=?, traffic_download=?, traffic_total=?, expire_at=?, updated_at=? WHERE id=?`, upload, download, total, nullTime(expireAt), time.Now(), id)
	return err
}

// DeleteSubscription 删除订阅；其节点经外键级联删除。
func (d *DB) DeleteSubscription(id int64) error {
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("开启订阅删除事务失败: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM proxy_group_members
		WHERE member_type='node' AND member_id IN
		(SELECT id FROM nodes WHERE subscription_id=?)`, id); err != nil {
		return fmt.Errorf("清理订阅 %d 的代理组引用失败: %w", id, err)
	}
	if _, err := tx.Exec(`UPDATE proxy_groups SET selected=''
		WHERE selected IN (SELECT 'node:' || id FROM nodes WHERE subscription_id=?)`, id); err != nil {
		return fmt.Errorf("清理订阅 %d 的选中引用失败: %w", id, err)
	}
	if _, err := tx.Exec(`DELETE FROM subscriptions WHERE id=?`, id); err != nil {
		return fmt.Errorf("删除订阅 %d 失败: %w", id, err)
	}
	return tx.Commit()
}

// GetSubscription 按 ID 查询订阅。
func (d *DB) GetSubscription(id int64) (*config.Subscription, error) {
	row := d.db.QueryRow(`SELECT `+subscriptionColumns+` FROM subscriptions WHERE id=?`, id)
	s, err := scanSubscription(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return s, err
}

// ListSubscriptions 返回全部订阅，按名称排序。
func (d *DB) ListSubscriptions() ([]*config.Subscription, error) {
	rows, err := d.db.Query(`SELECT ` + subscriptionColumns + ` FROM subscriptions ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("查询订阅列表失败: %w", err)
	}
	defer rows.Close()

	var out []*config.Subscription
	for rows.Next() {
		s, err := scanSubscription(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

const subscriptionColumns = `id, name, url, enabled, user_agent, download_strategy, node_filter, sort_by, auto_test, auto_clean, traffic_upload, traffic_download, traffic_total, expire_at, last_updated, node_count, created_at, updated_at`

func scanSubscription(row interface{ Scan(...any) error }) (*config.Subscription, error) {
	var s config.Subscription
	var enabled int
	var lastUpdated, expireAt sql.NullTime
	var autoTest, autoClean int
	if err := row.Scan(&s.ID, &s.Name, &s.URL, &enabled, &s.UserAgent, &s.DownloadStrategy, &s.NodeFilter, &s.SortBy, &autoTest, &autoClean,
		&s.TrafficUpload, &s.TrafficDownload, &s.TrafficTotal, &expireAt, &lastUpdated, &s.NodeCount, &s.CreatedAt, &s.UpdatedAt); err != nil {
		return nil, err
	}
	s.Enabled = enabled != 0
	s.AutoTest = autoTest != 0
	s.AutoClean = autoClean != 0
	strategy, err := config.NormalizeDownloadStrategy(s.DownloadStrategy)
	if err != nil {
		return nil, err
	}
	s.DownloadStrategy = strategy
	if expireAt.Valid {
		s.ExpireAt = expireAt.Time
	}
	s.SortBy = subscriptionSort(s.SortBy)
	s.LastUpdated = lastUpdated.Time
	return &s, nil
}

func subscriptionSort(v string) string {
	if v == "latency" {
		return v
	}
	return "name"
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
