package server

import (
	"sync"
	"time"
)

// degradeGate 降级状态机：passthrough 模式下请求被上游内容策略拦截时，
// 切换到 Degraded 中性提示词直到次日 00:00 CST 重置。
//
// 定位：误报处理（与 internal/upstream/sanitize.go 同哲学）——内容拦截多为
// system 来源的指纹误报，换最小中性提示词即可绕开；不是对抗框架。
//
// 状态机：进程内存、重启清零（可接受：重启极少触发，且 custom 模式根本
// 不进降级路径）。降级期内的请求直达 Degraded，不再先撞 400。
type degradeGate struct {
	mu    sync.Mutex
	until time.Time
}

// Active 当前是否处于降级期（now < until）。
func (g *degradeGate) Active() bool {
	active, _ := g.Status()
	return active
}

// Status 返回降级状态快照（面板「反代劣化」看板用）：是否在降级期 + 重置时刻。
// 与 Active 同一把锁、同一判定，避免看板与请求路径出现两套口径。
func (g *degradeGate) Status() (active bool, until time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return time.Now().Before(g.until), g.until
}

// Trigger 触发降级，直到次日 00:00 CST（Asia/Shanghai）。
// 返回是否**新开窗口**：已在降级期内不续期、返回 false（保持最早触发点的
// 00:00 重置语义）。调用方据此决定要不要记 degrade_trigger 事件——事件按
// 「降级窗口」计一次，而不是按撞墙的请求数计。
func (g *degradeGate) Trigger() (started bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	if now.Before(g.until) {
		return false
	}
	g.until = nextMidnightCST(now)
	return true
}

// nextMidnightCST 返回 now 之后最近的 Asia/Shanghai 00:00 时刻（纯函数，方便单测）。
//
// 边界语义：
//   - 23:59 → 次日 00:00（几秒后）
//   - 00:00 → 次日 00:00（刚过 00:00，下个零点是次日）
//
// 用固定 +08:00 偏移计算，避免依赖系统时区配置（容器/宿主机时区不确定）。
func nextMidnightCST(now time.Time) time.Time {
	cst := time.FixedZone("CST", 8*60*60)
	// 把 now 转到 CST 视角取当天 00:00，再加一天；若该 00:00 <= now 则再加一天。
	y, m, d := now.In(cst).Date()
	midnight := time.Date(y, m, d, 0, 0, 0, 0, cst)
	for !midnight.After(now) {
		midnight = midnight.Add(24 * time.Hour)
	}
	return midnight
}

// ChannelState 反代通道的当前状态快照（只读，面板看板用）。
//
// 为什么要把「进程内」和「跨重启」两类字段分开命名：degradeGate / systemModeGate
// 都是进程内存状态，重启即清零；如果只看 LearnedNow / Active，重启后的看板会
// 显示「历史上什么都没发生」——那是错误陈述。LearnedTotal / DegradeLastAt 由
// channelstat 账本持久化，回答的是「历史上发生过多少次」。
type ChannelState struct {
	// Active 当前是否处于提示词降级期（进程内）。
	Active bool
	// Until 降级期截止（进程内；未触发时为零值）。
	Until time.Time
	// LearnedNow 当前生效的「必须伪装成 user」指纹条数（进程内表，24h TTL）。
	LearnedNow int
	// LearnedTotal 累计学习次数（跨重启，来自 channelstat）。
	LearnedTotal int64
	// DegradeLastAt 提示词降级最近一次触发时刻（跨重启；从未触发为零值）。
	DegradeLastAt time.Time
	// Mode 当前系统提示词模式（passthrough / custom / append）。
	Mode string
}

// ChannelState 返回当前反代通道状态快照。
//
// 为什么由 handler 暴露而不是放进 panel：degradeGate / systemModeGate 都是
// handler 的进程内私有状态，面板只能经这条只读出口取值（main 事后接线）。
func (h *Handler) ChannelState() ChannelState {
	active, until := h.degrade.Status()
	st := ChannelState{
		Active:     active,
		Until:      until,
		LearnedNow: h.sysMode.Len(),
		Mode:       h.cfg.PromptMode,
	}
	if h.cfg.ChannelStats != nil {
		_, last, total := h.cfg.ChannelStats.Snapshot()
		st.DegradeLastAt = last
		st.LearnedTotal = total
	}
	return st
}
