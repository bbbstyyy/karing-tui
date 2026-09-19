package config

import (
	"fmt"
	"strings"
)

// ValidateDNSResolverGraph 检查 DNS 服务器之间的 domain_resolver（AddressResolver）
// 引用是否成环。
//
// 为什么必须显式检（真实内核实测，2026-09-19，本机内核 revision cf69a007）：
//
//	sing-box check  <cfg>  → rc=0      （只校验解码，看不出环）
//	sing-box run    <cfg>  → FATAL start service: circular server dependency: a -> b -> a
//
// 即环只在 run 阶段暴露，与 V6 轮「detour=direct / 悬空 domain_resolver / 悬空 detour
// 全都 check rc=0、只有 run 报 FATAL」同源。用户可以在界面里把 A 的 resolver 设成 B、
// 把 B 的设成 A——单条各自都合法（validateServer 只查「引用存在且启用」），
// 组合起来成环，直到启动内核才炸，而且那时报的是一条与用户操作无关的内核错误。
//
// 入参语义：**传入的每个元素都视为会进配置**（不看 Enabled）。
// 调用方负责只传「将要被输出/将会生效」的那批服务器——两个调用点需要过滤的集合
// 不同（生成器传全部待输出项，界面层传启用项 + 候选），把过滤放在调用方更清楚。
//
// 只报「环」这一件事：引用不存在的 tag 由生成器的悬空引用校验负责，
// 两件事分开报错，用户才知道该改哪里。
func ValidateDNSResolverGraph(servers []DNSServer) error {
	byTag := make(map[string]DNSServer, len(servers))
	for _, s := range servers {
		byTag[s.Tag] = s
	}
	for _, start := range servers {
		path := make([]string, 0, len(servers)+1)
		onPath := make(map[string]bool, len(servers))
		for cur := start; ; {
			if onPath[cur.Tag] {
				return fmt.Errorf("DNS 服务器 %q 的 domain_resolver 引用成环: %s",
					start.Tag, strings.Join(append(path, cur.Tag), " -> "))
			}
			onPath[cur.Tag] = true
			path = append(path, cur.Tag)
			// 只在**真的会被用到**时才沿引用走链（V8-1）：地址是字面 IP 的服务器
			// 根本不输出 domain_resolver，它上面残留的值不该产生一条边——
			// 否则「两条只要字面 IP 的服务器互相留了个残留引用」会被误判成环。
			if cur.AddressResolver == "" || !DNSServerNeedsDomainResolver(&cur) {
				break
			}
			next, ok := byTag[cur.AddressResolver]
			if !ok {
				// 指向集合外的 tag：交给悬空引用校验，不算环。
				break
			}
			cur = next
		}
	}
	return nil
}

// ApplyResolverTagCascade 返回一份按「重命名级联」改写后的服务器副本，
// 复刻 storage.UpdateDNSServerRenamed 里那条
//
//	UPDATE dns_servers SET address_resolver = newTag WHERE address_resolver = oldTag
//
// 的语义：把所有 AddressResolver == oldTag 的条目改写为 newTag。
//
// 为什么必须存在（V9-2）：判环看到的图必须与**提交后**的图一致。改名时级联会在
// 提交阶段把「指向旧 tag」的边一起改向，若校验侧不镜像这一步，就会漏掉一条边——
// 实测夹具（A→""、X→A；把 A 改名 C 且 C→X）提交后是 C→X→C，而旧校验认为
// 「C→X→A（A 不存在）」，放行入库。真实内核只在 run 阶段报
// "circular server dependency"（check 返回 0）。
//
// 顺序约束：调用方必须**先**把被改名的条目替换成候选，**再**调用本函数。
// 级联 SQL 在行更新之后执行，所以候选自身的 AddressResolver 同样会被改写
// （该情形已被「不能把自身作为 resolver」挡住，但顺序写反仍会让边界条件与 SQL 漂移）。
//
// 总是返回新切片，不复用入参底层数组；不改变除 AddressResolver 之外的任何字段。
// 注意：只改 Enabled 的过滤与本函数可交换（两者作用在不同字段上），因此调用方
// 可以先把候选替换进「将生效的那批」，再做级联。
func ApplyResolverTagCascade(servers []DNSServer, oldTag, newTag string) []DNSServer {
	out := make([]DNSServer, len(servers))
	copy(out, servers)
	if oldTag == "" || oldTag == newTag {
		return out
	}
	for i := range out {
		if out[i].AddressResolver == oldTag {
			out[i].AddressResolver = newTag
		}
	}
	return out
}
