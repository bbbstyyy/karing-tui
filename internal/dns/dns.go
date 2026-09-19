// Package dns 实现 DNS 配置管理：DNS 服务器（含 DoH/DoQ）、DNS 规则、
// FakeIP 与全局策略。全局选项持久化在 settings 键值表。
package dns

import (
	"context"
	"database/sql"
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
//
// 四步写必须在同一个事务里（V7-3）：任一步失败留下「只有 local、没有 remote」
// 之类的残片时，下次启动会因 len(servers) > 0 而直接返回，残片被永久固化。
// 「是否已初始化」的判定也在事务内读——WithTx 开的是 IMMEDIATE 事务，
// 两个进程同时首启时只有先拿到写锁的那个能看到「还没有服务器」。
func (m *Manager) EnsureDefaultDNS() error {
	created := false
	err := m.DB.WithTx(context.Background(), func(tx *sql.Tx) error {
		servers, err := m.DB.ListDNSServersTx(tx)
		if err != nil {
			return err
		}
		if len(servers) > 0 {
			return nil
		}
		local := &config.DNSServer{Tag: "local", Type: "udp", Address: "223.5.5.5", Enabled: true, Position: 0}
		if err := m.DB.CreateDNSServerTx(tx, local); err != nil {
			return err
		}
		// 默认 remote 是字面 IP（8.8.8.8），不存在「域名 → DNS → IP」这一步，
		// 因此**不写 AddressResolver**（V8-1）。旧默认值 `AddressResolver: "local"`
		// 是一条假依赖：既多输出一个用不到的 domain_resolver，
		// 又让 local 永远无法被停用或删除。
		remote := &config.DNSServer{
			Tag: "remote", Type: "https", Address: "8.8.8.8",
			Detour: storage.DefaultGroupAuto, Enabled: true, Position: 1,
		}
		if err := m.DB.CreateDNSServerTx(tx, remote); err != nil {
			return err
		}
		rule := &config.DNSRule{Type: "rule_set", Value: "geosite:cn", Server: "local", Enabled: true, Position: 0}
		if err := m.DB.CreateDNSRuleTx(tx, rule); err != nil {
			return err
		}
		// 默认 final 指向 remote
		if err := m.DB.SetSettingTx(tx, keyFinal, "remote"); err != nil {
			return err
		}
		created = true
		return nil
	})
	if err != nil {
		return err
	}
	if created {
		m.Logf("已初始化默认 DNS 配置")
	}
	return nil
}

// --- DNS 服务器 ---

// normalizeServerResolver 在保存前收口 resolver 语义（V8-1）：地址不是域名时，
// AddressResolver 没有用途（内核也不会输出它），直接清空。
//
// 只在**数据入口**（AddServer / UpdateServer）做这件事，而不是在生成/展示时到处兜底：
//   - 用户输入 `https + 8.8.8.8 + 解析DNS=local` 保存后库里就是空的，以后不会再产生
//     新的假依赖；而 `https + dns.google + 解析DNS=local` 原样保留。
//   - 老库里已经存在的残留值由 LoadConfig / ListServers 的副本规范化兜住，
//     不改数据（只读启动路径不允许写库）。
func normalizeServerResolver(s *config.DNSServer) {
	if s == nil {
		return
	}
	if !config.DNSServerNeedsDomainResolver(s) {
		s.AddressResolver = ""
	}
}

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
	// 规范化必须先于校验：否则字面 IP 上的无用 resolver 会先触发
	// 「AddressResolver 已停用 / 不是已存在的 Tag」这类针对假依赖的报错。
	normalizeServerResolver(s)
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
	// 新增：没有旧 tag，改名级联不适用（oldTag 传空）。
	if err := m.validateResolverGraph(s, 0, ""); err != nil {
		return nil, err
	}
	if err := m.DB.CreateDNSServer(s); err != nil {
		return nil, err
	}
	m.Logf("添加 DNS 服务器 %q (%s)", s.Tag, s.Type)
	return s, nil
}

