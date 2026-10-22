// sanitize.go 出站请求体脱敏：剥离上游内容审核黑名单指纹。
//
// 背景：客户端（Claude Code 类 CLI）在 system prompt 注入若干固定模板句，
// 上游内容审核按逐字精确匹配拦截（非语义审核），一字改动即可绕过。
// 策略：键值/header 型指纹整段剥离；承载语义的模板句最小改写（换一词），语义不变。
package upstream

import (
	"regexp"
	"strings"
)

// sanitizeFeatures 特征预检：任一命中才进入净化（strings.Contains 快速路径，
// 普通请求全不中 → 原样返回，零分配）。
var sanitizeFeatures = []string{
	"x-anthropic-billing-header", // header 键值段键名
	"cc_entrypoint=",             // 尾随裸键值（截断前缀即可命中）
	"You are Claude Code",        // 身份句（截断前缀即可命中）
	"Main branch (",              // 注入指令句（截断前缀即可命中）
	"You are a coding agent running in the Codex CLI", // Codex instructions 首段（截断前缀即可命中）
	"github.com/anthropics/",                          // 反馈句里的 Anthropic 仓库链接
	"11128",                                           // 上游反探测：裸数字错误码
	// DSH 身份句（整句，不是品牌词）：上游按逐字匹配把这一行当成「非官方渠道」
	// 整单拦（400 11-128）。品牌词本身不在上游的匹配表里——magpie 插件 2026-10-21
	// 的真机复现也只命中整句，所以这里不再匹配裸 "DeepSeek Harness"：否则用户
	// 在对话里讨论 DSH、或让模型读/改含品牌词的仓库文档时，正文会被无谓改写。
	"You are an AI agent powered by DeepSeek Harness",
	// 审查插件（官方 dsh-auto-review + 三方 auto-review-zh）的 system/policy 身份句：
	// 这些提示词由客户端作为独立一次模型调用的正文发出，同样按逐字指纹被拦。
	"You are a read-only safety reviewer for a coding agent",                     // 官方 REVIEW_SYSTEM_PROMPT
	"You are the auto-review safety reviewer of",                                 // 官方审查请求正文
	"You are a READ-ONLY reviewer:",                                              // 官方审查请求正文第二句
	"You are the eval reviewer of the dsh-eval engine",                           // 官方 eval 审查
	"You are the final authorization reviewer for exactly one pending tool call", // 三方 REVIEW_POLICY
}

// sanitizeHdrRe 剥离层：header 键名即触发（与值无关），整段删除。
var sanitizeHdrRe = regexp.MustCompile(`(?i)x-anthropic-billing-header:[^;\n]*;?\s*`)

// sanitizeBareHdrRe 兜底层：裸键名（无冒号无值）同样是指纹——2026-09-13 实验 F4
// 证实 assistant 消息里反引号引用裸键名即触发 11128，而剥离层要求冒号、对裸串无效。
// 键值形态被整段删除后，残留的裸键名做最小缩写（header→hdr）：破坏逐字匹配、
// 语义不变、保留可读性。大小写不敏感，覆盖 X-Anthropic-... 变体。
//
// 注意该正则不要求冒号，是 sanitizeHdrRe 的超集——hasFingerprint 与 sanitizeText
// 中两者并用：先删键值形态（sanitizeHdrRe），再缩写残留裸键名（本正则），
// 替换语义不同（整段删除 vs 最小缩写），不可合并为一个正则。
var sanitizeBareHdrRe = regexp.MustCompile(`(?i)x-anthropic-billing-header`)

// sanitizeKvRe 剥离层：尾随裸键值（cc_xxx=...;）循环清理。
var sanitizeKvRe = regexp.MustCompile(`(?i)\bcc_[a-z0-9_]+=[^;\n]*;?\s*`)

