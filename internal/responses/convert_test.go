package responses

import (
	"encoding/json"
	"strings"
	"testing"
)

// decodeChat 把 ConvertRequest 的输出解码成 map，供断言取字段。
func decodeChat(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("chat body is not valid JSON: %v (%s)", err, raw)
	}
	return out
}

func messagesOf(t *testing.T, chat map[string]any) []any {
	t.Helper()
	msgs, ok := chat["messages"].([]any)
	if !ok {
		t.Fatalf("messages missing: %v", chat["messages"])
	}
	return msgs
}

func msgAt(t *testing.T, msgs []any, i int) map[string]any {
	t.Helper()
	if i >= len(msgs) {
		t.Fatalf("messages has %d entries, want index %d", len(msgs), i)
	}
	m, _ := msgs[i].(map[string]any)
	if m == nil {
		t.Fatalf("messages[%d] is not an object", i)
	}
	return m
}

// TestConvertStringInput 字符串 input 应折叠成单条 user 消息。
func TestConvertStringInput(t *testing.T) {
	raw, meta, err := ConvertRequest([]byte(`{"model":"deepseek-v4","input":"hello"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if meta.Model != "deepseek-v4" {
		t.Errorf("meta.Model=%q", meta.Model)
	}
	chat := decodeChat(t, raw)
	msgs := messagesOf(t, chat)
	if len(msgs) != 1 {
		t.Fatalf("messages len=%d want 1", len(msgs))
	}
	if role, _ := msgAt(t, msgs, 0)["role"].(string); role != "user" {
		t.Errorf("role=%q", role)
	}
	if content, _ := msgAt(t, msgs, 0)["content"].(string); content != "hello" {
		t.Errorf("content=%q", content)
	}
	if stream, _ := chat["stream"].(bool); stream {
		t.Errorf("stream should default to false")
	}
}

// TestConvertFullItemHistory 覆盖 Codex 真实请求的核心形态：
// instructions + 多轮 message / function_call / function_call_output + tools。
func TestConvertFullItemHistory(t *testing.T) {
	body := `{
		"model":"deepseek-v4-pro",
		"instructions":"You are Codex, a coding agent.",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"list files"}]},
			{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{\"command\":[\"ls\"]}"},
			{"type":"function_call","call_id":"call_2","name":"shell","arguments":"{\"command\":[\"pwd\"]}"},
			{"type":"function_call_output","call_id":"call_1","output":"a.txt\n"},
			{"type":"function_call_output","call_id":"call_2","output":"/tmp\n"},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}
		],
		"tools":[{"type":"function","name":"shell","description":"run a command","parameters":{"type":"object","properties":{"command":{"type":"array","items":{"type":"string"}}}}}],
		"tool_choice":"auto",
		"parallel_tool_calls":false,
		"max_output_tokens":1024,
		"reasoning":{"effort":"minimal"},
		"store":false,
		"stream":true
	}`
	raw, meta, err := ConvertRequest([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	chat := decodeChat(t, raw)
	msgs := messagesOf(t, chat)

	// system(instructions) + user + assistant(2 tool_calls) + tool + tool + assistant
	if len(msgs) != 6 {
		t.Fatalf("messages len=%d want 6: %v", len(msgs), msgs)
	}
	if role, _ := msgAt(t, msgs, 0)["role"].(string); role != "system" {
		t.Errorf("messages[0].role=%q want system", role)
	}
	if content, _ := msgAt(t, msgs, 0)["content"].(string); !strings.Contains(content, "Codex") {
		t.Errorf("instructions not carried: %q", content)
	}
	assistant := msgAt(t, msgs, 2)
	tcs, _ := assistant["tool_calls"].([]any)
	if len(tcs) != 2 {
		t.Fatalf("assistant tool_calls=%d want 2 (并行调用必须合并到一条 assistant)", len(tcs))
	}
	if callID, _ := tcs[0].(map[string]any)["id"].(string); callID != "call_1" {
		t.Errorf("tool_call id=%q", callID)
	}
	if role, _ := msgAt(t, msgs, 3)["role"].(string); role != "tool" {
		t.Errorf("messages[3].role=%q want tool", role)
	}
	if id, _ := msgAt(t, msgs, 3)["tool_call_id"].(string); id != "call_1" {
		t.Errorf("tool_call_id=%q", id)
	}

	tools, _ := chat["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools len=%d want 1", len(tools))
	}
	fn, _ := tools[0].(map[string]any)["function"].(map[string]any)
	if name, _ := fn["name"].(string); name != "shell" {
		t.Errorf("tool name=%q", name)
	}
	if _, leaked := fn["strict"]; leaked {
		t.Errorf("strict should be dropped from chat tool")
	}

	if tc, _ := chat["tool_choice"].(string); tc != "auto" {
		t.Errorf("tool_choice=%v", chat["tool_choice"])
	}
	if v, _ := chat["max_tokens"].(float64); v != 1024 {
		t.Errorf("max_tokens=%v", chat["max_tokens"])
	}
	if v, _ := chat["reasoning_effort"].(string); v != "low" {
		t.Errorf("minimal should map to low, got %q", v)
	}
	if meta.ParallelToolCalls == nil || *meta.ParallelToolCalls {
		t.Errorf("parallel_tool_calls should be captured as false")
	}
	if !meta.Stream {
		t.Errorf("meta.Stream should be true")
	}
}

// TestConvertCustomTool custom（freeform）工具必须包装成单 input 字符串参数的
// function，并记入 Meta.CustomTools（响应侧据此还原 custom_tool_call）。
func TestConvertCustomTool(t *testing.T) {
	body := `{
		"model":"deepseek-v4",
		"input":"patch it",
		"tools":[{"type":"custom","name":"apply_patch","description":"apply a patch","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}}]
	}`
	raw, meta, err := ConvertRequest([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !meta.CustomTools["apply_patch"] {
		t.Fatalf("apply_patch not recorded as custom tool")
	}
	chat := decodeChat(t, raw)
	tools, _ := chat["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools len=%d", len(tools))
	}
	fn, _ := tools[0].(map[string]any)["function"].(map[string]any)
	params, _ := fn["parameters"].(map[string]any)
	props, _ := params["properties"].(map[string]any)
	if _, ok := props["input"]; !ok {
		t.Fatalf("custom tool should expose an `input` string property: %v", params)
	}
	if _, hasFormat := fn["format"]; hasFormat {
		t.Errorf("format must not leak into chat tool")
	}
}

// TestConvertInputImage 图片 part 转成 chat 的 image_url 对象形态。
func TestConvertInputImage(t *testing.T) {
	body := `{"model":"glm-5v","input":[{"type":"message","role":"user","content":[
		{"type":"input_text","text":"看图"},
		{"type":"input_image","image_url":"data:image/png;base64,AAAA","detail":"low"}
	]}]}`
	raw, _, err := ConvertRequest([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	chat := decodeChat(t, raw)
	content, ok := msgAt(t, messagesOf(t, chat), 0)["content"].([]any)
	if !ok {
		t.Fatalf("multimodal content should stay an array")
	}
	if len(content) != 2 {
		t.Fatalf("content parts=%d want 2", len(content))
	}
	img, _ := content[1].(map[string]any)["image_url"].(map[string]any)
	if url, _ := img["url"].(string); url != "data:image/png;base64,AAAA" {
		t.Errorf("image url=%q", url)
	}
	if detail, _ := img["detail"].(string); detail != "low" {
		t.Errorf("detail=%q", detail)
	}
}

// TestConvertToolOutputParts function_call_output 的 part 数组应压成文本。
func TestConvertToolOutputParts(t *testing.T) {
	body := `{"model":"m","input":[
		{"type":"function_call","call_id":"c1","name":"t","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":[{"type":"input_text","text":"line1\n"},{"type":"input_text","text":"line2"}]}
	]}`
	raw, _, err := ConvertRequest([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	chat := decodeChat(t, raw)
	msgs := messagesOf(t, chat)
	content, _ := msgAt(t, msgs, 1)["content"].(string)
	if content != "line1\nline2" {
		t.Errorf("tool output=%q", content)
	}
}

// TestConvertUnsupportedStatelessParams previous_response_id / background 等
// 必须明确拒绝，不能静默忽略（否则客户端以为上下文还在）。
func TestConvertUnsupportedStatelessParams(t *testing.T) {
	cases := []string{
		`{"model":"m","input":"x","previous_response_id":"resp_1"}`,
		`{"model":"m","input":"x","background":true}`,
		`{"model":"m","input":"x","conversation":"conv_1"}`,
		`{"model":"m","input":"x","prompt":{"id":"p"}}`,
	}
	for _, body := range cases {
		_, meta, err := ConvertRequest([]byte(body))
		if err != nil {
			t.Fatalf("%s: unexpected error %v", body, err)
		}
		if meta.Unsupported == "" {
			t.Errorf("%s: Unsupported should be set", body)
		}
	}
	// null / 空串是客户端默认值，不应误判。
	_, meta, err := ConvertRequest([]byte(`{"model":"m","input":"x","previous_response_id":null}`))
	if err != nil || meta.Unsupported != "" {
		t.Errorf("null previous_response_id should be accepted, err=%v unsupported=%q", err, meta.Unsupported)
	}
}

// TestConvertRejectsInvalid 缺 model / 空 input / 非法 JSON 均返回错误。
func TestConvertRejectsInvalid(t *testing.T) {
	cases := []string{
		`not json`,
		`{"input":"x"}`,
		`{"model":"m"}`,
		`{"model":"m","input":123}`,
	}
	for _, body := range cases {
		if _, _, err := ConvertRequest([]byte(body)); err == nil {
			t.Errorf("%s: expected error", body)
		}
	}
}

// TestConvertMetadataAndCacheKey prompt_cache_key 映射成 metadata.conversation_id，
// 供网关会话粘性/上游前缀缓存使用。
func TestConvertMetadataAndCacheKey(t *testing.T) {
	raw, meta, err := ConvertRequest([]byte(`{"model":"m","input":"x","prompt_cache_key":"sess-1","metadata":{"user":"u"}}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if meta.PromptCacheKey != "sess-1" {
		t.Errorf("PromptCacheKey=%q", meta.PromptCacheKey)
	}
	chat := decodeChat(t, raw)
	md, _ := chat["metadata"].(map[string]any)
	if v, _ := md["conversation_id"].(string); v != "sess-1" {
		t.Errorf("conversation_id=%v", md["conversation_id"])
	}
	if v, _ := md["user"].(string); v != "u" {
		t.Errorf("metadata.user lost: %v", md)
	}
}

// TestConvertNestedFunctionTool 兼容 chat 嵌套形态的 function 声明。
func TestConvertNestedFunctionTool(t *testing.T) {
	body := `{"model":"m","input":"x","tools":[{"type":"function","function":{"name":"shell","description":"d","parameters":{"type":"object","properties":{"command":{"type":"string"}}}}}]}`
	raw, _, err := ConvertRequest([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	chat := decodeChat(t, raw)
	tools, _ := chat["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools len=%d", len(tools))
	}
	fn, _ := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "shell" || fn["description"] != "d" || fn["parameters"] == nil {
		t.Fatalf("nested function not carried: %v", fn)
	}
}

// TestConvertToolOutputObject 工具输出为对象时应序列化成 JSON 文本，而不是
// fmt 的 Go 语法（map[...]）。
func TestConvertToolOutputObject(t *testing.T) {
	body := `{"model":"m","input":[
		{"type":"function_call","call_id":"c1","name":"t","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":{"ok":true,"count":2}}
	]}`
	raw, _, err := ConvertRequest([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	chat := decodeChat(t, raw)
	msgs := messagesOf(t, chat)
	content, _ := msgAt(t, msgs, 1)["content"].(string)
	if content != `{"count":2,"ok":true}` {
		t.Errorf("object tool output=%q", content)
	}
}

// TestConvertToolChoiceEcho tool_choice / truncation / service_tier 必须回显，
// 且被抓进 Meta（响应对象按它构建）。
func TestConvertToolChoiceEcho(t *testing.T) {
	body := `{"model":"m","input":"x","tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}],
		"tool_choice":{"type":"function","name":"shell"},"truncation":"auto","service_tier":"flex"}`
	raw, meta, err := ConvertRequest([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	chat := decodeChat(t, raw)
	tc, _ := chat["tool_choice"].(map[string]any)
	fn, _ := tc["function"].(map[string]any)
	if fn["name"] != "shell" {
		t.Fatalf("chat tool_choice=%v", chat["tool_choice"])
	}
	if meta.ToolChoice == nil {
		t.Fatalf("Meta.ToolChoice not captured (回显会退化成 auto)")
	}
	if meta.Truncation != "auto" || meta.ServiceTier != "flex" {
		t.Fatalf("meta truncation=%q service_tier=%q", meta.Truncation, meta.ServiceTier)
	}
	resp := buildResponse("resp_x", meta, "completed", []any{}, nil, nil)
	if resp["truncation"] != "auto" || resp["service_tier"] != "flex" {
		t.Fatalf("response echo truncation=%v service_tier=%v", resp["truncation"], resp["service_tier"])
	}

	// custom 工具的 tool_choice 回显应保留 custom 类型（不能一律写成 function）。
	body2 := `{"model":"m","input":"x","tools":[{"type":"custom","name":"apply_patch"}],
		"tool_choice":{"type":"custom","name":"apply_patch"}}`
	_, meta2, err := ConvertRequest([]byte(body2))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tc2, _ := meta2.ToolChoice.(map[string]any)
	if tc2["type"] != "custom" || tc2["name"] != "apply_patch" {
		t.Fatalf("custom tool_choice echo=%v", meta2.ToolChoice)
	}
}

// TestConvertEchoToolsFiltered 被丢弃的内置工具不应出现在回显工具集里。
func TestConvertEchoToolsFiltered(t *testing.T) {
	body := `{"model":"m","input":"x","tools":[
		{"type":"function","name":"shell","parameters":{"type":"object"}},
		{"type":"custom","name":"apply_patch"},
		{"type":"web_search"}]}`
	_, meta, err := ConvertRequest([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(meta.Tools) != 2 {
		t.Fatalf("echo tools=%d want 2 (web_search 应被过滤)", len(meta.Tools))
	}
}

// TestConvertInstructionsOnlyRejected 只有 instructions 没有 input 必须 400。
func TestConvertInstructionsOnlyRejected(t *testing.T) {
	if _, _, err := ConvertRequest([]byte(`{"model":"m","instructions":"be nice"}`)); err == nil {
		t.Fatal("instructions-only request should be rejected")
	}
}

// TestConvertTextFormat text.format 的三种形态映射。
func TestConvertTextFormat(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string // response_format.type；"" 表示不下发
	}{
		{"json_object", `{"format":{"type":"json_object"}}`, "json_object"},
		{"json_schema", `{"format":{"type":"json_schema","name":"s","schema":{"type":"object"},"strict":true}}`, "json_schema"},
		{"text", `{"format":{"type":"text"},"verbosity":"low"}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := `{"model":"m","input":"x","text":` + c.text + `}`
			raw, _, err := ConvertRequest([]byte(body))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			chat := decodeChat(t, raw)
			rf, _ := chat["response_format"].(map[string]any)
			got, _ := rf["type"].(string)
			if got != c.want {
				t.Fatalf("response_format.type=%q want %q (%v)", got, c.want, chat["response_format"])
			}
			if c.want == "json_schema" {
				js, _ := rf["json_schema"].(map[string]any)
				if js["name"] != "s" || js["strict"] != true || js["schema"] == nil {
					t.Fatalf("json_schema=%v", js)
				}
			}
		})
	}
}

// TestConvertDeveloperRole developer 角色在入站即归一为 system，保证提示词改写/
// 脱敏/SystemAsUser 管线能识别到它。
func TestConvertDeveloperRole(t *testing.T) {
	body := `{"model":"m","input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"env: linux"}]},{"type":"message","role":"user","content":"hi"}]}`
	raw, _, err := ConvertRequest([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	chat := decodeChat(t, raw)
	msgs := messagesOf(t, chat)
	if role, _ := msgAt(t, msgs, 0)["role"].(string); role != "system" {
		t.Fatalf("developer role=%q want system", role)
	}
}

// TestConvertUnusableInputRejected 只有 reasoning/item_reference 的 input 无法
// 还原成任何消息，必须 400。
func TestConvertUnusableInputRejected(t *testing.T) {
	body := `{"model":"m","input":[{"type":"reasoning","summary":[]},{"type":"item_reference","id":"x"}]}`
	if _, _, err := ConvertRequest([]byte(body)); err == nil {
		t.Fatal("input with no usable items should be rejected")
	}
}

// TestConvertDropsInputFile 文件输入上游不支持：丢弃但保留同条消息的文本。
func TestConvertDropsInputFile(t *testing.T) {
	body := `{"model":"m","input":[{"type":"message","role":"user","content":[
		{"type":"input_text","text":"看这个文件"},
		{"type":"input_file","file_id":"file_123","filename":"a.txt"}]}]}`
	raw, _, err := ConvertRequest([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	chat := decodeChat(t, raw)
	content, _ := msgAt(t, messagesOf(t, chat), 0)["content"].(string)
	if content != "看这个文件" {
		t.Fatalf("content=%q (input_file 应被丢弃且不影响文本)", content)
	}
}

// TestConvertDropsImageFileID file_id 形态的图片无法解析：丢弃并只保留文本。
func TestConvertDropsImageFileID(t *testing.T) {
	body := `{"model":"m","input":[{"type":"message","role":"user","content":[
		{"type":"input_text","text":"看图"},
		{"type":"input_image","file_id":"file_img"}]}]}`
	raw, _, err := ConvertRequest([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	chat := decodeChat(t, raw)
	content, _ := msgAt(t, messagesOf(t, chat), 0)["content"].(string)
	if content != "看图" {
		t.Fatalf("content=%q", content)
	}
}

// TestConvertDropsUnsupportedToolTypes 内置工具丢弃但请求仍可继续。
func TestConvertDropsUnsupportedToolTypes(t *testing.T) {
	body := `{"model":"m","input":"x","tools":[
		{"type":"function","name":"shell","parameters":{"type":"object"}},
		{"type":"web_search"},
		{"type":"local_shell"}
	]}`
	raw, meta, err := ConvertRequest([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	chat := decodeChat(t, raw)
	tools, _ := chat["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools len=%d want 1 (内置工具应被丢弃)", len(tools))
	}
	if len(meta.CustomTools) != 0 {
		t.Errorf("web_search/local_shell must not be treated as custom tools")
	}
}
