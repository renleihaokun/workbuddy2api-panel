package responses

import "testing"

// chatTextResponse 构造一个纯文本的 chat.completion 聚合结果。
func chatTextResponse() map[string]any {
	return map[string]any{
		"id":     "chatcmpl-1",
		"object": "chat.completion",
		"model":  "deepseek-v4",
		"choices": []any{map[string]any{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": "你好，世界",
			},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     float64(12),
			"completion_tokens": float64(4),
			"total_tokens":      float64(16),
			"prompt_tokens_details": map[string]any{
				"cached_tokens": float64(8),
			},
		},
	}
}

func outputOf(t *testing.T, resp map[string]any) []any {
	t.Helper()
	out, ok := resp["output"].([]any)
	if !ok {
		t.Fatalf("output missing: %v", resp["output"])
	}
	return out
}

// TestFromChatCompletionText 纯文本响应 → 单个 message item + usage 映射。
func TestFromChatCompletionText(t *testing.T) {
	resp := FromChatCompletion(chatTextResponse(), Meta{Model: "deepseek-v4"})

	if resp["object"] != "response" {
		t.Errorf("object=%v", resp["object"])
	}
	if resp["status"] != "completed" {
		t.Errorf("status=%v", resp["status"])
	}
	if resp["store"] != false {
		t.Errorf("store must always be false")
	}
	out := outputOf(t, resp)
	if len(out) != 1 {
		t.Fatalf("output len=%d want 1", len(out))
	}
	item, _ := out[0].(map[string]any)
	if item["type"] != "message" || item["role"] != "assistant" {
		t.Errorf("item=%v", item)
	}
	content, _ := item["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content len=%d", len(content))
	}
	part, _ := content[0].(map[string]any)
	if part["type"] != "output_text" || part["text"] != "你好，世界" {
		t.Errorf("part=%v", part)
	}

	usage, _ := resp["usage"].(map[string]any)
	if v, _ := usage["input_tokens"].(int64); v != 12 {
		t.Errorf("input_tokens=%v", usage["input_tokens"])
	}
	if v, _ := usage["output_tokens"].(int64); v != 4 {
		t.Errorf("output_tokens=%v", usage["output_tokens"])
	}
	details, _ := usage["input_tokens_details"].(map[string]any)
	if v, _ := details["cached_tokens"].(int64); v != 8 {
		t.Errorf("cached_tokens=%v", details["cached_tokens"])
	}
}

// TestFromChatCompletionToolCalls 工具调用：普通工具 → function_call，
// custom 工具 → custom_tool_call（apply_patch 必须走后者）。
func TestFromChatCompletionToolCalls(t *testing.T) {
	chat := map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]any{
				"role":    "assistant",
				"content": nil,
				"tool_calls": []any{
					map[string]any{
						"id": "call_1", "type": "function",
						"function": map[string]any{"name": "shell", "arguments": `{"command":["ls"]}`},
					},
					map[string]any{
						"id": "call_2", "type": "function",
						"function": map[string]any{"name": "apply_patch", "arguments": `{"input":"*** Begin Patch\n*** End Patch"}`},
					},
				},
			},
			"finish_reason": "tool_calls",
		}},
	}
	meta := Meta{Model: "m", CustomTools: map[string]bool{"apply_patch": true}}
	resp := FromChatCompletion(chat, meta)
	out := outputOf(t, resp)
	if len(out) != 2 {
		t.Fatalf("output len=%d want 2 (纯工具响应不应额外产生空 message)", len(out))
	}
	first, _ := out[0].(map[string]any)
	if first["type"] != "function_call" || first["call_id"] != "call_1" {
		t.Errorf("first=%v", first)
	}
	second, _ := out[1].(map[string]any)
	if second["type"] != "custom_tool_call" {
		t.Fatalf("apply_patch should be custom_tool_call, got %v", second["type"])
	}
	if input, _ := second["input"].(string); input != "*** Begin Patch\n*** End Patch" {
		t.Errorf("custom input=%q", input)
	}
	if _, leaked := second["arguments"]; leaked {
		t.Errorf("custom_tool_call must not carry arguments")
	}
}

// TestFromChatCompletionLength 截断响应 → incomplete/max_output_tokens。
func TestFromChatCompletionLength(t *testing.T) {
	chat := chatTextResponse()
	choices := chat["choices"].([]any)
	choice := choices[0].(map[string]any)
	choice["finish_reason"] = "length"
	resp := FromChatCompletion(chat, Meta{Model: "m"})
	if resp["status"] != "incomplete" {
		t.Errorf("status=%v", resp["status"])
	}
	details, _ := resp["incomplete_details"].(map[string]any)
	if details["reason"] != "max_output_tokens" {
		t.Errorf("incomplete_details=%v", resp["incomplete_details"])
	}
	out := outputOf(t, resp)
	item, _ := out[0].(map[string]any)
	if item["status"] != "incomplete" {
		t.Errorf("truncated message item status=%v want incomplete", item["status"])
	}
}

