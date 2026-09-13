package storage

import (
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
func (d *DB) SaveSettings(s config.Settings) error {
	pairs := map[string]string{
		keyMixedPort:      strconv.Itoa(s.MixedPort),
		keyAllowLAN:       strconv.FormatBool(s.AllowLAN),
		keyDownloadProxy:  strings.TrimSpace(s.DownloadProxy),
		keyLogLevel:       s.LogLevel,
		keyClashAPIPort:   strconv.Itoa(s.ClashAPIPort),
		keyClashAPISecret: s.ClashAPISecret,
		keyAutoUpdateMin:  strconv.Itoa(s.AutoUpdateMinutes),
		keyPrivateDirect:  strconv.FormatBool(s.PrivateDirect),
		keyResolveIPRules: strconv.FormatBool(s.ResolveIPRules),
	}
	for k, v := range pairs {
		if err := d.SetSetting(k, v); err != nil {
			return fmt.Errorf("保存设置 %s 失败: %w", k, err)
		}
	}
	return nil
}
