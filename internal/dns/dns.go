// Package dns 实现 DNS 配置管理：DNS 服务器（含 DoH/DoQ）、DNS 规则、
// FakeIP 与全局策略。全局选项持久化在 settings 键值表。
package dns

import (
	"fmt"
	"github.com/bbbstyyy/karing-tui/internal/validation"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/storage"
)

// settings 键名。
const (
	keyStrategy    = "dns_strategy"
	keyFakeIPOn    = "dns_fakeip_enabled"
	keyFakeIPRange = "dns_fakeip_range"
	keyFinal       = "dns_final"
)

// 默认值。
const (
	DefaultStrategy    = "prefer_ipv4"
	DefaultFakeIPRange = "198.18.0.0/15"
)

// serverTypes 合法的 DNS 服务器类型（sing-box 1.12+ 传输类型）。
var serverTypes = map[string]bool{
	"udp": true, "tcp": true, "tls": true, "https": true,
	"quic": true, "h3": true, "local": true,
}

// ruleTypes 合法的 DNS 规则类型。
var ruleTypes = map[string]bool{
	"domain": true, "domain_suffix": true, "domain_keyword": true, "rule_set": true,
}

// Manager DNS 配置管理器。
type Manager struct {
	DB   *storage.DB
	Logf func(format string, args ...any)
}

// NewManager 创建 DNS 管理器。logf 可为 nil。
func NewManager(db *storage.DB, logf func(string, ...any)) *Manager {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Manager{DB: db, Logf: logf}
}

// EnsureDefaultDNS 首次使用时初始化默认 DNS：国内 UDP、国外 DoH，
// 以及"国内域名走国内 DNS"的默认规则。
func (m *Manager) EnsureDefaultDNS() error {
	servers, err := m.DB.ListDNSServers()
	if err != nil {
		return err
	}
	if len(servers) > 0 {
		return nil
	}
	local := &config.DNSServer{Tag: "local", Type: "udp", Address: "223.5.5.5", Enabled: true, Position: 0}
	if err := m.DB.CreateDNSServer(local); err != nil {
		return err
	}
	remote := &config.DNSServer{
		Tag: "remote", Type: "https", Address: "8.8.8.8",
		AddressResolver: "local", Detour: storage.DefaultGroupAuto, Enabled: true, Position: 1,
	}
	if err := m.DB.CreateDNSServer(remote); err != nil {
		return err
	}
	rule := &config.DNSRule{Type: "rule_set", Value: "geosite:cn", Server: "local", Enabled: true, Position: 0}
	if err := m.DB.CreateDNSRule(rule); err != nil {
		return err
	}
	// 默认 final 指向 remote
	if err := m.DB.SetSetting(keyFinal, "remote"); err != nil {
		return err
	}
	m.Logf("已初始化默认 DNS 配置")
	return nil
}

// --- DNS 服务器 ---

// AddServer 新增 DNS 服务器。
func (m *Manager) AddServer(tag, typ, address, addressResolver, detour string) (*config.DNSServer, error) {
	s := &config.DNSServer{
		Tag:             strings.TrimSpace(tag),
		Type:            strings.ToLower(strings.TrimSpace(typ)),
		Address:         strings.TrimSpace(address),
		AddressResolver: strings.TrimSpace(addressResolver),
		Detour:          strings.TrimSpace(detour),
		Enabled:         true,
	}
	if err := m.validateServer(s, 0); err != nil {
		return nil, err
	}
	s.Position = int(^uint(0) >> 1) // 排到最后，下方取 max+1
	existing, err := m.DB.ListDNSServers()
	if err != nil {
		return nil, err
	}
	max := -1
	for _, e := range existing {
		if e.Position > max {
			max = e.Position
		}
	}
	s.Position = max + 1
	if err := m.DB.CreateDNSServer(s); err != nil {
		return nil, err
	}
	m.Logf("添加 DNS 服务器 %q (%s)", s.Tag, s.Type)
	return s, nil
}

