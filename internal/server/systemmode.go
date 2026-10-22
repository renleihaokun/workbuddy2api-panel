// systemmode.go system 提示词「角色伪装」的进程内学习表与配套判定。
//
// 定位：上游把某些 system 内容判成「非官方渠道」返回 11-128 时，把 system 指令
// 搬进 user 角色重发一次通常就能过（见 internal/prompt/system.go）。但每段新
// system 都先撞一次 400 是纯浪费：同一段原文一旦被证实「必须伪装」，后续请求就
// 直接以伪装形态出站。
//
// 与 degradeGate 同哲学：进程内存态、重启清零；不同点是按 **system 原文** 分组，
// 而不是全局单状态——不同客户端的 system 指纹互不相干，全局降级会把没问题的
// 请求也一起改写。
package server

import (
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

const (
	// systemModeTTL 学习条目的有效期：上游的校验规则会变，过期后重新「先原样、
	// 撞了再伪装」，让提示词恢复 system 角色的原生权重。
	systemModeTTL = 24 * time.Hour
	// systemModeMax 学习表上限：进程内最多记这么多段 system 原文的键；
	// 超出按最旧淘汰。正常部署（少量客户端模板）远达不到。
	systemModeMax = 512
)

// systemModeGate 记录「哪些 system 原文已被证实需要伪装成 user 角色」。
// 零值可用；所有方法并发安全。
type systemModeGate struct {
	mu sync.Mutex
	m  map[string]time.Time
}

// Known 报告该键是否仍在学习有效期内（过期即删）；空键恒 false。
func (g *systemModeGate) Known(key string) bool {
	if key == "" {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	at, ok := g.m[key]
	if !ok {
		return false
	}
	if time.Since(at) > systemModeTTL {
		delete(g.m, key)
		return false
	}
	return true
}

// Remember 记下该键（已在表内则刷新时间）。超出上限按最旧条目淘汰。
func (g *systemModeGate) Remember(key string) {
	if key == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.m == nil {
		g.m = make(map[string]time.Time, 8)
	}
	g.m[key] = time.Now()
	for len(g.m) > systemModeMax {
		oldestKey, oldest := "", time.Time{}
		for k, at := range g.m {
			if oldestKey == "" || at.Before(oldest) {
				oldestKey, oldest = k, at
			}
		}
		if oldestKey == "" {
			break
		}
		delete(g.m, oldestKey)
	}
}

// systemModeKey 把一段 system 原文派生为稳定短键；空文本返回 ""（不学习）。
//
// 复用 session.RequestIDForKey（sha256 + 进程盐，恒 32 hex）：纯派生、无缓存、
// 不把提示词原文留在内存里（只存哈希）。进程盐每次启动变化，所以学习表天然
// 不跨重启——与 degradeGate 的进程内语义一致。
func systemModeKey(systemText string) string {
	if systemText == "" {
		return ""
	}
	return session.RequestIDForKey("sys|" + systemText)
}

// systemModeKeyShort 日志用短键（前 8 位；空键返回 "-"），避免把完整哈希写进日志。
func systemModeKeyShort(key string) string {
	if len(key) < 8 {
		return "-"
	}
	return key[:8]
}

// systemAsUserEnabled 报告 system→user 伪装重试是否启用。经 livecfg 热生效；
// Live 为 nil 的裸用/测试路径由 loadLive 回落到静态字段。
func (h *Handler) systemAsUserEnabled() bool {
	return h.loadLive().SystemAsUserFallback
}

// attemptKind 从一次 ChatStreamContext 的结果里取出错误分类（不构造信封）：
// uerr 非空直接取其 Kind；否则按 status>=400 的 body 走 Classify（与
// status>=400 块的防御逻辑同口径）；其余为 ErrNone。
func attemptKind(status int, respBody []byte, uerr *upstream.Error) upstream.ErrKind {
	if uerr != nil {
		return uerr.Kind
	}
	if status >= 400 {
		return upstream.Classify(status, string(respBody))
	}
	return upstream.ErrNone
}
