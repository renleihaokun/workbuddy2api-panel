package responses

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
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

// toolNamesOf 取 chat tools 里每个 function 的 name，保持顺序。
func toolNamesOf(t *testing.T, tools []any) []string {
	t.Helper()
	out := make([]string, 0, len(tools))
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		fn, _ := tool["function"].(map[string]any)
		name, _ := fn["name"].(string)
		out = append(out, name)
	}
	return out
}

// TestConvertNamespaceToolsFlattened namespace 里的 function/custom 都要摊平成
// 普通 chat function，原始身份记进 Meta.ToolRefs；回显保留 namespace 组。
func TestConvertNamespaceToolsFlattened(t *testing.T) {
	body := `{"model":"m","input":"x","tools":[
		{"type":"namespace","name":"container","description":"Shell tools","tools":[
			{"type":"function","name":"run_shell","description":"run a shell","parameters":{"type":"object","properties":{"command":{"type":"string"}}}},
			{"type":"custom","name":"apply_patch","description":"apply a patch","format":{"type":"grammar","syntax":"lark","definition":"x"}}
		]}]}`
	raw, meta, err := ConvertRequest([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	chat := decodeChat(t, raw)
	tools, _ := chat["tools"].([]any)
	names := toolNamesOf(t, tools)
	if len(names) != 2 || names[0] != "container.run_shell" || names[1] != "container.apply_patch" {
		t.Fatalf("chat tool names=%v want [container.run_shell container.apply_patch]", names)
	}
	if !meta.CustomTools["container.apply_patch"] {
		t.Errorf("namespaced custom tool not recorded: %v", meta.CustomTools)
	}
	ref, ok := meta.ToolRefs["container.run_shell"]
	if !ok || ref.Namespace != "container" || ref.Name != "run_shell" {
		t.Fatalf("ToolRefs[container.run_shell]=%+v ok=%v", ref, ok)
	}
	fn, _ := tools[0].(map[string]any)["function"].(map[string]any)
	if desc, _ := fn["description"].(string); !strings.Contains(desc, "Shell tools") {
		t.Errorf("namespace description not prefixed: %q", desc)
	}
	if len(meta.Tools) != 1 {
		t.Fatalf("echo tools len=%d want 1 (namespace 组应保留)", len(meta.Tools))
	}
	echo, _ := meta.Tools[0].(map[string]any)
	if echo["type"] != "namespace" {
		t.Errorf("echo tool type=%v want namespace", echo["type"])
	}
}

// TestConvertDefaultNamespaceBare 默认 functions 命名空间的子工具用裸名；
// 与顶层工具重名时按重复声明跳过，不加后缀。
func TestConvertDefaultNamespaceBare(t *testing.T) {
	body := `{"model":"m","input":"x","tools":[
		{"type":"function","name":"shell","parameters":{"type":"object"}},
		{"type":"namespace","name":"functions","tools":[
			{"type":"function","name":"shell"},
			{"type":"function","name":"read_file"}
		]}]}`
	raw, meta, err := ConvertRequest([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	chat := decodeChat(t, raw)
	names := toolNamesOf(t, chat["tools"].([]any))
	if len(names) != 2 || names[0] != "shell" || names[1] != "read_file" {
		t.Fatalf("chat tool names=%v want [shell read_file]", names)
	}
	if len(meta.ToolRefs) != 0 {
		t.Errorf("default namespace tools must not need ToolRefs: %v", meta.ToolRefs)
	}
}

// TestConvertNamespaceHistory 历史里的 namespace 调用要还原成本次声明算出的
// 扁平名；没有 tools 声明时也要能重建，并记住映射供响应侧使用。
func TestConvertNamespaceHistory(t *testing.T) {
	body := `{"model":"m","input":[
		{"type":"function_call","call_id":"c1","name":"run_shell","namespace":"container","arguments":"{\"command\":\"pwd\"}"},
		{"type":"function_call_output","call_id":"c1","output":"/tmp"},
		{"type":"custom_tool_call","call_id":"c2","name":"apply_patch","namespace":"container","input":"*** Begin Patch\n*** End Patch"},
		{"type":"custom_tool_call_output","call_id":"c2","output":"ok"}
	]}`
	raw, meta, err := ConvertRequest([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	chat := decodeChat(t, raw)
	msgs := messagesOf(t, chat)
	if len(msgs) < 4 {
		t.Fatalf("messages len=%d want 4 (call/output/call/output)", len(msgs))
	}
	firstCall, _ := msgAt(t, msgs, 0)["tool_calls"].([]any)
	if len(firstCall) != 1 {
		t.Fatalf("first assistant tool_calls=%d want 1", len(firstCall))
	}
	firstFn, _ := firstCall[0].(map[string]any)["function"].(map[string]any)
	if firstFn["name"] != "container.run_shell" {
		t.Errorf("first tool name=%v want container.run_shell", firstFn["name"])
	}
	secondCall, _ := msgAt(t, msgs, 2)["tool_calls"].([]any)
	if len(secondCall) != 1 {
		t.Fatalf("second assistant tool_calls=%d want 1", len(secondCall))
	}
	secondFn, _ := secondCall[0].(map[string]any)["function"].(map[string]any)
	if secondFn["name"] != "container.apply_patch" {
		t.Errorf("second tool name=%v want container.apply_patch", secondFn["name"])
	}
	if !meta.CustomTools["container.apply_patch"] {
		t.Errorf("history custom call not recorded under flat name: %v", meta.CustomTools)
	}
	ref, ok := meta.ToolRefs["container.run_shell"]
	if !ok || ref.Namespace != "container" || ref.Name != "run_shell" {
		t.Fatalf("history ToolRefs=%+v ok=%v", ref, ok)
	}
}

// TestConvertDefaultNamespaceHistory 历史里 namespace=functions 的调用保持裸名。
func TestConvertDefaultNamespaceHistory(t *testing.T) {
	body := `{"model":"m","input":[
		{"type":"function_call","call_id":"c1","name":"run_shell","namespace":"functions","arguments":"{}"}
	]}`
	raw, _, err := ConvertRequest([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	chat := decodeChat(t, raw)
	assistant := msgAt(t, messagesOf(t, chat), 0)
	tcs, _ := assistant["tool_calls"].([]any)
	fn, _ := tcs[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "run_shell" {
		t.Errorf("default namespace history name=%v want run_shell", fn["name"])
	}
}

// TestConvertNamespaceToolChoice tool_choice 带 namespace 时要换成 chat 扁平名，
// 回显保留原始 namespace。
func TestConvertNamespaceToolChoice(t *testing.T) {
	body := `{"model":"m","input":"x","tools":[
		{"type":"namespace","name":"container","tools":[
			{"type":"function","name":"run_shell","parameters":{"type":"object"}}
		]}],
		"tool_choice":{"type":"function","name":"run_shell","namespace":"container"}}`
	raw, meta, err := ConvertRequest([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	chat := decodeChat(t, raw)
	tc, _ := chat["tool_choice"].(map[string]any)
	fn, _ := tc["function"].(map[string]any)
	if fn["name"] != "container.run_shell" {
		t.Fatalf("chat tool_choice name=%v want container.run_shell", fn["name"])
	}
	echo, _ := meta.ToolChoice.(map[string]any)
	if echo["namespace"] != "container" || echo["name"] != "run_shell" {
		t.Fatalf("tool_choice echo=%v", meta.ToolChoice)
	}

	// 客户端直接给扁平名（没有 namespace）也要能用。
	body2 := `{"model":"m","input":"x","tools":[
		{"type":"namespace","name":"container","tools":[
			{"type":"function","name":"run_shell","parameters":{"type":"object"}}
		]}],
		"tool_choice":{"type":"function","name":"container.run_shell"}}`
	raw2, _, err := ConvertRequest([]byte(body2))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tc2, _ := decodeChat(t, raw2)["tool_choice"].(map[string]any)
	fn2, _ := tc2["function"].(map[string]any)
	if fn2["name"] != "container.run_shell" {
		t.Errorf("flat tool_choice name=%v", fn2["name"])
	}
}

// TestConvertNamespaceCollision 顶层工具已占用扁平名时，namespace 子工具加后缀，
// 不丢能力也不覆盖顶层工具。
func TestConvertNamespaceCollision(t *testing.T) {
	body := `{"model":"m","input":"x","tools":[
		{"type":"function","name":"container.run_shell","parameters":{"type":"object"}},
		{"type":"namespace","name":"container","tools":[
			{"type":"function","name":"run_shell","parameters":{"type":"object"}}
		]}]}`
	raw, meta, err := ConvertRequest([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	chat := decodeChat(t, raw)
	names := toolNamesOf(t, chat["tools"].([]any))
	if len(names) != 2 || names[0] != "container.run_shell" || names[1] != "container.run_shell_2" {
		t.Fatalf("chat tool names=%v want [container.run_shell container.run_shell_2]", names)
	}
	ref, ok := meta.ToolRefs["container.run_shell_2"]
	if !ok || ref.Namespace != "container" || ref.Name != "run_shell" {
		t.Fatalf("collision ToolRefs=%+v ok=%v", ref, ok)
	}
}

// TestConvertNamespaceNameCap 超长扁平名要确定性截断到 64 字符，映射仍可还原。
func TestConvertNamespaceNameCap(t *testing.T) {
	ns := "mcp__very_long_namespace_name_for_testing_cap"
	toolName := strings.Repeat("x", 40)
	body := fmt.Sprintf(`{"model":"m","input":"x","tools":[
		{"type":"namespace","name":%q,"tools":[
			{"type":"function","name":%q,"parameters":{"type":"object"}}
		]}]}`, ns, toolName)
	raw, meta, err := ConvertRequest([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	names := toolNamesOf(t, decodeChat(t, raw)["tools"].([]any))
	if len(names) != 1 {
		t.Fatalf("tools len=%d", len(names))
	}
	flat := names[0]
	if len(flat) != maxChatToolNameLen {
		t.Fatalf("flat name len=%d want %d (%q)", len(flat), maxChatToolNameLen, flat)
	}
	ref, ok := meta.ToolRefs[flat]
	if !ok || ref.Namespace != ns || ref.Name != toolName {
		t.Fatalf("capped ToolRefs=%+v ok=%v", ref, ok)
	}
	// 同一输入必须得到同一个截断名。
	raw2, _, err := ConvertRequest([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if names2 := toolNamesOf(t, decodeChat(t, raw2)["tools"].([]any)); len(names2) != 1 || names2[0] != flat {
		t.Fatalf("capped name not deterministic: %v vs %q", names2, flat)
	}
}

// TestConvertNamespaceLogs namespace 摊平要有 INFO 汇总；web_search 必须 WARN，
// 不能再出现旧的 namespace INFO 丢弃日志。
func TestConvertNamespaceLogs(t *testing.T) {
	old := log.Writer()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	body := `{"model":"m","input":"x","tools":[
		{"type":"web_search"},
		{"type":"namespace","name":"container","tools":[
			{"type":"function","name":"run_shell","parameters":{"type":"object"}}
		]}]}`
	if _, _, err := ConvertRequest([]byte(body)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "WARN") || !strings.Contains(out, "web_search") {
		t.Fatalf("web_search should be dropped at WARN, got: %s", out)
	}
	if !strings.Contains(out, "flattened namespace tools") || !strings.Contains(out, "groups=1") {
		t.Fatalf("missing namespace flatten INFO summary: %s", out)
	}
	if strings.Contains(out, `ignored unsupported tool type="namespace"`) {
		t.Fatalf("namespace should not be logged as ignored: %s", out)
	}
}

// TestConvertNamespaceMalformedContinues 畸形 namespace 只丢自己，不影响同请求
// 里其他可摊平的子工具。
func TestConvertNamespaceMalformedContinues(t *testing.T) {
	body := `{"model":"m","input":"x","tools":[
		{"type":"namespace"},
		{"type":"namespace","name":"empty","tools":[]},
		{"type":"namespace","name":"bad","tools":"nope"},
		{"type":"namespace","name":"mixed","tools":[
			{"type":"function"},
			{"type":"mystery","name":"x"},
			{"type":"function","name":"ok","parameters":{"type":"object"}}
		]}]}`
	raw, meta, err := ConvertRequest([]byte(body))
	if err != nil {
		t.Fatalf("malformed namespace must not reject the request: %v", err)
	}
	names := toolNamesOf(t, decodeChat(t, raw)["tools"].([]any))
	if len(names) != 1 || names[0] != "mixed.ok" {
		t.Fatalf("chat tool names=%v want [mixed.ok]", names)
	}
	if _, ok := meta.ToolRefs["mixed.ok"]; !ok {
		t.Errorf("mixed.ok not recorded in ToolRefs: %v", meta.ToolRefs)
	}
}

// TestConvertEchoToolsIncludesNamespace namespace 组属于受支持工具，回显要保留，
// 但组内不支持的类型/无名字条目要过滤掉。
func TestConvertEchoToolsIncludesNamespace(t *testing.T) {
	body := `{"model":"m","input":"x","tools":[
		{"type":"namespace","name":"container","tools":[
			{"type":"function","name":"run_shell"},
			{"type":"mystery","name":"x"},
			{"type":"custom"}
		]},
		{"type":"web_search"}
	]}`
	_, meta, err := ConvertRequest([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(meta.Tools) != 1 {
		t.Fatalf("echo tools len=%d want 1: %v", len(meta.Tools), meta.Tools)
	}
	group, _ := meta.Tools[0].(map[string]any)
	children, _ := group["tools"].([]any)
	if len(children) != 1 {
		t.Fatalf("echo namespace children=%d want 1: %v", len(children), children)
	}
	child, _ := children[0].(map[string]any)
	if child["name"] != "run_shell" {
		t.Errorf("echo child=%v", child)
	}
}