// UpdateServer 更新 DNS 服务器。
func (m *Manager) UpdateServer(s *config.DNSServer) error {
	old, err := m.getServer(s.ID)
	if err != nil {
		return err
	}
	s.Tag = strings.TrimSpace(s.Tag)
	s.Type = strings.ToLower(strings.TrimSpace(s.Type))
	s.Address = strings.TrimSpace(s.Address)
	s.AddressResolver = strings.TrimSpace(s.AddressResolver)
	s.Detour = strings.TrimSpace(s.Detour)
	if err := m.validateServer(s, s.ID); err != nil {
		return err
	}
	if old.Enabled && !s.Enabled {
		cfg, err := m.LoadConfig()
		if err != nil {
			return err
		}
		if cfg.Final == old.Tag {
			return fmt.Errorf("DNS 服务器 %q 是当前默认（final）服务器，请先修改默认 DNS", old.Tag)
		}
		rules, err := m.DB.ListDNSRules()
		if err != nil {
			return err
		}
		for _, r := range rules {
			if r.Server == old.Tag {
				return fmt.Errorf("DNS 规则正在引用服务器 %q，请先修改或删除对应规则", old.Tag)
			}
		}
		servers, err := m.DB.ListDNSServers()
		if err != nil {
			return err
		}
		for _, other := range servers {
			if other.ID != old.ID && other.AddressResolver == old.Tag {
				return fmt.Errorf("DNS 服务器 %q 的 AddressResolver 正在引用 %q，请先修改引用", other.Tag, old.Tag)
			}
		}
	}
	if old.Tag != s.Tag {
		if err := m.DB.UpdateDNSServerRenamed(s, old.Tag); err != nil {
			return err
		}
	} else if err := m.DB.UpdateDNSServer(s); err != nil {
		return err
	}
	m.Logf("修改 DNS 服务器 %q", s.Tag)
	return nil
}

// DeleteServer 删除 DNS 服务器；被 DNS 规则引用或作为 final 时拒绝。
func (m *Manager) DeleteServer(id int64) error {
	s, err := m.getServer(id)
	if err != nil {
		return err
	}
	cfg, err := m.LoadConfig()
	if err != nil {
		return err
	}
	if cfg.Final == s.Tag {
		return fmt.Errorf("DNS 服务器 %q 是当前默认（final）服务器，请先修改默认 DNS", s.Tag)
	}
	rules, err := m.DB.ListDNSRules()
	if err != nil {
		return err
	}
	for _, r := range rules {
		if r.Server == s.Tag {
			return fmt.Errorf("DNS 规则正在引用服务器 %q，请先删除对应规则", s.Tag)
		}
	}
	servers, err := m.DB.ListDNSServers()
	if err != nil {
		return err
	}
	for _, other := range servers {
		if other.ID != s.ID && other.AddressResolver == s.Tag {
			return fmt.Errorf("DNS 服务器 %q 的 AddressResolver 正在引用 %q，请先修改引用", other.Tag, s.Tag)
		}
	}
	if err := m.DB.DeleteDNSServer(id); err != nil {
		return err
	}
	m.Logf("删除 DNS 服务器 %q", s.Tag)
	return nil
}

// SetServerEnabled 启用/停用 DNS 服务器。
func (m *Manager) SetServerEnabled(id int64, enabled bool) error {
	s, err := m.getServer(id)
	if err != nil {
		return err
	}
	if !enabled {
		cfg, err := m.LoadConfig()
		if err != nil {
			return err
		}
		if cfg.Final == s.Tag {
			return fmt.Errorf("DNS 服务器 %q 是当前默认（final）服务器，请先修改默认 DNS", s.Tag)
		}
		rules, err := m.DB.ListDNSRules()
		if err != nil {
			return err
		}
		for _, r := range rules {
			if r.Server == s.Tag {
				return fmt.Errorf("DNS 规则正在引用服务器 %q，请先修改或删除对应规则", s.Tag)
			}
		}
		servers, err := m.DB.ListDNSServers()
		if err != nil {
			return err
		}
		for _, other := range servers {
			if other.ID != s.ID && other.AddressResolver == s.Tag {
				return fmt.Errorf("DNS 服务器 %q 的 AddressResolver 正在引用 %q，请先修改引用", other.Tag, s.Tag)
			}
		}
	}
	s.Enabled = enabled
	if err := m.DB.UpdateDNSServer(s); err != nil {
		return err
	}
	state := "启用"
	if !enabled {
		state = "停用"
	}
	m.Logf("DNS 服务器 %q 已%s", s.Tag, state)
	return nil
}

