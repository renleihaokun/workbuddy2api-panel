// channel.go 一次 chat 请求内的「通道适配」状态机。
//
// 背景：上游把非官方客户端的请求判成 11-128、把可疑 system 内容判成内容拦截。
// 网关的应对有三层：
//  1. 出站前按提示词模式改写（custom / append / passthrough）；
//  2. 撞 11-128 时把 system 指令搬进 user 角色，同账号同请求内重发一次；
//  3. 内容拦截误报时触发「提示词降级」，换 Degraded 中性提示词重试。
//
// 这三件事此前以四个隐式变量（degradedApplied / sysKey / systemConverted /
// channelRefusedConverted）散在 chatCompletions 的轮转循环里，新增一条重试
// 路径就要同时改四五处；漏一处就是「状态对了但归档没记」（v1.22.0 的
// st.degraded 漏记就是这么来的）。这里把它们收进一个显式状态机：
// 判定/改写/埋点在 channelAdapt，重发与记账仍留在 handler（需要轮转上下文）。
package server

import (
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/channelstat"
	"github.com/linguo2625469/workbuddy2api-panel/internal/prompt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// channelAdapt 单次请求的通道适配状态。
type channelAdapt struct {
	h  *Handler
	st *chatStat

	// sysKey 当前出站 body 的 system 学习键：搬移前计算并保持（学习键 = 原始
	// system 原文），降级重写后重算（出站形态变了，学习表必须跟 body 对齐）。
	sysKey string
	// degraded 已换 Degraded 中性提示词（粘性降级期直达，或本次降级重试）。
	degraded bool
	// sysConverted 已把 system 搬进 user（层 3 直达或层 2 重试）。
	sysConverted bool
}

// newChannelAdapt 构造状态机；st 用于同步 reqlog 的 degraded / sys_as_user 维度。
func newChannelAdapt(h *Handler, st *chatStat) *channelAdapt {
	return &channelAdapt{h: h, st: st}
}

// RewritePrompt 按提示词模式改写出站 body（每个请求一次、轮转前）。
//   - custom：用自有提示词替换客户端 system/developer（从源头消灭指纹误报）；
//   - append：开头连续 system/developer 块后插自有提示词，既有消息逐字不动；
//   - passthrough / append + 降级期：换 Degraded 中性提示词直达，不再先撞 400。
//
// append 在降级期退化为 replace——带指纹原文重试是确定性再撞墙，replace 是
// 一次性最小抢救（issue #129 设计 §4）。
func (c *channelAdapt) RewritePrompt(body []byte) []byte {
	mode, text := c.h.cfg.PromptMode, c.h.cfg.PromptText
	switch {
	case mode == "custom" && text != "":
		return prompt.Rewrite(body, text)
	case mode == "append" && text != "" && !c.h.degrade.Active():
		return prompt.Append(body, text)
	case (mode == "passthrough" || mode == "append") && c.h.degrade.Active():
		c.degraded = true
		c.st.degraded = true
		return prompt.Rewrite(body, prompt.Degraded)
	}
	return body
}

// ApplyLearned 层 3：该段 system 已被证实需要伪装 → 直接以搬移形态出站，
// 不再先撞一次 400。sysKey 在搬移前计算并保留（学习键是原始 system 原文）。
func (c *channelAdapt) ApplyLearned(body []byte) []byte {
	c.sysKey = systemModeKey(prompt.SystemText(body))
	if !c.h.systemAsUserEnabled() || c.sysKey == "" || !c.h.sysMode.Known(c.sysKey) {
		return body
	}
	moved := prompt.SystemAsUser(body, prompt.Degraded)
	if moved == nil {
		// 幂等保护：已无可搬内容（例如降级期 system 本来就是 Degraded）。
		return body
	}
	c.sysConverted = true
	c.st.sysAsUser = true
	return moved
}

// TrySystemAsUser 层 2：撞 11-128 且是「身份句被拒」形态时，同账号同请求内
// 搬进 user 角色重发一次。返回 (新 body, 是否应重试)。
//
// 只看 11-128：结构性拒绝（首条不是 system、缺会话 id）搬了也没用。
// 不罚号、不退避、不占换号名额（inline 重发，MaxRotate=1 也能重试）。
func (c *channelAdapt) TrySystemAsUser(body []byte, status int, respBody []byte, uerr *upstream.Error) ([]byte, bool) {
	if status < 400 || c.sysConverted || !c.h.systemAsUserEnabled() {
		return nil, false
	}
	if attemptKind(status, respBody, uerr) != upstream.ErrChannelRefused {
		return nil, false
	}
	if !upstream.IsSystemMovedRefusal(string(respBody)) {
		return nil, false
	}
	moved := prompt.SystemAsUser(body, prompt.Degraded)
	if moved == nil {
		return nil, false
	}
	c.sysConverted = true
	c.st.sysAsUser = true
	// 通道事件：上游把请求判成「非官方客户端」→ 我们用 system→user 搬移救回。
	// 这是反代通道劣化的核心信号，与本次请求最终成败无关（救回也是成功）。
	c.h.noteEvent(channelstat.KindChannelRefused)
	c.h.noteEvent(channelstat.KindSysAsUser)
	return moved, true
}

// TryDegraded 内容拦截误报：passthrough/append 首遇 → 触发降级到次日 00:00 CST，
// 换 Degraded 中性提示词同请求内重试。返回 (新 body, 是否应重试)。
//
// 只触发一次：c.degraded 置位后再撞内容拦截走终止分支（层 4 语义）。
func (c *channelAdapt) TryDegraded(body []byte, kind upstream.ErrKind) ([]byte, bool) {
	if kind != upstream.ErrContentBlocked || c.degraded {
		return nil, false
	}
	if mode := c.h.cfg.PromptMode; mode != "passthrough" && mode != "append" {
		return nil, false
	}
	// 已在降级期内（并发请求先到）不重复记事件：degrade_trigger 按「降级窗口」
	// 计一次，而不是按撞墙的请求数计。
	if started := c.h.degrade.Trigger(); started {
		// 降级触发时刻落盘：进程内状态重启清零，看板需要「上次触发」。
		if c.h.cfg.ChannelStats != nil {
			c.h.cfg.ChannelStats.MarkDegrade(time.Now())
		}
		c.h.noteEvent(channelstat.KindDegradeTrigger)
	}
	out := prompt.Rewrite(body, prompt.Degraded)
	c.degraded = true
	c.st.degraded = true // 轮转循环内触发的降级也要进 reqlog 归档（v1.22.0 漏记）
	// system 已整段换成 Degraded → 学习键必须重算：否则 11-128 分支会拿旧
	// system 的键去判断/记录新形态，学习表与实际出站 body 脱节。
	c.sysKey = systemModeKey(prompt.SystemText(out))
	return out, true
}

// RememberOnPass 层 3 学习：伪装后这一跳没再被通道拒绝 → 记住这段 system。
//
// 反向不学：伪装后仍是 11-128 说明指纹在 assistant/tool 历史里搬不走，
// 记下来只会让后续请求白走伪装。成功路径传 upstream.ErrNone。
func (c *channelAdapt) RememberOnPass(kind upstream.ErrKind) {
	if c.sysConverted && c.sysKey != "" && kind != upstream.ErrChannelRefused {
		c.h.rememberSystem(c.sysKey)
	}
}

// SysConverted 本请求是否走过 system→user 搬移（末端 hint 用）。
func (c *channelAdapt) SysConverted() bool { return c.sysConverted }
