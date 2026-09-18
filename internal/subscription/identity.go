package subscription

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bbbstyyy/karing-tui/internal/config"
)

// identityOutboundTag 是计算身份指纹时使用的占位出站 tag。
// 它只活在一次内存中的 map 里，不会进入任何真实配置。
const identityOutboundTag = "__identity__"

// nodeFingerprint 返回节点的「连接语义指纹」：两个节点在服务端看来是否等价。
// 等价则指纹相同，只有指纹相同的节点之间才允许互相继承数据库 ID。
//
// 为什么不是 `protocol|server|port`（旧的 nodeKey）：同一台 example.com:443 上
// 可以并存多个不同凭据的节点（VLESS UUID A / UUID B，或 Trojan 的两个密码）。
// 旧键把它们当成同一个身份，订阅刷新时会把 A 的 ID、禁用状态与测速结果错绑到 B
// 上；最严重的表现是显式代理组的 `node:<id>` 引用悄悄换成了另一个语义节点，
// 而界面上看不出任何异常。
//
// 为什么用 NodeToOutbound 而不是自己列字段白名单：NodeToOutbound 是「节点 →
// 真实连接参数」的唯一集中来源。将来新增协议字段时身份规则自动跟随，不会出现
// 「配置里变了、身份却没变」的分叉。这里刻意不写 uuid/password/sni/path 清单。
//
// 返回值是 SHA-256 的 hex，**不是 JSON**：JSON 会带 UUID、密码等凭据，
// 一旦作为 map key 或进入日志与错误上下文就是凭据泄露。
//
// 刻意保持的边界：只把 Protocol / Server 归一为 lower+trim。
// Transport 必须原样参与——`config.transportBlock` 是按字面值 switch 的
// （"ws"/"http"/"grpc"/"httpupgrade"/"quic"），在这里顺手 ToLower 会让身份
// 与真实生成的出站分叉：两个节点会被判为同一个身份，但生成的配置一个带传输层、
// 一个不带。归一规则必须与生成规则同源，要么都归一，要么都不归一。
func nodeFingerprint(n *config.Node) (string, error) {
	if n == nil {
		return "", fmt.Errorf("节点为空，无法计算身份指纹")
	}
	clone := *n // 浅复制：NodeToOutbound 只读，但归一化不得污染调用方对象
	clone.Protocol = strings.ToLower(strings.TrimSpace(clone.Protocol))
	clone.Server = strings.ToLower(strings.TrimSpace(clone.Server))

	out, err := config.NodeToOutbound(&clone, identityOutboundTag)
	if err != nil {
		return "", fmt.Errorf("计算节点 %q 身份指纹失败: %w", n.Name, err)
	}
	delete(out, "tag") // tag 由调用方决定，不属于连接语义

	raw, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("序列化节点 %q 身份指纹失败: %w", n.Name, err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// takeBucketNode 从同一个指纹桶里取走一个旧节点，返回剩余桶。
// 优先挑「展示名称相同」的旧节点（用户只改备注时能留住 ID），
// 否则取 ID 最小的未使用者。被取走的元素从桶里删除，
// 因此每个旧 ID 最多被消费一次。
func takeBucketNode(bucket []*config.Node, name string) (*config.Node, []*config.Node) {
	for i, o := range bucket {
		if o.Name == name {
			// 三下标切片：让 append 分配新数组，避免与 bucket 共享底层数组。
			return o, append(bucket[:i:i], bucket[i+1:]...)
		}
	}
	if len(bucket) == 0 {
		return nil, bucket
	}
	return bucket[0], bucket[1:]
}
