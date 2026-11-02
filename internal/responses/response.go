package responses

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

// idSeq 熵源故障时的兜底计数器（正常路径不使用）。
var idSeq uint64

// NewID 生成 Responses 风格的对象 ID（resp_/msg_/fc_/ctc_/call_ 前缀 + 24 hex）。
// 与 messages.go 的 randomHex 同源思路：crypto/rand 优先，熵源故障时回落
// 时间戳 + 计数器派生值——ID 唯一性是这里的硬要求（客户端按 id 关联 item）。
func NewID(prefix string) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err == nil {
		return joinID(prefix, hex.EncodeToString(b))
	}
	return joinID(prefix, fmt.Sprintf("%016x%08x", time.Now().UnixNano(), atomic.AddUint64(&idSeq, 1)))
}

func joinID(prefix, body string) string {
	if prefix == "" {
		return body
	}
	return prefix + "_" + body
}

// messageItem 构造一个 assistant message output item。
func messageItem(id, text, status string) map[string]any {
	content := map[string]any{
		"type":        "output_text",
		"text":        text,
		"annotations": []any{},
		"logprobs":    []any{},
	}
	return map[string]any{
		"id":      id,
		"type":    "message",
		"status":  status,
		"role":    "assistant",
		"content": []any{content},
	}
}

// functionCallItem 构造一个 function_call output item。
func functionCallItem(id, callID, name, arguments, status string) map[string]any {
	item := map[string]any{
		"id":        id,
		"type":      "function_call",
		"status":    status,
		"call_id":   callID,
		"name":      name,
		"arguments": arguments,
	}
	return item
}

// customToolCallItem 构造一个 custom_tool_call output item（freeform 工具）。
//
// Codex 的 apply_patch 以 type=custom 声明，只接受 custom_tool_call + input
// 字符串；返回 function_call 会被判为「不兼容 payload」。
func customToolCallItem(id, callID, name, input, status string) map[string]any {
	return map[string]any{
		"id":      id,
		"type":    "custom_tool_call",
		"status":  status,
		"call_id": callID,
		"name":    name,
		"input":   input,
	}
}

// customInputFromArguments 从 chat 工具调用的 arguments 里取 freeform 文本。
// 规范形态是 {"input":"..."}；模型偶尔直接吐裸文本（非法 JSON），此时原样返回，
// 至少不让整轮工具调用丢失。
func customInputFromArguments(arguments string) string {
	if strings.TrimSpace(arguments) == "" {
		return ""
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(arguments), &payload); err == nil {
		if input, ok := payload["input"].(string); ok {
			return input
		}
		// 模型偶尔用别的键名（patch/content/text）承载 freeform 文本：单字段对象
		// 且值为字符串时无歧义，直接取用，避免把整段 JSON 当补丁发给客户端。
		if len(payload) == 1 {
			for _, v := range payload {
				if s, ok := v.(string); ok {
					return s
				}
			}
		}
	}
	return arguments
}

// usageFromChat 把 chat 的 usage 映射成 Responses 的 usage。
//
// 上游缺失 usage 时返回全零对象而不是 null：部分严格客户端会对 completed 事件的
// usage 做非空解引用，null 直接 panic；零值至少结构合法。
func usageFromChat(v any) map[string]any {
	usage, _ := v.(map[string]any)
	prompt := intValue(usage["prompt_tokens"])
	completion := intValue(usage["completion_tokens"])
	total := intValue(usage["total_tokens"])
	if total == 0 && (prompt != 0 || completion != 0) {
		total = prompt + completion
	}
	cached := intValue(usage["cached_tokens"])
	if d, ok := usage["prompt_tokens_details"].(map[string]any); ok && d != nil {
		if n := intValue(d["cached_tokens"]); n > 0 {
			cached = n
		}
	}
	reasoning := intValue(usage["reasoning_tokens"])
	if d, ok := usage["completion_tokens_details"].(map[string]any); ok && d != nil {
		if n := intValue(d["reasoning_tokens"]); n > 0 {
			reasoning = n
		}
	}
	return map[string]any{
		"input_tokens":          prompt,
		"input_tokens_details":  map[string]any{"cached_tokens": cached},
		"output_tokens":         completion,
		"output_tokens_details": map[string]any{"reasoning_tokens": reasoning},
		"total_tokens":          total,
	}
}

// intValue 取整数值；JSON 数字、int 系列都兼容。缺失/非数值返回 0。
func intValue(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case float32:
		return int64(n)
	case int:
		return int64(n)
	case int64:
		return n
	case int32:
		return int64(n)
	case json.Number:
		i, err := n.Int64()
		if err == nil {
			return i
		}
	}
	return 0
}

