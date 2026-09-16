package storage

// migrations 按序排列的 schema 升级脚本；下标 i 对应 schema 版本 i+1。
// 只允许追加，不允许修改历史条目。
var migrations = [][]string{
	// v1: 初始 schema
	{
		`CREATE TABLE subscriptions (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			name         TEXT    NOT NULL UNIQUE,
			url          TEXT    NOT NULL,
			enabled      INTEGER NOT NULL DEFAULT 1,
			user_agent   TEXT    NOT NULL DEFAULT '',
			last_updated DATETIME,
			node_count   INTEGER NOT NULL DEFAULT 0,
			created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,

		`CREATE TABLE nodes (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			subscription_id INTEGER REFERENCES subscriptions(id) ON DELETE CASCADE,
			name            TEXT    NOT NULL,
			protocol        TEXT    NOT NULL,
			server          TEXT    NOT NULL,
			port            INTEGER NOT NULL,
			tls             INTEGER NOT NULL DEFAULT 0,
			transport       TEXT    NOT NULL DEFAULT '',
			enabled         INTEGER NOT NULL DEFAULT 1,
			latency_ms      INTEGER,
			last_tested     DATETIME,
			metadata        TEXT    NOT NULL DEFAULT '{}',
			created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX idx_nodes_subscription ON nodes(subscription_id)`,
		`CREATE INDEX idx_nodes_protocol     ON nodes(protocol)`,

		`CREATE TABLE proxy_groups (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			name       TEXT    NOT NULL UNIQUE,
			type       TEXT    NOT NULL CHECK (type IN ('select','urltest','fallback','loadbalance')),
			test_url   TEXT    NOT NULL DEFAULT '',
			interval_s INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,

		`CREATE TABLE proxy_group_members (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			group_id    INTEGER NOT NULL REFERENCES proxy_groups(id) ON DELETE CASCADE,
			member_type TEXT    NOT NULL CHECK (member_type IN ('node','group')),
			member_id   INTEGER NOT NULL,
			position    INTEGER NOT NULL DEFAULT 0,
			UNIQUE (group_id, member_type, member_id)
		)`,
		`CREATE INDEX idx_pgm_group ON proxy_group_members(group_id)`,

		`CREATE TABLE routing_groups (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			name       TEXT    NOT NULL UNIQUE,
			target     TEXT    NOT NULL,
			position   INTEGER NOT NULL DEFAULT 0,
			enabled    INTEGER NOT NULL DEFAULT 1,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,

		`CREATE TABLE rules (
			id               INTEGER PRIMARY KEY AUTOINCREMENT,
			routing_group_id INTEGER NOT NULL REFERENCES routing_groups(id) ON DELETE CASCADE,
			rule_type        TEXT    NOT NULL CHECK (rule_type IN
				('domain','domain_suffix','domain_keyword','ip_cidr','geoip','geosite','rule_set','final')),
			value       TEXT    NOT NULL,
			invert      INTEGER NOT NULL DEFAULT 0,
			enabled     INTEGER NOT NULL DEFAULT 1,
			position    INTEGER NOT NULL DEFAULT 0,
			created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX idx_rules_group ON rules(routing_group_id)`,

		`CREATE TABLE rulesets (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			name        TEXT    NOT NULL UNIQUE,
			tag         TEXT    NOT NULL UNIQUE,
			source_type TEXT    NOT NULL DEFAULT 'remote' CHECK (source_type IN ('remote','local')),
			format      TEXT    NOT NULL DEFAULT 'srs' CHECK (format IN ('srs','json')),
			url         TEXT    NOT NULL DEFAULT '',
			enabled     INTEGER NOT NULL DEFAULT 1,
			cached_path TEXT    NOT NULL DEFAULT '',
			updated_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,

		`CREATE TABLE dns_servers (
			id                INTEGER PRIMARY KEY AUTOINCREMENT,
			tag               TEXT    NOT NULL UNIQUE,
			address           TEXT    NOT NULL,
			address_resolver  TEXT    NOT NULL DEFAULT '',
			detour            TEXT    NOT NULL DEFAULT '',
			enabled           INTEGER NOT NULL DEFAULT 1,
			position          INTEGER NOT NULL DEFAULT 0
		)`,

		`CREATE TABLE settings (
			key        TEXT PRIMARY KEY,
			value      TEXT NOT NULL,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
	},

	// v2: 代理组增加选中项持久化；成员类型放开 all（动态全部节点）
	{
		`ALTER TABLE proxy_groups ADD COLUMN selected TEXT NOT NULL DEFAULT ''`,

		`CREATE TABLE proxy_group_members_new (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			group_id    INTEGER NOT NULL REFERENCES proxy_groups(id) ON DELETE CASCADE,
			member_type TEXT    NOT NULL CHECK (member_type IN ('node','group','all')),
			member_id   INTEGER NOT NULL,
			position    INTEGER NOT NULL DEFAULT 0,
			UNIQUE (group_id, member_type, member_id)
		)`,
		`INSERT INTO proxy_group_members_new (id, group_id, member_type, member_id, position)
		 SELECT id, group_id, member_type, member_id, position FROM proxy_group_members`,
		`DROP TABLE proxy_group_members`,
		`ALTER TABLE proxy_group_members_new RENAME TO proxy_group_members`,
		`CREATE INDEX idx_pgm_group ON proxy_group_members(group_id)`,
	},

	// v3: DNS 规则表 + DNS 服务器类型列（1.12+ 新版格式按 type 分流）
	{
		`CREATE TABLE dns_rules (
			id       INTEGER PRIMARY KEY AUTOINCREMENT,
			rule_type TEXT   NOT NULL CHECK (rule_type IN ('domain','domain_suffix','domain_keyword','rule_set')),
			value    TEXT   NOT NULL,
			server   TEXT   NOT NULL,
			enabled  INTEGER NOT NULL DEFAULT 1,
			position INTEGER NOT NULL DEFAULT 0
		)`,
		`ALTER TABLE dns_servers ADD COLUMN type TEXT NOT NULL DEFAULT 'udp'`,
	},

	// v4: 逻辑规则：rule_type 放开 logical、rules 增加 mode 列、新增子条件表
	{
		`CREATE TABLE rules_new (
			id               INTEGER PRIMARY KEY AUTOINCREMENT,
			routing_group_id INTEGER NOT NULL REFERENCES routing_groups(id) ON DELETE CASCADE,
			rule_type        TEXT    NOT NULL CHECK (rule_type IN
				('domain','domain_suffix','domain_keyword','ip_cidr','geoip','geosite','rule_set','final','logical')),
			value       TEXT    NOT NULL,
			mode        TEXT    NOT NULL DEFAULT '',
			invert      INTEGER NOT NULL DEFAULT 0,
			enabled     INTEGER NOT NULL DEFAULT 1,
			position    INTEGER NOT NULL DEFAULT 0,
			created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`INSERT INTO rules_new (id, routing_group_id, rule_type, value, mode, invert, enabled, position, created_at)
		 SELECT id, routing_group_id, rule_type, value, '', invert, enabled, position, created_at FROM rules`,
		`DROP TABLE rules`,
		`ALTER TABLE rules_new RENAME TO rules`,
		`CREATE INDEX idx_rules_group ON rules(routing_group_id)`,

		`CREATE TABLE rule_conditions (
			id        INTEGER PRIMARY KEY AUTOINCREMENT,
			rule_id   INTEGER NOT NULL REFERENCES rules(id) ON DELETE CASCADE,
			cond_type TEXT    NOT NULL CHECK (cond_type IN
				('domain','domain_suffix','domain_keyword','ip_cidr','geoip','geosite','rule_set')),
			value     TEXT    NOT NULL DEFAULT '',
			invert    INTEGER NOT NULL DEFAULT 0,
			position  INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX idx_rule_conditions_rule ON rule_conditions(rule_id)`,
	},

	// v5: 规则类型放开 domain_regex（rules 与 rule_conditions 的 CHECK 约束都要重建）。
	// 顺序要点：foreign_keys=ON 时 DROP TABLE rules 会隐式 DELETE FROM 并触发
	// rule_conditions 的 ON DELETE CASCADE，子条件会被连带删除。故先把子条件搬到
	// 无约束的临时表并删掉子表，重建 rules 后再重建子表回填。
	{
		`CREATE TABLE rule_conditions_backup AS SELECT * FROM rule_conditions`,
		`DROP TABLE rule_conditions`,

		// 清理孤儿规则（所属分流组已不存在）：新表的外键会让下面的 INSERT 整体失败，
		// 迁移随之失败、应用无法启动。这类行本就不可达（外部工具在 foreign_keys=OFF
		// 下删除分流组即可产生），直接丢弃。
		`DELETE FROM rules WHERE routing_group_id NOT IN (SELECT id FROM routing_groups)`,

		`CREATE TABLE rules_new (
			id               INTEGER PRIMARY KEY AUTOINCREMENT,
			routing_group_id INTEGER NOT NULL REFERENCES routing_groups(id) ON DELETE CASCADE,
			rule_type        TEXT    NOT NULL CHECK (rule_type IN
				('domain','domain_suffix','domain_keyword','domain_regex','ip_cidr','geoip','geosite','rule_set','final','logical')),
			value       TEXT    NOT NULL,
			mode        TEXT    NOT NULL DEFAULT '',
			invert      INTEGER NOT NULL DEFAULT 0,
			enabled     INTEGER NOT NULL DEFAULT 1,
			position    INTEGER NOT NULL DEFAULT 0,
			created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`INSERT INTO rules_new (id, routing_group_id, rule_type, value, mode, invert, enabled, position, created_at)
		 SELECT id, routing_group_id, rule_type, value, mode, invert, enabled, position, created_at FROM rules`,
		`DROP TABLE rules`,
		`ALTER TABLE rules_new RENAME TO rules`,
		`CREATE INDEX idx_rules_group ON rules(routing_group_id)`,

		`CREATE TABLE rule_conditions (
			id        INTEGER PRIMARY KEY AUTOINCREMENT,
			rule_id   INTEGER NOT NULL REFERENCES rules(id) ON DELETE CASCADE,
			cond_type TEXT    NOT NULL CHECK (cond_type IN
				('domain','domain_suffix','domain_keyword','domain_regex','ip_cidr','geoip','geosite','rule_set')),
			value     TEXT    NOT NULL DEFAULT '',
			invert    INTEGER NOT NULL DEFAULT 0,
			position  INTEGER NOT NULL DEFAULT 0
		)`,
		`INSERT INTO rule_conditions (id, rule_id, cond_type, value, invert, position)
		 SELECT id, rule_id, cond_type, value, invert, position FROM rule_conditions_backup
		 WHERE rule_id IN (SELECT id FROM rules)`,
		`DROP TABLE rule_conditions_backup`,
		`CREATE INDEX idx_rule_conditions_rule ON rule_conditions(rule_id)`,
	},

	// v6: 订阅高级更新策略与流量信息。
	{
		`ALTER TABLE subscriptions ADD COLUMN node_filter TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE subscriptions ADD COLUMN sort_by TEXT NOT NULL DEFAULT 'name'`,
		`ALTER TABLE subscriptions ADD COLUMN auto_test INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE subscriptions ADD COLUMN auto_clean INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE subscriptions ADD COLUMN traffic_upload INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE subscriptions ADD COLUMN traffic_download INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE subscriptions ADD COLUMN traffic_total INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE subscriptions ADD COLUMN expire_at DATETIME`,
	},

	// v7: 订阅级下载通道策略，默认代理优先、代理失败回退直连。
	{
		`ALTER TABLE subscriptions ADD COLUMN download_strategy TEXT NOT NULL DEFAULT 'prefer_proxy'`,
	},

	// v8: 分流组分层（Kind）——移植 karing 的「层序」模型。
	// kind_rank 是 config.KindRank 的冗余列：SQLite 不能调用 Go 函数，把层序
	// 落成整数列才能继续把排序下推到 SQL。Position 语义同步改为「层内序号」。
	// 存量数据的回填需要「哪个组是 final」这层判断，放在 postMigrations[8]
	// （见 routing_layers.go），与这里的 DDL 同处一个事务。
	{
		`ALTER TABLE routing_groups ADD COLUMN kind TEXT NOT NULL DEFAULT 'custom'`,
		`ALTER TABLE routing_groups ADD COLUMN kind_rank INTEGER NOT NULL DEFAULT 0`,
		`CREATE INDEX idx_routing_groups_order ON routing_groups(kind_rank, position, id)`,
	},
}
