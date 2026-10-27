// Package livecfg 运行期可变配置的并发安全持有者。
//
// 背景：进程启动时读入的配置是普通字段（读多写零），但管理面板允许在线改配置，
// 于是少量"可热生效"的字段需要有并发安全的读写点。此处用不可变快照 + atomic 指针：
// 读方 Load 拿到一致视图，写方 Store 整体替换，无锁无数据竞争。
//
// 只承载**读路径深、热改需求强**的少数字段；池参数/排程参数等各有既有 setter
// （pool.SetBreaker、scheduler.Reconfigure 等），不重复收编到这里。
package livecfg

import (
	"strings"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/modelid"
)

// Snapshot 一次读取的不可变配置视图。
type Snapshot struct {
	APIKey               string              // 网关/面板共同鉴权密钥；空 = 不鉴权
	SoftCooldown         time.Duration       // 429 软冷却基数（<=0 时调用方回退内置默认）
	SanitizeFingerprints bool                // 出站请求体指纹脱敏
	SystemAsUserFallback bool                // 撞 11-128 时把 system 搬进 user 角色重试
	RecordClientInfo     bool                // 请求日志是否记录调用来源（客户端 IP / UA）
	HiddenModels         map[string]struct{} // 精确隐藏的模型 id（已归一化为 "cn:x"/"global:x" 全量形式）；nil = 不隐藏
	// ModelIDStyle 模型列表/面板输出的 ID 分隔符（models.id_style）。
	// 零值 = colon（cn:model，零回归）；slash = cn/model（registry 驱动的客户端用）。
	// 只影响展示，不影响请求解析与内部规范键。
	ModelIDStyle modelid.Style
	// RealmNamePrefix 为 true 时列表/面板的 name 前置 "CN · " / "Global · "，
	// 便于两域同 id 的模型在客户端里区分（默认 false，零回归）。
	RealmNamePrefix bool
}

// Holder 原子持有当前快照。
type Holder struct {
	p atomic.Pointer[Snapshot]
}

// New 以初始快照构建。
func New(s Snapshot) *Holder {
	h := &Holder{}
	h.Store(s)
	return h
}

// Load 返回当前快照（Holder 为 nil 或从未 Store 时返回零值快照，调用方无需判空）。
func (h *Holder) Load() Snapshot {
	if h == nil {
		return Snapshot{}
	}
	if s := h.p.Load(); s != nil {
		return *s
	}
	return Snapshot{}
}

// Store 整体替换快照。
func (h *Holder) Store(s Snapshot) { h.p.Store(&s) }

// NormalizeHiddenModels 把用户配置的 hidden 条目归一化为精确 id 集合（规范键 realm:bare）。
//
// 条目带合法 "cn:"/"global:" 前缀（冒号或斜杠两种分隔符都行）→ 只隐藏该域；
// 裸名 → 两域都隐藏。大小写敏感（与 internal/server.resolveModel 的前缀规则
// 对称：只认精确小写枚举；"xx:yy"/"CN/x" 等整体当裸名）。
//
// 归一化到规范键的意义：models.hidden 是**内部状态**，不应随 models.id_style
// 的展示风格变化而失效——客户端用 cn:x 还是 cn/x 打进来，都能命中同一键。
// 返回的 map 只读共享，调用方不得修改。
func NormalizeHiddenModels(hidden []string) map[string]struct{} {
	if len(hidden) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(hidden)*2)
	for _, m := range hidden {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if realm, bare, explicit := modelid.Split(m); explicit {
			out[modelid.Key(realm, bare)] = struct{}{}
		} else {
			out[modelid.Key(modelid.RealmCN, m)] = struct{}{}
			out[modelid.Key(modelid.RealmGlobal, m)] = struct{}{}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
