// system.go system 指令的「角色伪装」改写（上游按 system 来源拦截时的兜底层）。
//
// 背景：上游对 system 来源的内容做逐字指纹校验，命中即 400 11-128（通道校验失败）。
// 指纹脱敏（internal/upstream/sanitize.go）解决「已知指纹」，本文件解决「未知指纹」：
// 把 system 指令搬进 user 角色再发一次——system 角色本身不再承载可疑正文。
//
// 关键约束：上游的 11-128 还有「首条消息不是 system」这一变体（实测首条 user →
// `first message is not system prompt`，首条 system → 200）。所以伪装**不能**把
// system 全部删掉，必须在 messages[0] 保留一条中性 system，否则会自己制造另一种
// 11-128。中性提示词复用 prompt.Degraded（同样刻意极简、无任何品牌/模板句）。
package prompt

import (
	"bytes"
	"encoding/json"
	"strings"
)

// SystemText 返回 messages 里所有 system/developer 消息的文本（按出现顺序用
// "\n\n" 连接）。多模态 content 只取 text part；无法解析的 body 返回 ""。
//
// 用途：派生「这段 system 是否需要伪装」的学习键。取全部 system/developer 而不是
// 只取首条——custom/append 模式下网关自己也会插一条 system，两者共同决定出站形态。
func SystemText(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return ""
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return ""
	}
	parts := make([]string, 0, 2)
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if role != "system" && role != "developer" {
			continue
		}
		if t := contentTextOf(m["content"]); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n\n")
}

// contentTextOf 取单条消息 content 的文本：字符串原样返回；数组拼接 text part；
// 其他形态（nil / 纯图片 / 未知）返回 ""。与 session.contentText 同口径，但保留
// 原始大小写与空白（学习键必须对「同一段原文」稳定，不做归一化）。
func contentTextOf(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, raw := range c {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			// OpenAI 规范要求 text part 带 type:"text"；部分客户端省略 type，按有
			// text 字段即视为文本（与 sanitizeContent 的宽松口径一致）。
			if t, _ := part["type"].(string); t != "" && t != "text" {
				continue
			}
			if s, ok := part["text"].(string); ok {
				b.WriteString(s)
			}
		}
		return b.String()
	}
	return ""
}

// InstructionsOpen / InstructionsClose 包住「搬进 user 的 system 原文」。
//
// 与 magpie 插件同一形态：模型能看清这段是「系统指令」而不是用户自己说的话，
// 既保住指令权重，又不让这段文字以 system 角色出站（上游只按 system/assistant
// 里的身份句判渠道）。
const (
	InstructionsOpen  = "<instructions>\n"
	InstructionsClose = "\n</instructions>"
)

// wrapInstructions 把 system 原文包成 <instructions>…</instructions>。
func wrapInstructions(system string) string {
	return InstructionsOpen + system + InstructionsClose
}

// SystemAsUser 把首条 system 的指令搬进 user 角色，system 只留中性提示词。
//
// 语义（对齐 magpie workbuddy 插件的 systemAsUser）：
//   - 首条 system/developer 的正文被搬进**第一条 user 消息**，用
//     <instructions>…</instructions> 包住；user 原有内容接在后面；
//   - messages[0] 固定为 {"role":"system","content":neutral}（neutral 为空回落
//     Degraded）——上游的「首条必须是 system」渠道校验不能丢；
//   - **后续** system/developer 消息就地改成 user（内容逐字保留），不再合并；
//   - 其余消息（assistant/tool/未知角色）逐字不动。
//
// 返回 nil 表示「没有可搬的东西」：无 messages、没有 system/developer、首条
// system 正文为空或本身就是中性提示词（已搬过）。调用方据此跳过重试——搬一次
// 不可能改变结果，白发一次上游请求。
//
// 守卫：空 body / 坏 JSON → nil（绝不失败）。
func SystemAsUser(body []byte, neutral string) []byte {
	if len(body) == 0 {
		return nil
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return nil
	}
	if strings.TrimSpace(neutral) == "" {
		neutral = Degraded
	}
	firstText := ""
	seenFirst := false
	kept := make([]any, 0, len(msgs)+1)
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			kept = append(kept, raw)
			continue
		}
		role, _ := m["role"].(string)
		if role != "system" && role != "developer" {
			kept = append(kept, raw)
			continue
		}
		if !seenFirst {
			// 首条 system：正文搬去 user，消息本身丢弃（中性 system 后面补在头部）。
			seenFirst = true
			firstText = contentTextOf(m["content"])
			continue
		}
		// 后续 system：就地降级为 user，内容逐字保留（magpie 同口径）。
		m["role"] = "user"
		kept = append(kept, m)
	}
	if !seenFirst {
		return nil
	}
	if strings.TrimSpace(firstText) == "" || strings.TrimSpace(firstText) == strings.TrimSpace(neutral) {
		return nil // 没有可搬的正文，或已经是搬过的形态（幂等）。
	}
	moved := false
	hasUser := false
	for _, raw := range kept {
		if m, ok := raw.(map[string]any); ok {
			if role, _ := m["role"].(string); role == "user" {
				hasUser = true
				break
			}
		}
	}
	out := make([]any, 0, len(kept)+2)
	out = append(out, map[string]any{"role": "system", "content": neutral})
	if !hasUser {
		// 没有 user 消息（纯 assistant/tool 历史）：指令块紧随 system 插入——
		// 「用户说的第一件事」就该是这段系统指令。
		out = append(out, map[string]any{"role": "user", "content": wrapInstructions(firstText)})
		moved = true
	}
	for _, raw := range kept {
		m, ok := raw.(map[string]any)
		if !ok {
			out = append(out, raw)
			continue
		}
		if role, _ := m["role"].(string); role == "user" && !moved {
			m["content"] = mergeInstructions(m["content"], firstText)
			moved = true
		}
		out = append(out, m)
	}
	obj["messages"] = out
	rewritten, err := marshalNoHTMLEscape(obj)
	if err != nil {
		return nil
	}
	return rewritten
}

// marshalNoHTMLEscape 序列化时不把 < > & 转成 \u003c 等：搬移后的
// <instructions> 标签要原样出现在出站 body 里（模型与排障日志都读它），
// 转义只影响可读性、不影响语义，但没必要让每次请求都带一串 \u003c。
func marshalNoHTMLEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// mergeInstructions 把包好的 system 原文并入一条 user 消息的 content：
//   - 字符串：<instructions>…</instructions> + "\n\n" + 原内容（原内容为空则只留指令块）；
//   - 数组：头部插一个 text part，其余 part（含图片）原样保留；
//   - 其他（nil/未知）：直接用指令块替换。
func mergeInstructions(content any, system string) any {
	block := wrapInstructions(system)
	switch c := content.(type) {
	case string:
		if strings.TrimSpace(c) == "" {
			return block
		}
		return block + "\n\n" + c
	case []any:
		merged := make([]any, 0, len(c)+1)
		merged = append(merged, map[string]any{"type": "text", "text": block})
		merged = append(merged, c...)
		return merged
	default:
		return block
	}
}