func (m *Manager) validateServer(s *config.DNSServer, selfID int64) error {
	if s.Tag == "" {
		return validation.New("tag", "DNS 标签不能为空")
	}
	if !serverTypes[s.Type] {
		return validation.New("type", "类型 %q 不支持（udp/tcp/tls/https/quic/h3/local）", s.Type)
	}
	if s.Type != "local" && s.Address == "" {
		return validation.New("address", "服务器地址不能为空")
	}
	if s.Type != "local" {
		if err := validateServerAddress(s.Type, s.Address); err != nil {
			return validation.New("address", "DNS 服务器地址非法: %w", err)
		}
	}
	// Tag 唯一
	existing, err := m.DB.ListDNSServers()
	if err != nil {
		return err
	}
	for _, e := range existing {
		if e.ID != selfID && e.Tag == s.Tag {
			return validation.New("tag", "DNS 标签 %q 已存在", s.Tag)
		}
	}
	// AddressResolver 必须指向存在且启用的 DNS tag；禁用的解析器不会出现在生成配置中。
	if s.Detour != "" && !strings.EqualFold(s.Detour, "direct") {
		groups, err := m.DB.ListProxyGroups()
		if err != nil {
			return err
		}
		found := false
		for _, g := range groups {
			if g.Name == s.Detour {
				found = true
				break
			}
		}
		if !found {
			return validation.New("detour", "出站代理组 %q 不存在，请选择已有代理组", s.Detour)
		}
	}
	if s.AddressResolver != "" {
		if s.AddressResolver == s.Tag {
			return validation.New("resolver", "DNS 服务器 %q 不能将自身作为 address_resolver", s.Tag)
		}
		for _, e := range existing {
			if e.Tag == s.AddressResolver {
				if e.ID == selfID {
					return validation.New("resolver", "DNS 服务器不能将自身作为域名解析 DNS")
				}
				if s.Enabled && !e.Enabled {
					return validation.New("resolver", "AddressResolver %q 已停用", s.AddressResolver)
				}
				return nil
			}
		}
		return validation.New("resolver", "AddressResolver %q 不是已存在的 DNS 服务器 Tag", s.AddressResolver)
	}
	return nil
}

func validateServerAddress(typ, address string) error {
	address = strings.TrimSpace(address)
	if strings.Contains(address, "://") {
		u, err := url.Parse(address)
		if err != nil || u.Host == "" || u.Hostname() == "" {
			return fmt.Errorf("地址或 URL 格式错误")
		}
		if !strings.EqualFold(u.Scheme, typ) {
			return fmt.Errorf("URL scheme %q 与 DNS 类型 %q 不一致", u.Scheme, typ)
		}
		if p := u.Port(); p != "" {
			return validatePort(p)
		}
		return nil
	}
	if strings.Count(address, ":") == 0 {
		return nil
	}
	if strings.HasPrefix(address, "[") {
		_, port, err := net.SplitHostPort(address)
		if err != nil {
			return fmt.Errorf("IPv6 地址或端口格式错误")
		}
		return validatePort(port)
	}
	if strings.Count(address, ":") > 1 {
		if net.ParseIP(address) != nil {
			return nil
		}
		return fmt.Errorf("IPv6 地址必须是合法地址")
	}
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("端口格式错误")
	}
	return validatePort(port)
}

func validatePort(value string) error {
	p, err := strconv.Atoi(value)
	if err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("端口必须为 1-65535")
	}
	return nil
}

// --- DNS 规则 ---

// AddRule 新增 DNS 规则。
func (m *Manager) AddRule(typ, value, server string) (*config.DNSRule, error) {
	r := &config.DNSRule{
		Type:    strings.ToLower(strings.TrimSpace(typ)),
		Value:   strings.TrimSpace(value),
		Server:  strings.TrimSpace(server),
		Enabled: true,
	}
	if err := m.validateRule(r); err != nil {
		return nil, err
	}
	existing, err := m.DB.ListDNSRules()
	if err != nil {
		return nil, err
	}
	max := -1
	for _, e := range existing {
		if e.Position > max {
			max = e.Position
		}
	}
	r.Position = max + 1
	if err := m.DB.CreateDNSRule(r); err != nil {
		return nil, err
	}
	m.Logf("添加 DNS 规则 %s:%s → %s", r.Type, r.Value, r.Server)
	return r, nil
}

// UpdateRule 更新 DNS 规则。
func (m *Manager) UpdateRule(r *config.DNSRule) error {
	r.Type = strings.ToLower(strings.TrimSpace(r.Type))
	r.Value = strings.TrimSpace(r.Value)
	if err := m.validateRule(r); err != nil {
		return err
	}
	if err := m.DB.UpdateDNSRule(r); err != nil {
		return err
	}
	m.Logf("修改 DNS 规则 %d", r.ID)
	return nil
}

// DeleteRule 删除 DNS 规则。
func (m *Manager) DeleteRule(id int64) error {
	if err := m.DB.DeleteDNSRule(id); err != nil {
		return err
	}
	m.Logf("删除 DNS 规则 %d", id)
	return nil
}

// SetRuleEnabled 启用/停用 DNS 规则。
func (m *Manager) SetRuleEnabled(id int64, enabled bool) error {
	rules, err := m.DB.ListDNSRules()
	if err != nil {
		return err
	}
	for _, r := range rules {
		if r.ID == id {
			r.Enabled = enabled
			return m.DB.UpdateDNSRule(r)
		}
	}
	return storage.ErrNotFound
}

