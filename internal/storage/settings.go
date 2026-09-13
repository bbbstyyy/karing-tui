package storage

import (
	"database/sql"
	"fmt"
	"time"
)

// GetSetting 读取一个设置项；不存在时返回 ("", nil)。
func (d *DB) GetSetting(key string) (string, error) {
	var value string
	err := d.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("读取设置 %s 失败: %w", key, err)
	}
	return value, nil
}

// SetSetting 写入（upsert）一个设置项。
func (d *DB) SetSetting(key, value string) error {
	_, err := d.db.Exec(
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, time.Now(),
	)
	if err != nil {
		return fmt.Errorf("写入设置 %s 失败: %w", key, err)
	}
	return nil
}

// DeleteSetting 删除一个设置项；键不存在视为成功。
func (d *DB) DeleteSetting(key string) error {
	_, err := d.db.Exec(`DELETE FROM settings WHERE key = ?`, key)
	if err != nil {
		return fmt.Errorf("删除设置 %s 失败: %w", key, err)
	}
	return nil
}

// AllSettings 返回全部设置项，按 key 排序。
func (d *DB) AllSettings() (map[string]string, error) {
	rows, err := d.db.Query(`SELECT key, value FROM settings ORDER BY key`)
	if err != nil {
		return nil, fmt.Errorf("读取设置列表失败: %w", err)
	}
	defer rows.Close()

	out := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("扫描设置行失败: %w", err)
		}
		out[k] = v
	}
	return out, rows.Err()
}