// TestFromChatCompletionEmpty 无文本无工具时仍产出空 message item。
func TestFromChatCompletionEmpty(t *testing.T) {
	chat := map[string]any{"choices": []any{map[string]any{
		"message":       map[string]any{"role": "assistant", "content": ""},
		"finish_reason": "stop",
	}}}
	resp := FromChatCompletion(chat, Meta{Model: "m"})
	out := outputOf(t, resp)
	if len(out) != 1 {
		t.Fatalf("output len=%d want 1", len(out))
	}
}

// TestCustomInputFromArguments freeform 文本的多种模型输出形态。
func TestCustomInputFromArguments(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"input":"*** Begin Patch"}`, "*** Begin Patch"},
		{`{"patch":"*** Begin Patch"}`, "*** Begin Patch"},
		{`*** Begin Patch`, "*** Begin Patch"},
		{`{"a":"x","b":"y"}`, `{"a":"x","b":"y"}`},
		{``, ``},
	}
	for _, c := range cases {
		if got := customInputFromArguments(c.in); got != c.want {
			t.Errorf("customInputFromArguments(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

// TestFromChatCompletionContentParts message.content 为 part 数组时拼接文本。
func TestFromChatCompletionContentParts(t *testing.T) {
	chat := map[string]any{"choices": []any{map[string]any{
		"message": map[string]any{
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "text", "text": "foo"},
				map[string]any{"type": "text", "text": "bar"},
			},
		},
		"finish_reason": "stop",
	}}}
	resp := FromChatCompletion(chat, Meta{Model: "m"})
	out := outputOf(t, resp)
	item, _ := out[0].(map[string]any)
	content, _ := item["content"].([]any)
	part, _ := content[0].(map[string]any)
	if part["text"] != "foobar" {
		t.Errorf("text=%q", part["text"])
	}
}

// TestUsageFromChatMissing 上游没给 usage 时返回全零结构，而不是 null。
func TestUsageFromChatMissing(t *testing.T) {
	u := usageFromChat(nil)
	if u["input_tokens"] != int64(0) || u["output_tokens"] != int64(0) || u["total_tokens"] != int64(0) {
		t.Fatalf("zero usage expected: %v", u)
	}
	if _, ok := u["input_tokens_details"].(map[string]any); !ok {
		t.Fatalf("details missing: %v", u)
	}
}

// TestFromChatCompletionMalformedArgs 非流式畸形工具参数也要标 incomplete。
func TestFromChatCompletionMalformedArgs(t *testing.T) {
	chat := map[string]any{"choices": []any{map[string]any{
		"message": map[string]any{
			"role": "assistant",
			"tool_calls": []any{map[string]any{
				"id": "call_1", "function": map[string]any{"name": "shell", "arguments": `{"command":[`},
			}},
		},
		"finish_reason": "stop",
	}}}
	resp := FromChatCompletion(chat, Meta{Model: "m"})
	if resp["status"] != "incomplete" {
		t.Fatalf("status=%v want incomplete", resp["status"])
	}
	out := outputOf(t, resp)
	item, _ := out[0].(map[string]any)
	if item["status"] != "incomplete" {
		t.Errorf("item status=%v", item["status"])
	}
}

// TestBuildResponseTextMerge text 只带 verbosity 时也要补默认 format。
func TestBuildResponseTextMerge(t *testing.T) {
	resp := buildResponse("resp_x", Meta{Model: "m", Text: map[string]any{"verbosity": "low"}}, "completed", []any{}, nil, nil)
	text, _ := resp["text"].(map[string]any)
	if text["verbosity"] != "low" {
		t.Fatalf("verbosity lost: %v", text)
	}
	format, _ := text["format"].(map[string]any)
	if format["type"] != "text" {
		t.Fatalf("format missing: %v", text)
	}
}

// TestBuildResponseEcho 响应对象必须回显请求侧的 tools/instructions 等字段。
func TestBuildResponseEcho(t *testing.T) {
	temp := 0.3
	maxOut := int64(2048)
	meta := Meta{
		Model:           "m",
		Instructions:    "be nice",
		Tools:           []any{map[string]any{"type": "function"}},
		ToolChoice:      map[string]any{"type": "function", "name": "shell"},
		Temperature:     &temp,
		MaxOutputTokens: &maxOut,
		PromptCacheKey:  "sess",
		Metadata:        map[string]any{"k": "v"},
	}
	resp := buildResponse("resp_x", meta, "completed", []any{}, nil, nil)
	if resp["instructions"] != "be nice" {
		t.Errorf("instructions=%v", resp["instructions"])
	}
	tc, _ := resp["tool_choice"].(map[string]any)
	if tc == nil || tc["name"] != "shell" {
		t.Errorf("tool_choice not echoed: %v", resp["tool_choice"])
	}
	if v, _ := resp["temperature"].(float64); v != 0.3 {
		t.Errorf("temperature=%v", resp["temperature"])
	}
	if v, _ := resp["max_output_tokens"].(int64); v != 2048 {
		t.Errorf("max_output_tokens=%v", resp["max_output_tokens"])
	}
	if resp["prompt_cache_key"] != "sess" {
		t.Errorf("prompt_cache_key=%v", resp["prompt_cache_key"])
	}
	if tools, _ := resp["tools"].([]any); len(tools) != 1 {
		t.Errorf("tools=%v", resp["tools"])
	}
}