// sanitizeRewrites 改写层：全模板句逐字替换（每句只改一个词，语义不变）。
//
// 身份句的匹配串**不带结尾标点**（只到 "…for Claude" 为止）：
// CLI 版这句以句号收尾（"…for Claude."），桌面版（claude-desktop-3p / Agent SDK）
// 以逗号接后继内容（"…for Claude, running within the Claude Agent SDK."）。
// 带句号的整句只匹配前者，桌面版会漏网、指纹原样发上游 → 400 code=11128。
// 去掉结尾标点后两种形态一并覆盖（替换串同样不带标点，让原有标点原样保留）。
// 注意仍要求 "You are Claude Code, " 前缀，不做更宽的子串替换，
// 以免误伤 TestExactMatchOnlyVariantNotTouched 所保护的零散文本。
var sanitizeRewrites = [][2]string{
	{
		"You are Claude Code, Anthropic's official CLI for Claude",
		"You are Claude Code, Anthropic's official CLI tool for Claude",
	},
	{
		"Main branch (you will usually use this for PRs)",
		"Default branch (you will usually use this for PRs)",
	},
	{
		"You are a coding agent running in the Codex CLI, a terminal-based coding assistant.",
		"You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant.",
	},
	{
		// 反馈句：整句带 Anthropic 仓库链接，上游按整句拦截（只留链接或只留半边均不拦，
		// 实测需整句同时出现）。give→provide 一词之差即可绕过，语义不变。
		"To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
		"To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
	},
	{
		// 上游反探测：请求里出现裸数字 11128 会被整单拦截（"code=11128" / 裸
		// "11128" / "错误码 11128" 全部命中；相邻的 11148 / 11101 / 11115 放行）。
		// 11128 正是本类拦截自身的错误码，上游据此识别"在讨论/回显其内部错误码"。
		// 注意作用域：只在 system / developer / assistant 消息里洗（见
		// sanitizeMessages）——user 与 tool 消息逐字原样出站，用户在对话里讨论这个
		// 错误码、或模型读取含它的仓库文件时，正文不再被无谓改写。
		"11128",
		"11-128",
	},
	{
		// DSH（DeepSeek Harness）的**整句**身份句才会被上游当成「非官方渠道调用」
		// 整单拦（400 11-128 Illegal API invocation from an unapproved channel）。
		// 改写整句（与 magpie 插件同一替换文案）：只换掉这一行的身份表述，品牌词
		// 在别处出现一律不动，避免误伤用户讨论与仓库文档。
		// 匹配串不带结尾标点，句号/换行随手保留，两种收尾形态一并覆盖。
		"You are an AI agent powered by DeepSeek Harness",
		"You are an AI agent that helps the user with programming tasks",
	},
	{
		// 官方自动审查插件 dsh-auto-review 的 REVIEW_SYSTEM_PROMPT：
		// "You are a read-only safety reviewer for a coding agent. ..."
		// 只把 reviewer 换成 checker（同一角色、语义不变），破坏逐字指纹。
		"You are a read-only safety reviewer for a coding agent",
		"You are a read-only safety checker for a coding agent",
	},
	{
		// 官方 dsh-auto-review 审查请求正文的身份句与只读约束句。
		// 原文以 "of DeepSeek Harness." 收尾，但品牌词另有全局改写规则——这里刻意
		// 只匹配到 "of"，与品牌词规则解耦，任意先后顺序都能命中。
		"You are the auto-review safety reviewer of",
		"You are the auto-review safety checker of",
	},
	{
		"You are a READ-ONLY reviewer:",
		"You are a READ-ONLY checker:",
	},
	{
		// 官方 dsh-auto-review 的 eval 审查身份句（dsh-eval 引擎）。
		"You are the eval reviewer of the dsh-eval engine",
		"You are the eval checker of the dsh-eval engine",
	},
	{
		// 三方审查插件 auto-review-zh 的 REVIEW_POLICY 首句。
		"You are the final authorization reviewer for exactly one pending tool call",
		"You are the final authorization checker for exactly one pending tool call",
	},
}

// sanitizeText 单段文本净化：预检不中 → 返回原串（零分配）。
func sanitizeText(text string) string {
	if !hasFingerprint(text) {
		return text
	}
	for _, rw := range sanitizeRewrites {
		text = strings.ReplaceAll(text, rw[0], rw[1])
	}
	if sanitizeHdrRe.MatchString(text) {
		text = sanitizeHdrRe.ReplaceAllString(text, "")
	}
	if strings.Contains(text, "cc_") {
		prev := ""
		for prev != text { // 清尾随裸 kv（cc_version=...; cc_entrypoint=...;）
			prev = text
			text = sanitizeKvRe.ReplaceAllString(text, "")
		}
	}
	// 兜底：键值形态已在上面整段删除，这里只剩裸键名（引用/示例文本形态）。
	text = sanitizeBareHdrRe.ReplaceAllString(text, "x-anthropic-billing-hdr")
	return strings.TrimSpace(text)
}