// MoveRule 移动 DNS 规则位置（delta: -1 上移 / +1 下移）。
func (m *Manager) MoveRule(id int64, delta int) error {
	rules, err := m.DB.ListDNSRules()
	if err != nil {
		return err
	}
	idx := -1
	for i, r := range rules {
		if r.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return storage.ErrNotFound
	}
	next := idx + delta
	if next < 0 || next >= len(rules) {
		return nil
	}
	rules[idx], rules[next] = rules[next], rules[idx]
	for i, r := range rules {
		r.Position = i
		if err := m.DB.UpdateDNSRule(r); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) validateRule(r *config.DNSRule) error {
	if !ruleTypes[r.Type] {
		return validation.New("type", "类型 %q 不支持（domain/domain_suffix/domain_keyword/rule_set）", r.Type)
	}
	if r.Value == "" {
		return validation.New("value", "规则值不能为空")
	}
	if r.Server == "" {
		return validation.New("server", "必须指定 DNS 服务器")
	}
	servers, err := m.DB.ListDNSServers()
	if err != nil {
		return err
	}
	for _, s := range servers {
		if s.Tag == r.Server {
			if !s.Enabled {
				return validation.New("server", "DNS 服务器 %q 已停用", r.Server)
			}
			return nil
		}
	}
	return validation.New("server", "DNS 服务器 %q 不存在", r.Server)
}

// --- 全局配置（settings 键） ---

// LoadConfig 组装 DNS 全局配置：服务器、规则、策略、FakeIP、final。
func (m *Manager) LoadConfig() (config.DNSConfig, error) {
	cfg := config.DNSConfig{
		Strategy:    DefaultStrategy,
		FakeIPRange: DefaultFakeIPRange,
	}
	all, err := m.DB.AllSettings()
	if err != nil {
		return cfg, err
	}
	if v, ok := all[keyStrategy]; ok && v != "" {
		cfg.Strategy = v
	}
	if v, ok := all[keyFakeIPOn]; ok {
		cfg.FakeIPEnabled = v == "true"
	}
	if v, ok := all[keyFakeIPRange]; ok && v != "" {
		cfg.FakeIPRange = v
	}
	if v, ok := all[keyFinal]; ok {
		cfg.Final = v
	}
	servers, err := m.DB.ListDNSServers()
	if err != nil {
		return cfg, err
	}
	for _, s := range servers {
		if s.Enabled {
			cfg.Servers = append(cfg.Servers, *s)
		}
	}
	rules, err := m.DB.ListDNSRules()
	if err != nil {
		return cfg, err
	}
	for _, r := range rules {
		if r.Enabled {
			cfg.Rules = append(cfg.Rules, *r)
		}
	}
	return cfg, nil
}

// SaveOptions 保存 DNS 全局选项（策略/FakeIP/final）。
func (m *Manager) SaveOptions(strategy string, fakeIPEnabled bool, fakeIPRange, final string) error {
	strategy = strings.TrimSpace(strategy)
	switch strategy {
	case "prefer_ipv4", "prefer_ipv6", "ipv4_only", "ipv6_only":
	default:
		return validation.New("strategy", "strategy 非法（prefer_ipv4/prefer_ipv6/ipv4_only/ipv6_only）")
	}
	if fakeIPEnabled {
		fakeIPRange = strings.TrimSpace(fakeIPRange)
		if fakeIPRange == "" {
			return validation.New("range", "启用 FakeIP 时必须填写网段")
		}
		if _, network, err := net.ParseCIDR(fakeIPRange); err != nil || network.IP.To4() == nil {
			return validation.New("range", "FakeIP IPv4 网段非法: %q", fakeIPRange)
		}
	}
	if final != "" {
		servers, err := m.DB.ListDNSServers()
		if err != nil {
			return err
		}
		found := false
		for _, s := range servers {
			if s.Tag == final && s.Enabled {
				found = true
				break
			}
		}
		if !found {
			return validation.New("final", "final %q 不是已启用的 DNS 服务器", final)
		}
	}
	pairs := map[string]string{
		keyStrategy:    strategy,
		keyFakeIPOn:    fmt.Sprintf("%v", fakeIPEnabled),
		keyFakeIPRange: strings.TrimSpace(fakeIPRange),
		keyFinal:       strings.TrimSpace(final),
	}
	for k, v := range pairs {
		if err := m.DB.SetSetting(k, v); err != nil {
			return err
		}
	}
	m.Logf("DNS 全局选项已保存")
	return nil
}

// ListServers / ListRules 供 TUI 展示全部条目（含停用）。
func (m *Manager) ListServers() ([]*config.DNSServer, error) { return m.DB.ListDNSServers() }
func (m *Manager) ListRules() ([]*config.DNSRule, error)     { return m.DB.ListDNSRules() }

func (m *Manager) getServer(id int64) (*config.DNSServer, error) {
	list, err := m.DB.ListDNSServers()
	if err != nil {
		return nil, err
	}
	for _, s := range list {
		if s.ID == id {
			return s, nil
		}
	}
	return nil, storage.ErrNotFound
}
