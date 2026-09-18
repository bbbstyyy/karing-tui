package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"github.com/bbbstyyy/karing-tui/internal/config"
)

// settings 键名约定。
const (
	keyMixedPort      = "mixed_port"
	keyAllowLAN       = "allow_lan"
	keyDownloadProxy  = "download_proxy"
	keyLogLevel       = "log_level"
	keyClashAPIPort   = "clash_api_port"
	keyClashAPISecret = "clash_api_secret"
	keyAutoUpdateMin  = "auto_update_minutes"
	keyPrivateDirect  = "private_direct"
	keyResolveIPRules = "resolve_ip_rules"
)

// LoadSettings 从键值表加载应用设置；缺失的键取默认值。
func (d *DB) LoadSettings() (config.Settings, error) {
	s := config.DefaultSettings()
	all, err := d.AllSettings()
	if err != nil {
		return s, err
	}
	if v, ok := all[keyMixedPort]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 65535 {
			s.MixedPort = n
		}
	}
	if v, ok := all[keyAllowLAN]; ok {
		s.AllowLAN = v == "true"
	}
	if v, ok := all[keyDownloadProxy]; ok {
		s.DownloadProxy = v
	}
	if v, ok := all[keyLogLevel]; ok {
		s.LogLevel = v
	}
	if v, ok := all[keyClashAPIPort]; ok {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 65535 {
			s.ClashAPIPort = n
		}
	}
	if v, ok := all[keyClashAPISecret]; ok {
		s.ClashAPISecret = v
	}
	if v, ok := all[keyAutoUpdateMin]; ok {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= config.MaxAutoUpdateMinutes {
			s.AutoUpdateMinutes = n
		}
	}
	// 缺键时保持默认（private_direct 默认开、resolve_ip_rules 默认关）
	if v, ok := all[keyPrivateDirect]; ok {
		s.PrivateDirect = v == "true"
	}
	if v, ok := all[keyResolveIPRules]; ok {
		s.ResolveIPRules = v == "true"
	}
	return s, nil
}

// SaveSettings 将应用设置写回键值表。
//
// 全部键在**一个事务**里提交（V7-4）：中途失败留下「一半新值一半旧值」的混合配置时，
// 设置页会自相矛盾（例如 mixed_port 改了、clash_api_port 没改，两个端口直接撞车），
// 而且当时不会有任何报错。
//
// 写入顺序用固定 slice 而不是 map：map 的遍历顺序随机会让「中途失败停在哪个键」
// 不可复现，失败现场每次都不一样，报障无法复述。顺序本身不影响语义——全部同一事务。
// 值也用同一个 slice 承载，避免出现「值表里加了一个键、顺序表里忘了加」的静默遗漏。
func (d *DB) SaveSettings(s config.Settings) error {
	pairs := []struct{ key, value string }{
		{keyMixedPort, strconv.Itoa(s.MixedPort)},
		{keyAllowLAN, strconv.FormatBool(s.AllowLAN)},
		{keyDownloadProxy, strings.TrimSpace(s.DownloadProxy)},
		{keyLogLevel, s.LogLevel},
		{keyClashAPIPort, strconv.Itoa(s.ClashAPIPort)},
		{keyClashAPISecret, s.ClashAPISecret},
		{keyAutoUpdateMin, strconv.Itoa(s.AutoUpdateMinutes)},
		{keyPrivateDirect, strconv.FormatBool(s.PrivateDirect)},
		{keyResolveIPRules, strconv.FormatBool(s.ResolveIPRules)},
	}
	return d.WithTx(context.Background(), func(tx *sql.Tx) error {
		for _, p := range pairs {
			if err := d.SetSettingTx(tx, p.key, p.value); err != nil {
				return fmt.Errorf("保存设置 %s 失败: %w", p.key, err)
			}
		}
		return nil
	})
}