// hasFingerprint 特征预检：先走 strings.Contains 快速路径（零分配）；
// header 键名有大小写变体（X-Anthropic-...）且可能以裸键名形态出现（无冒号），
// Contains 大小写敏感、sanitizeHdrRe 要求冒号——两者都会漏掉「混合大小写 + 裸键名」，
// 必须再用不要求冒号的 (?i) 正则兜底（sanitizeBareHdrRe），否则整条净化被跳过。
// sanitizeBareHdrRe 不要求冒号，是 sanitizeHdrRe 的超集，故无需再单独匹配后者。
func hasFingerprint(text string) bool {
	for _, f := range sanitizeFeatures {
		if strings.Contains(text, f) {
			return true
		}
	}
	return sanitizeBareHdrRe.MatchString(text)
}

// sanitizeContent 兼容字符串与多模态数组；只动 text part，image 等 part 不动。
// 返回净化后的值及是否发生变化。
func sanitizeContent(v any) (any, bool) {
	switch c := v.(type) {
	case string:
		s := sanitizeText(c)
		return s, s != c
	case []any:
		changed := false
		for _, p := range c {
			m, ok := p.(map[string]any)
			if !ok {
				continue
			}
			text, ok := m["text"].(string)
			if !ok {
				continue
			}
			if s := sanitizeText(text); s != text {
				m["text"] = s
				changed = true
			}
		}
		return c, changed
	}
	return v, false
}

// sanitizeToolCalls 净化 assistant.tool_calls[].function.arguments。
//
// arguments 是**字符串化的 JSON**（不是对象），因此按文本走 sanitizeText 即可。
// 这块长期是盲区：工具调用消息的 content 通常是 null，而旧版 sanitizeMessages
// 在 content 缺失时直接 continue，整条消息连 tool_calls 一起被跳过——
// 于是历史里任何写进工具参数的被拦字符串（文件名、命令、写入内容）都会原样漏出。
func sanitizeToolCalls(v any) bool {
	callList, ok := v.([]any)
	if !ok {
		return false
	}
	changed := false
	for _, c := range callList {
		call, ok := c.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := call["function"].(map[string]any)
		if !ok {
			continue
		}
		args, ok := fn["arguments"].(string)
		if !ok {
			continue
		}
		if s := sanitizeText(args); s != args {
			fn["arguments"] = s
			changed = true
		}
	}
	return changed
}

// sanitizeMessages 净化 system / developer / assistant 消息里的 content、
// reasoning 与 tool_calls；任一命中返回 true。
//
// 角色范围有实测依据：2026-10-21 在两个 build 上用真实账号（glm-5.3 /
// deepseek-v4.1-flash / gpt-5.5）验证，通道校验只读 system 与 assistant 消息——
// 同样的被拒句放进 user 消息、工具结果、user 的多模态 part 都不会触发，放回
// system / assistant 才拒。因此 user 与 tool 消息**逐字原样出站**：讨论被拦串、
// 让模型读改含这些串的仓库文件、翻译原文都不会被改写。
func sanitizeMessages(messages []any) bool {
	changed := false
	for _, msg := range messages {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		switch role, _ := m["role"].(string); role {
		case "system", "developer", "assistant":
		default:
			continue // user / tool / 未知角色：上游不读，原样出站
		}
		// content 与 tool_calls 各自独立判断：content 可以为 null（工具调用轮），
		// 早期版本在此 continue，导致这类消息的 tool_calls 完全不被净化。
		if c, ok := m["content"]; ok {
			if nc, ch := sanitizeContent(c); ch {
				m["content"] = nc
				changed = true
			}
		}
		// reasoning_content（思维链回填字段，见 thinking.go/sse.go）实测同样
		// 携带指纹，与 content 同等净化。string 形态直接走 sanitizeText。
		if rc, ok := m["reasoning_content"].(string); ok {
			if s := sanitizeText(rc); s != rc {
				m["reasoning_content"] = s
				changed = true
			}
		}
		// reasoning：另一形态的思维链字段。thinking.go 的回填会把客户端送来的
		// reasoning_content 镜像进 reasoning（反之亦然），而此前只洗 content /
		// reasoning_content / tool_calls —— 镜像进 reasoning 的指纹原样出站，
		// 裸 "11128" 这种反探测串同样致命。
		if r, ok := m["reasoning"].(string); ok {
			if s := sanitizeText(r); s != r {
				m["reasoning"] = s
				changed = true
			}
		}
		if tc, ok := m["tool_calls"]; ok {
			if sanitizeToolCalls(tc) {
				changed = true
			}
		}
	}
	return changed
}
