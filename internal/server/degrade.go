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
	g.mu.Lock()
	defer g.mu.Unlock()
	return time.Now().Before(g.until)
}

// Status 返回降级状态快照（面板「反代劣化」看板用）：是否在降级期 + 重置时刻。
// 与 Active 同一把锁、同一判定，避免看板与请求路径出现两套口径。
func (g *degradeGate) Status() (active bool, until time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return time.Now().Before(g.until), g.until
}

// Trigger 触发降级，直到次日 00:00 CST（Asia/Shanghai）。
// 已在降级期内则不续期（保持最早触发点的 00:00 重置语义）。
func (g *degradeGate) Trigger() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !time.Now().Before(g.until) {
		// 已过期/未触发 → 设新的 until；未过期则保持原 until（不续期）。
		g.until = nextMidnightCST(time.Now())
	}
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

// DegradeState 当前「反代通道劣化」的进程内状态快照（只读，面板看板用）：
// 是否处于提示词降级期、降级期截止、已学习的 system 指纹条数、当前提示词模式。
//
// 为什么由 handler 暴露而不是放进 panel：degradeGate / systemModeGate 都是
// handler 的进程内私有状态，面板只能经这条只读出口取值（main 事后接线）。
func (h *Handler) DegradeState() (active bool, until time.Time, learned int, mode string) {
	active, until = h.degrade.Status()
	return active, until, h.sysMode.Len(), h.cfg.PromptMode
}