// FromChatCompletion 把本地聚合出的 chat.completion 响应翻译成 Responses 的
// Response 对象（非流式）。
func FromChatCompletion(chat map[string]any, meta Meta) map[string]any {
	output := make([]any, 0, 2)
	status := "completed"
	var incomplete any

	choices, _ := chat["choices"].([]any)
	if len(choices) > 0 {
		choice, _ := choices[0].(map[string]any)
		message, _ := choice["message"].(map[string]any)

		switch finish, _ := choice["finish_reason"].(string); finish {
		case "length":
			status = "incomplete"
			incomplete = map[string]any{"reason": "max_output_tokens"}
		case "content_filter":
			status = "incomplete"
			incomplete = map[string]any{"reason": "content_filter"}
		}
		itemStatus := "completed"
		if status == "incomplete" {
			itemStatus = "incomplete"
		}

		text := messageText(message["content"])
		toolCalls, _ := message["tool_calls"].([]any)
		if text != "" || len(toolCalls) == 0 {
			// 无文本也无工具调用时仍产出一个空 message item：output:[] 会被部分
			// 客户端判为畸形响应（Codex 直接报 "empty output"）。
			output = append(output, messageItem(NewID("msg"), text, itemStatus))
		}
		for _, raw := range toolCalls {
			call, _ := raw.(map[string]any)
			if call == nil {
				continue
			}
			fn, _ := call["function"].(map[string]any)
			name, _ := fn["name"].(string)
			args, _ := fn["arguments"].(string)
			callID, _ := call["id"].(string)
			if callID == "" {
				callID = NewID("call")
			}
			itemStatusForCall := itemStatus
			if args != "" && !json.Valid([]byte(args)) {
				// 畸形参数：item 与整体响应都标 incomplete，避免客户端执行坏参数。
				itemStatusForCall = "incomplete"
				status = "incomplete"
				incomplete = nil
			}
			if meta.CustomTools[name] {
				output = append(output, customToolCallItem(NewID("ctc"), callID, name, customInputFromArguments(args), itemStatusForCall))
			} else {
				output = append(output, functionCallItem(NewID("fc"), callID, name, args, itemStatusForCall))
			}
		}
	}

	if len(output) == 0 {
		// choices 缺失/为空时也补一个空 message item：output:[] 会被严格客户端
		// 判为畸形响应（与流式 Finish 的空输出兜底同口径）。
		output = append(output, messageItem(NewID("msg"), "", "completed"))
	}

	return buildResponse(NewID("resp"), meta, status, output, usageFromChat(chat["usage"]), incomplete)
}

// messageText 取 chat message.content 的文本。正常是 string；少数上游会回
// content part 数组，拼接其中的 text 字段。
func messageText(v any) string {
	switch c := v.(type) {
	case nil:
		return ""
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, raw := range c {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := part["text"].(string); ok {
				b.WriteString(text)
			}
		}
		return b.String()
	default:
		return fmt.Sprintf("%v", c)
	}
}

// buildResponse 组装 Response 对象。created / completed / failed / incomplete
// 各状态共用同一份骨架，保证字段集合稳定（客户端按同一结构反序列化）。
func buildResponse(id string, meta Meta, status string, output []any, usage any, incomplete any) map[string]any {
	resp := map[string]any{
		"id":                   id,
		"object":               "response",
		"created_at":           time.Now().Unix(),
		"status":               status,
		"background":           false,
		"error":                nil,
		"incomplete_details":   incomplete,
		"instructions":         meta.Instructions,
		"max_output_tokens":    nil,
		"max_tool_calls":       nil,
		"model":                meta.Model,
		"output":               output,
		"parallel_tool_calls":  true,
		"previous_response_id": nil,
		"reasoning":            meta.Reasoning,
		"service_tier":         "default",
		"store":                false,
		"temperature":          nil,
		"text":                 map[string]any{"format": map[string]any{"type": "text"}},
		"tool_choice":          "auto",
		"tools":                []any{},
		"top_p":                nil,
		"truncation":           "disabled",
		"usage":                usage,
		"metadata":             map[string]any{},
	}
	if meta.MaxOutputTokens != nil {
		resp["max_output_tokens"] = *meta.MaxOutputTokens
	}
	if meta.ParallelToolCalls != nil {
		resp["parallel_tool_calls"] = *meta.ParallelToolCalls
	}
	if meta.Temperature != nil {
		resp["temperature"] = *meta.Temperature
	}
	if meta.TopP != nil {
		resp["top_p"] = *meta.TopP
	}
	if meta.Text != nil {
		// 合并而不是整体替换：客户端可能只传 verbosity，缺 format 会让严格
		// SDK 的反序列化失败。format 默认补 text。
		text := map[string]any{"format": map[string]any{"type": "text"}}
		if incoming, ok := meta.Text.(map[string]any); ok {
			for k, v := range incoming {
				text[k] = v
			}
			if _, hasFormat := text["format"]; !hasFormat {
				text["format"] = map[string]any{"type": "text"}
			}
		}
		resp["text"] = text
	}
	if meta.ToolChoice != nil {
		resp["tool_choice"] = meta.ToolChoice
	}
	if len(meta.Tools) > 0 {
		resp["tools"] = meta.Tools
	}
	if len(meta.Metadata) > 0 {
		resp["metadata"] = meta.Metadata
	}
	if meta.PromptCacheKey != "" {
		resp["prompt_cache_key"] = meta.PromptCacheKey
	}
	if meta.Truncation != "" {
		resp["truncation"] = meta.Truncation
	}
	if meta.ServiceTier != "" {
		resp["service_tier"] = meta.ServiceTier
	}
	return resp
}

// failedResponse 生成 status=failed 的 Response 对象，附带 error 详情。
func failedResponse(id string, meta Meta, output []any, code, message string) map[string]any {
	resp := buildResponse(id, meta, "failed", output, nil, nil)
	resp["error"] = map[string]any{
		"code":    code,
		"message": message,
	}
	return resp
}
