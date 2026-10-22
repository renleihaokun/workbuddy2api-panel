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

// SystemAsUser 把 system/developer 指令搬进 user 角色，首条保留一条中性 system。
//
// 语义：
//   - 删除 messages 里所有 role 为 system/developer 的消息，内容按序拼接；
//   - messages[0] 固定为 {"role":"system","content":neutral}（neutral 为空回落
//     Degraded）——满足上游「首条必须是 system」的渠道校验；
//   - 拼接内容并入**第一条 user 消息**：字符串内容 → 前缀 + "\n\n" + 原文；
//     数组内容 → 头部插一个 text part；没有 user 消息 → 在 system 之后插入一条；
//   - 其余消息（assistant/tool/未知角色）逐字不动。
//
// 守卫与 Rewrite 一致：空 body / 坏 JSON / 无 messages → 原样返回（绝不失败）。
// 幂等：对已伪装的 body 再调用，结果完全相同（此时已无 system 可抽，只剩中性 system）。
func SystemAsUser(body []byte, neutral string) []byte {
	if len(body) == 0 {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return body
	}
	if strings.TrimSpace(neutral) == "" {
		neutral = Degraded
	}
	kept := make([]any, 0, len(msgs)+1)
	var sysParts []string
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			kept = append(kept, raw)
			continue
		}
		role, _ := m["role"].(string)
		if role == "system" || role == "developer" {
			if t := contentTextOf(m["content"]); t != "" {
				sysParts = append(sysParts, t)
			}
			continue
		}
		kept = append(kept, raw)
	}
	if merged := strings.Join(sysParts, "\n\n"); merged != "" {
		// 已是伪装形态（唯一的 system 就是中性提示词）：原样返回，保证幂等。
		// 正常链路只调用一次（handler 用 systemConverted 守），这里防的是重复调用
		// 把中性提示词又搬进一遍 user 正文。
		if len(sysParts) == 1 && merged == neutral {
			return body
		}
		mergedInto := false
		for i, raw := range kept {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if role, _ := m["role"].(string); role != "user" {
				continue
			}
			m["content"] = mergeSystemIntoContent(m["content"], merged)
			kept[i] = m
			mergedInto = true
			break
		}
		if !mergedInto {
			// 没有 user 消息（纯 assistant/tool 历史）：单独插一条承载 system 原文。
			kept = append([]any{map[string]any{"role": "user", "content": merged}}, kept...)
		}
	}
	out := make([]any, 0, len(kept)+1)
	out = append(out, map[string]any{"role": "system", "content": neutral})
	out = append(out, kept...)
	obj["messages"] = out
	rewritten, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return rewritten
}

// mergeSystemIntoContent 把 system 原文并入一条 user 消息的 content：
//   - 字符串：system 原文 + "\n\n" + 原内容（原内容为空则只留 system 原文）；
//   - 数组：头部插一个 {"type":"text","text":system}，其余 part 原样保留；
//   - 其他（nil/未知）：直接用 system 原文替换。
func mergeSystemIntoContent(content any, system string) any {
	switch c := content.(type) {
	case string:
		if strings.TrimSpace(c) == "" {
			return system
		}
		return system + "\n\n" + c
	case []any:
		part := map[string]any{"type": "text", "text": system}
		merged := make([]any, 0, len(c)+1)
		merged = append(merged, part)
		merged = append(merged, c...)
		return merged
	default:
		return system
	}
}