// validateResolverGraph 用「候选服务器替换/加入现有集合后」的视图判环（V7-8）。
//
// 必须在**写库之前**调用：写进去之后每一条单独看都合法（validateServer 只查
// 「引用存在且启用」），只有启动内核才会炸——而真实内核实测表明那时报的是
// "circular server dependency"（check 阶段返回 0，看不出问题）。
//
// 只把**启用**的服务器纳入图：禁用的不进生成配置，也不该拦住用户保存。
// selfID 为 0 表示新增；否则是「用 candidate 替换 ID=selfID 的那一条」。
//
// oldTag 非空且与 candidate.Tag 不同 = 这是一次**改名**，提交阶段会级联改写所有
// 「指向旧 tag」的条目（V9-2）。此时必须先把候选替换进图、再镜像级联，否则看到的
// 是「改名前的库 + 改名的自己」，会漏掉一条边而放行进环。（只对启用项做级联与
// 先全量级联再过滤等价：级联作用在 AddressResolver 上，Enabled 过滤作用在另一个
// 字段上，两者可交换。）
func (m *Manager) validateResolverGraph(candidate *config.DNSServer, selfID int64, oldTag string) error {
	servers, err := m.DB.ListDNSServers()
	if err != nil {
		return err
	}
	graph := make([]config.DNSServer, 0, len(servers)+1)
	replaced := false
	for _, s := range servers {
		if !s.Enabled {
			continue
		}
		if selfID != 0 && s.ID == selfID {
			replaced = true
			// 候选把自己停用了：它不再进配置，也就不参与判环。
			if candidate.Enabled {
				graph = append(graph, *candidate)
			}
			continue
		}
		graph = append(graph, *s)
	}
	if !replaced && candidate.Enabled {
		graph = append(graph, *candidate)
	}
	if oldTag != "" && oldTag != candidate.Tag {
		graph = config.ApplyResolverTagCascade(graph, oldTag, candidate.Tag)
	}
	return config.ValidateDNSResolverGraph(graph)
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
	// 同 AddServer：先规范化再校验（V8-1）。把地址从域名改成字面 IP 时，
	// 旧的 resolver 会在这里被清掉，不会作为假依赖留在库里。
	normalizeServerResolver(s)
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
			// 只有**真的会用**这个 resolver 的服务器才算依赖（V8-1）：
			// 地址是字面 IP 的那些人不会输出 domain_resolver，
			// 它们身上残留的值不该把别的服务器锁死。
			if other.ID != old.ID &&
				config.DNSServerNeedsDomainResolver(other) &&
				other.AddressResolver == old.Tag {
				return fmt.Errorf("DNS 服务器 %q 的 AddressResolver 正在引用 %q，请先修改引用", other.Tag, old.Tag)
			}
		}
	}
	// 写库前判环（V7-8）：单条 update 都可能把「A→B」补成环，必须拦住。
	// 传 old.Tag 是为了让校验看到**提交后**的图：改名提交时级联会改写所有
	// 「指向旧 tag」的条目（V9-2）。
	if err := m.validateResolverGraph(s, s.ID, old.Tag); err != nil {
		return err
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
		// 同 UpdateServer：字面 IP 上的残留 resolver 不构成真实依赖（V8-1）。
		if other.ID != s.ID &&
			config.DNSServerNeedsDomainResolver(other) &&
			other.AddressResolver == s.Tag {
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
//
// 启用方向必须与 AddServer/UpdateServer 走同一套校验（V9-1）：否则用户可以把两条都
// **停用**的服务器先编成互相引用（停用状态下编辑不做「引用的 resolver 必须启用」检查），
// 再依次启用，从而把一张「启用项引用停用项」甚至成环的图写进库 ——
// V7-8 的「环必须在写库之前拦住」正是从这里被绕过的。后果要等到生成配置才暴露，
// 而报错内容与用户刚才那个「启用」动作看不出关系。
//
// 停用方向**刻意不加 validateServer**（不是漏了）：停用是脏数据行唯一的自救动作，
// 若因为「自身那条悬空/停用的 resolver」而被拒绝，用户会被锁死在「既不能用、也不能停」。
// 停用只会让生效图变小，造不出环，因此这里连判环也不需要。
//
// 两个分支都写规范化后的 candidate 而非原始行：与另两条入口同序（规范化先于校验），
// 顺带把老库里「字面 IP + 残留 resolver」收敛掉。启用/停用是用户动作，
// 不是只读路径，写回规范化结果是允许的（V8-1 的「只读路径不写库」说的是 LoadConfig）。
func (m *Manager) SetServerEnabled(id int64, enabled bool) error {
	s, err := m.getServer(id)
	if err != nil {
		return err
	}
	candidate := *s
	normalizeServerResolver(&candidate)
	candidate.Enabled = enabled

	if !enabled {
		cfg, err := m.LoadConfig()
		if err != nil {
			return err
		}
		if cfg.Final == candidate.Tag {
			return fmt.Errorf("DNS 服务器 %q 是当前默认（final）服务器，请先修改默认 DNS", candidate.Tag)
		}
		rules, err := m.DB.ListDNSRules()
		if err != nil {
			return err
		}
		for _, r := range rules {
			if r.Server == candidate.Tag {
				return fmt.Errorf("DNS 规则正在引用服务器 %q，请先修改或删除对应规则", candidate.Tag)
			}
		}
		servers, err := m.DB.ListDNSServers()
		if err != nil {
			return err
		}
		for _, other := range servers {
			// 同 UpdateServer：字面 IP 上的残留 resolver 不构成真实依赖（V8-1）。
			if other.ID != candidate.ID &&
				config.DNSServerNeedsDomainResolver(other) &&
				other.AddressResolver == candidate.Tag {
				return fmt.Errorf("DNS 服务器 %q 的 AddressResolver 正在引用 %q，请先修改引用", other.Tag, candidate.Tag)
			}
		}
	} else {
		if err := m.validateServer(&candidate, candidate.ID); err != nil {
			return err
		}
		// 启用不改 tag，oldTag 传空（改名级联不适用）。
		if err := m.validateResolverGraph(&candidate, candidate.ID, ""); err != nil {
			return err
		}
	}
	if err := m.DB.UpdateDNSServer(&candidate); err != nil {
		return err
	}
	state := "启用"
	if !enabled {
		state = "停用"
	}
	m.Logf("DNS 服务器 %q 已%s", candidate.Tag, state)
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
//
// 重新编号必须整体原子（V7-3）：中间失败会留下重复 position，而 ListDNSRules
// 按 (position, id) 排序，重复值会让界面顺序与用户操作不一致且无法自愈。
func (m *Manager) MoveRule(id int64, delta int) error {
	return m.DB.WithTx(context.Background(), func(tx *sql.Tx) error {
		rules, err := m.DB.ListDNSRulesTx(tx)
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
			if err := m.DB.UpdateDNSRuleTx(tx, r); err != nil {
				return err
			}
		}
		return nil
	})
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
		if !s.Enabled {
			continue
		}
		// 复制后再规范化（V8-1）：老库里字面 IP 上的残留 AddressResolver 不得进入
		// 生成 / 测速的输入；同时**不写库**——OpenQueryOnly 与备份恢复路径也会走到
		// 这里，只读路径不允许偷偷改数据。落库留给用户下次真正编辑保存。
		server := *s
		normalizeServerResolver(&server)
		cfg.Servers = append(cfg.Servers, server)
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
	// 固定顺序的 slice，**不要**改成 map（V7-3）：map 的遍历顺序随机会让
	// 「写入中断点在哪」不可复现，也会让回滚类测试失去确定性。
	// 4 个选项必须同一事务提交，否则用户会拿到「策略新、FakeIP 旧」的混合配置。
	pairs := []struct{ key, value string }{
		{keyStrategy, strategy},
		{keyFakeIPOn, strconv.FormatBool(fakeIPEnabled)},
		{keyFakeIPRange, strings.TrimSpace(fakeIPRange)},
		{keyFinal, strings.TrimSpace(final)},
	}
	if err := m.DB.WithTx(context.Background(), func(tx *sql.Tx) error {
		for _, p := range pairs {
			if err := m.DB.SetSettingTx(tx, p.key, p.value); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	m.Logf("DNS 全局选项已保存")
	return nil
}

// ListServers / ListRules 供 TUI 展示全部条目（含停用）。
//
// ListServers 返回**规范化后的副本**（V8-1）：老库里字面 IP 上的残留 AddressResolver
// 不该在界面上显示成一条真依赖。只改返回值、不写库。
func (m *Manager) ListServers() ([]*config.DNSServer, error) {
	servers, err := m.DB.ListDNSServers()
	if err != nil {
		return nil, err
	}
	for _, s := range servers {
		normalizeServerResolver(s)
	}
	return servers, nil
}

func (m *Manager) ListRules() ([]*config.DNSRule, error) { return m.DB.ListDNSRules() }

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
