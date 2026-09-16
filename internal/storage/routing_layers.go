package storage

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/bbbstyyy/karing-tui/internal/config"
)

// postMigrations 登记「除 DDL 之外还需要 Go 侧判定」的迁移收尾步骤，键为目标
// schema 版本。migrations 本身只放纯 SQL，涉及数据语义判断（哪个组是 final）与
// 层内重排的回填放在这里；执行仍在 migrate() 的同一个 BEGIN IMMEDIATE 事务内，
// 失败即整体回滚。幂等由版本号保证：只有从旧版本升上来时才会走一次。
var postMigrations = map[int]func(context.Context, *sql.Conn) error{
	8: backfillRoutingLayers,
}

// layerBackfillRow 回填使用的分流组行。
type layerBackfillRow struct {
	id       int64
	name     string
	position int
}

// backfillRoutingLayers 把 v8 之前的分流组数据升级到分层模型：
//
//  1. 含**启用中 final 规则**的组 → kind='final'（层序 4）。全库这样的组超过一个
//     是坏数据（生成器 generate.go 的 buildRoute 本就报错），这里直接中止迁移并
//     列出组名，绝不自行挑一个——否则迁移会让用户的分流方案静默改变。
//  2. 其余组 → kind='custom'（层序 0），与 karing「预置与手工规则一律进 custom 层」
//     的归属规则一致。
//  3. 各层内 position 按迁移前的 (position, id) 顺序重排为连续值 0..n-1，使迁移
//     前后生成的 route 规则**逐条一致**（老方案的 4 组本来全部 position=0，靠 id
//     决胜；重排后仍保持 insertion 顺序）。
//  4. 非 final 组里残留的 final 规则一律删除：引入 Kind 后 final 语义收紧为
//     「kind='final' 的组必须且仅有一条 final 规则」，停用中的 final 规则本就不产出
//     任何路由规则，留着只会让该组在分层模型下变成非法数据（无法再保存编辑）。
func backfillRoutingLayers(ctx context.Context, conn *sql.Conn) error {
	rows, err := conn.QueryContext(ctx, `SELECT id, name, position FROM routing_groups`)
	if err != nil {
		return fmt.Errorf("读取分流组失败: %w", err)
	}
	var groups []layerBackfillRow
	for rows.Next() {
		var g layerBackfillRow
		if err := rows.Scan(&g.id, &g.name, &g.position); err != nil {
			rows.Close()
			return fmt.Errorf("扫描分流组失败: %w", err)
		}
		groups = append(groups, g)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	finalRows, err := conn.QueryContext(ctx,
		`SELECT DISTINCT routing_group_id FROM rules WHERE rule_type='final' AND enabled=1`)
	if err != nil {
		return fmt.Errorf("读取 final 规则失败: %w", err)
	}
	activeFinal := map[int64]bool{}
	for finalRows.Next() {
		var id int64
		if err := finalRows.Scan(&id); err != nil {
			finalRows.Close()
			return fmt.Errorf("扫描 final 规则失败: %w", err)
		}
		activeFinal[id] = true
	}
	if err := finalRows.Err(); err != nil {
		finalRows.Close()
		return err
	}
	finalRows.Close()

	if len(activeFinal) > 1 {
		names := make([]string, 0, len(activeFinal))
		for _, g := range groups {
			if activeFinal[g.id] {
				names = append(names, g.name)
			}
		}
		sort.Strings(names)
		return fmt.Errorf("发现 %d 个含活动 final 规则的分流组（%s）：活动 final 分流组只能有一个，请先在旧版本中修正后再升级",
			len(names), strings.Join(names, "、"))
	}

	var finalID int64
	for id := range activeFinal {
		finalID = id
	}

	// 按 (position, id) 还原迁移前的全局顺序，再逐层重排为连续值。
	sort.SliceStable(groups, func(i, j int) bool {
		if groups[i].position != groups[j].position {
			return groups[i].position < groups[j].position
		}
		return groups[i].id < groups[j].id
	})
	counters := map[string]int{}
	for _, g := range groups {
		kind := config.KindCustom
		if g.id == finalID {
			kind = config.KindFinal
		}
		position := counters[kind]
		counters[kind]++
		if _, err := conn.ExecContext(ctx,
			`UPDATE routing_groups SET kind=?, kind_rank=?, position=? WHERE id=?`,
			kind, config.KindRank(kind), position, g.id); err != nil {
			return fmt.Errorf("回填分流组 %q 失败: %w", g.name, err)
		}
		if kind == config.KindCustom {
			if _, err := conn.ExecContext(ctx,
				`DELETE FROM rules WHERE routing_group_id=? AND rule_type='final'`, g.id); err != nil {
				return fmt.Errorf("清理分流组 %q 的 final 规则失败: %w", g.name, err)
			}
		}
	}
	return nil
}
