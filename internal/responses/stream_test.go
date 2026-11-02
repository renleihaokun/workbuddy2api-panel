package responses

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

// sseEvent 解析后的一条 Responses 事件。
type sseEvent struct {
	Name string
	Data map[string]any
}

// parseEvents 把翻译器写出的 SSE 文本解析成事件列表。
func parseEvents(t *testing.T, raw string) []sseEvent {
	t.Helper()
	var out []sseEvent
	for _, block := range strings.Split(strings.TrimSpace(raw), "\n\n") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		var name string
		var dataLines []string
		for _, line := range strings.Split(block, "\n") {
			line = strings.TrimRight(line, "\r")
			switch {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				dataLines = append(dataLines, strings.TrimPrefix(line, "data: "))
			}
		}
		data := strings.Join(dataLines, "\n")
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatalf("event %q has invalid JSON: %v (%s)", name, err, data)
		}
		out = append(out, sseEvent{Name: name, Data: payload})
	}
	return out
}

func eventNames(events []sseEvent) []string {
	names := make([]string, len(events))
	for i, e := range events {
		names[i] = e.Name
	}
	return names
}

func wantNames(t *testing.T, events []sseEvent, want []string) {
	t.Helper()
	got := eventNames(events)
	if len(got) != len(want) {
		t.Fatalf("event count=%d want %d\n got=%v\nwant=%v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event[%d]=%q want %q\n got=%v", i, got[i], want[i], got)
		}
	}
}

// TestStreamText 文本流的完整事件序列：created → item.added → part.added →
// delta* → text.done → part.done → item.done → completed。
func TestStreamText(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := NewTranslator(rec, Meta{Model: "deepseek-v4"})
	if err := tr.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	frames := []string{
		`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"}}]}` + "\n\n",
		`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"lo"}}]}` + "\n\n",
		`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}` + "\n\n",
		"data: [DONE]\n\n",
	}
	for _, f := range frames {
		if err := tr.WriteFrame([]byte(f)); err != nil {
			t.Fatalf("write frame: %v", err)
		}
	}
	if err := tr.Finish(nil); err != nil {
		t.Fatalf("finish: %v", err)
	}

	events := parseEvents(t, rec.Body.String())
	wantNames(t, events, []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	})

	// sequence_number 必须从 0 开始严格递增。
	for i, e := range events {
		seq, ok := e.Data["sequence_number"].(float64)
		if !ok || int(seq) != i {
			t.Fatalf("event[%d] sequence_number=%v want %d", i, e.Data["sequence_number"], i)
		}
	}

	// 首个 delta 之前必须已经发过 content_part.added（Codex 的硬要求）。
	for i, e := range events {
		if e.Name != "response.output_text.delta" {
			continue
		}
		found := false
		for _, prev := range events[:i] {
			if prev.Name == "response.content_part.added" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("output_text.delta before content_part.added")
		}
		break
	}

	completed := events[len(events)-1]
	resp, _ := completed.Data["response"].(map[string]any)
	if resp["status"] != "completed" {
		t.Errorf("status=%v", resp["status"])
	}
	usage, _ := resp["usage"].(map[string]any)
	if v, _ := usage["input_tokens"].(float64); v != 10 {
		t.Errorf("usage.input_tokens=%v", usage["input_tokens"])
	}
	out, _ := resp["output"].([]any)
	if len(out) != 1 {
		t.Fatalf("output len=%d", len(out))
	}
	msg, _ := out[0].(map[string]any)
	content, _ := msg["content"].([]any)
	part, _ := content[0].(map[string]any)
	if part["text"] != "Hello" {
		t.Errorf("text=%q", part["text"])
	}
}

// TestStreamToolCall 工具调用：item.added → arguments.delta* → done → item.done。
func TestStreamToolCall(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := NewTranslator(rec, Meta{Model: "m"})
	frames := []string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"shell","arguments":"{\"command\":"}}]}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"[\"ls\"]}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n",
	}
	for _, f := range frames {
		if err := tr.WriteFrame([]byte(f)); err != nil {
			t.Fatalf("write frame: %v", err)
		}
	}
	if err := tr.Finish(nil); err != nil {
		t.Fatalf("finish: %v", err)
	}
	events := parseEvents(t, rec.Body.String())
	wantNames(t, events, []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		"response.output_item.done",
		"response.completed",
	})

	added, _ := events[2].Data["item"].(map[string]any)
	if added["type"] != "function_call" || added["call_id"] != "call_1" || added["name"] != "shell" {
		t.Fatalf("added item=%v", added)
	}
	done := events[5].Data
	if done["arguments"] != `{"command":["ls"]}` {
		t.Errorf("arguments=%v", done["arguments"])
	}
	completed := events[len(events)-1]
	resp, _ := completed.Data["response"].(map[string]any)
	out, _ := resp["output"].([]any)
	item, _ := out[0].(map[string]any)
	if item["arguments"] != `{"command":["ls"]}` {
		t.Errorf("completed item arguments=%v", item["arguments"])
	}
}

// TestStreamCustomTool custom 工具的流式往返：事件名与 item 类型都必须是
// custom_tool_call（否则 Codex 判 apply_patch 不兼容）。
func TestStreamCustomTool(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := NewTranslator(rec, Meta{Model: "m", CustomTools: map[string]bool{"apply_patch": true}})
	frames := []string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_p","type":"function","function":{"name":"apply_patch","arguments":"{\"input\":\"*** Begin"}}]}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":" Patch\\n*** End Patch\"}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n",
	}
	for _, f := range frames {
		if err := tr.WriteFrame([]byte(f)); err != nil {
			t.Fatalf("write frame: %v", err)
		}
	}
	if err := tr.Finish(nil); err != nil {
		t.Fatalf("finish: %v", err)
	}
	events := parseEvents(t, rec.Body.String())
	names := eventNames(events)
	var deltaText, doneText string
	for _, e := range events {
		switch e.Name {
		case "response.custom_tool_call_input.delta":
			if s, _ := e.Data["delta"].(string); s != "" {
				deltaText += s
			}
		case "response.custom_tool_call_input.done":
			doneText, _ = e.Data["input"].(string)
		}
	}
	if deltaText != "*** Begin Patch\n*** End Patch" {
		t.Fatalf("custom input delta=%q (must carry the decoded freeform text)", deltaText)
	}
	if doneText != "*** Begin Patch\n*** End Patch" {
		t.Fatalf("custom input done=%q", doneText)
	}
	if !strings.Contains(strings.Join(names, ","), "response.custom_tool_call_input.done") {
		t.Fatalf("custom_tool_call_input.done missing: %v", names)
	}
	resp, _ := events[len(events)-1].Data["response"].(map[string]any)
	out, _ := resp["output"].([]any)
	item, _ := out[0].(map[string]any)
	if item["type"] != "custom_tool_call" {
		t.Fatalf("item type=%v", item["type"])
	}
	if item["input"] != "*** Begin Patch\n*** End Patch" {
		t.Errorf("input=%q", item["input"])
	}
}

// TestStreamErrorFrame 上游 error 帧 → error + response.failed，且不再发 completed。
func TestStreamErrorFrame(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := NewTranslator(rec, Meta{Model: "m"})
	if err := tr.WriteFrame([]byte(`data: {"error":{"message":"rate limited","code":"6004"}}` + "\n\n")); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	if err := tr.Finish(nil); err != nil {
		t.Fatalf("finish: %v", err)
	}
	events := parseEvents(t, rec.Body.String())
	wantNames(t, events, []string{
		"response.created",
		"response.in_progress",
		"error",
		"response.failed",
	})
	errEvent := events[2].Data
	if errEvent["code"] != "6004" || errEvent["message"] != "rate limited" {
		t.Errorf("error event=%v", errEvent)
	}
	failed, _ := events[3].Data["response"].(map[string]any)
	if failed["status"] != "failed" {
		t.Errorf("status=%v", failed["status"])
	}
}

// TestStreamTruncatedToolCallKeepsLifecycle finish_reason=length 时残缺的
// arguments 仍要收口：item 以 status=incomplete 出现在 output，且不得留悬空 added。
func TestStreamTruncatedToolCallKeepsLifecycle(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := NewTranslator(rec, Meta{Model: "m"})
	frame := `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"shell","arguments":"{\"command\":["}}]},"finish_reason":"length"}]}` + "\n\n"
	if err := tr.WriteFrame([]byte(frame)); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	if err := tr.Finish(nil); err != nil {
		t.Fatalf("finish: %v", err)
	}
	events := parseEvents(t, rec.Body.String())
	var fcDone map[string]any
	for _, e := range events {
		if e.Name != "response.output_item.done" {
			continue
		}
		item, _ := e.Data["item"].(map[string]any)
		if item["type"] == "function_call" {
			fcDone = item
		}
	}
	if fcDone == nil {
		t.Fatalf("truncated tool call must still emit output_item.done (不能留悬空 added): %v", eventNames(events))
	}
	if fcDone["status"] != "incomplete" {
		t.Fatalf("truncated item status=%v want incomplete", fcDone["status"])
	}
	last := events[len(events)-1]
	if last.Name != "response.incomplete" {
		t.Fatalf("last=%q want response.incomplete", last.Name)
	}
	resp, _ := last.Data["response"].(map[string]any)
	out, _ := resp["output"].([]any)
	found := false
	for _, raw := range out {
		if item, _ := raw.(map[string]any); item["type"] == "function_call" && item["status"] == "incomplete" {
			found = true
		}
	}
	if !found {
		t.Fatalf("truncated item missing from output: %v", out)
	}
}

// TestStreamMixedTextAndTool 文本与工具调用混合：message item 必须先创建，
// 后续工具 item 的 output_index 顺延；收尾时按创建顺序关闭。
func TestStreamMixedTextAndTool(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := NewTranslator(rec, Meta{Model: "m"})
	frames := []string{
		`data: {"choices":[{"index":0,"delta":{"content":"先说明一下"}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"shell","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n",
	}
	for _, f := range frames {
		if err := tr.WriteFrame([]byte(f)); err != nil {
			t.Fatalf("write frame: %v", err)
		}
	}
	if err := tr.Finish(nil); err != nil {
		t.Fatalf("finish: %v", err)
	}
	events := parseEvents(t, rec.Body.String())
	var addedTypes []string
	var addedIndexes []int
	for _, e := range events {
		if e.Name != "response.output_item.added" {
			continue
		}
		item, _ := e.Data["item"].(map[string]any)
		addedTypes = append(addedTypes, item["type"].(string))
		addedIndexes = append(addedIndexes, int(e.Data["output_index"].(float64)))
	}
	if len(addedTypes) != 2 || addedTypes[0] != "message" || addedTypes[1] != "function_call" {
		t.Fatalf("added types=%v want [message function_call]", addedTypes)
	}
	if addedIndexes[0] != 0 || addedIndexes[1] != 1 {
		t.Fatalf("output indexes=%v want [0 1]", addedIndexes)
	}
	last := events[len(events)-1]
	resp, _ := last.Data["response"].(map[string]any)
	out, _ := resp["output"].([]any)
	if len(out) != 2 {
		t.Fatalf("output len=%d want 2", len(out))
	}
}

// TestStreamEmptyOutputFallback 上游只回 finish/usage（无正文无工具）时，
// 仍要产出一个空 message item，避免 output:[]。
func TestStreamEmptyOutputFallback(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := NewTranslator(rec, Meta{Model: "m"})
	if err := tr.WriteFrame([]byte(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":0,"total_tokens":1}}` + "\n\n")); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	if err := tr.Finish(nil); err != nil {
		t.Fatalf("finish: %v", err)
	}
	events := parseEvents(t, rec.Body.String())
	last := events[len(events)-1]
	if last.Name != "response.completed" {
		t.Fatalf("last=%q", last.Name)
	}
	resp, _ := last.Data["response"].(map[string]any)
	out, _ := resp["output"].([]any)
	if len(out) != 1 {
		t.Fatalf("output len=%d want 1 (空 message 兜底)", len(out))
	}
	item, _ := out[0].(map[string]any)
	if item["type"] != "message" {
		t.Fatalf("item=%v", item)
	}
	// 事件序列仍必须完整：added → content_part.added → text.done → part.done → item.done。
	wantNames(t, events, []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	})
}

// TestStreamPartialThenError 已有部分正文时上游断流：必须 response.failed，
// 且 output 里保留已生成的部分 item（status=incomplete）。
func TestStreamPartialThenError(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := NewTranslator(rec, Meta{Model: "m"})
	if err := tr.WriteFrame([]byte(`data: {"choices":[{"index":0,"delta":{"content":"半截"}}]}` + "\n\n")); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	if err := tr.Finish(errors.New("connection reset")); err != nil {
		t.Fatalf("finish: %v", err)
	}
	events := parseEvents(t, rec.Body.String())
	last := events[len(events)-1]
	if last.Name != "response.failed" {
		t.Fatalf("last=%q want response.failed", last.Name)
	}
	// P0-2 回归：message item 只能有一个 added，且不能是 function_call。
	var added []map[string]any
	for _, e := range events {
		if e.Name == "response.output_item.added" {
			item, _ := e.Data["item"].(map[string]any)
			added = append(added, item)
		}
	}
	if len(added) != 1 || added[0]["type"] != "message" {
		t.Fatalf("added items=%v want exactly one message item", added)
	}
	resp, _ := last.Data["response"].(map[string]any)
	out, _ := resp["output"].([]any)
	if len(out) != 1 {
		t.Fatalf("output len=%d want 1", len(out))
	}
	item, _ := out[0].(map[string]any)
	if item["status"] != "incomplete" {
		t.Errorf("partial item status=%v", item["status"])
	}
}

// TestStreamPlaceholderErrorIgnored "error": null/{} / "" 是上游占位字段，
// 不能据此把正常正文流判失败（三路判定必须与 Aggregate 一致）。
func TestStreamPlaceholderErrorIgnored(t *testing.T) {
	for _, placeholder := range []string{`null`, `{}`, `""`} {
		t.Run(placeholder, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tr := NewTranslator(rec, Meta{Model: "m"})
			frame := `data: {"error":` + placeholder + `,"choices":[{"index":0,"delta":{"content":"hello"}}]}` + "\n\n"
			if err := tr.WriteFrame([]byte(frame)); err != nil {
				t.Fatalf("write frame: %v", err)
			}
			if err := tr.Finish(nil); err != nil {
				t.Fatalf("finish: %v", err)
			}
			events := parseEvents(t, rec.Body.String())
			if last := events[len(events)-1]; last.Name != "response.completed" {
				t.Fatalf("placeholder error must not fail stream, last=%q", last.Name)
			}
		})
	}
}

// TestStreamMetadataFrameNotBadFrames Azure 风格元数据帧（choices 空 +
// prompt_filter_results）不能被 badFrames 误判为解析失败。
func TestStreamMetadataFrameNotBadFrames(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := NewTranslator(rec, Meta{Model: "m"})
	frames := []string{
		`data: {"choices":[],"prompt_filter_results":[{"prompt_index":0}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"content_filter"}]}` + "\n\n",
	}
	for _, f := range frames {
		if err := tr.WriteFrame([]byte(f)); err != nil {
			t.Fatalf("write frame: %v", err)
		}
	}
	if err := tr.Finish(nil); err != nil {
		t.Fatalf("finish: %v", err)
	}
	events := parseEvents(t, rec.Body.String())
	last := events[len(events)-1]
	if last.Name != "response.incomplete" {
		t.Fatalf("last=%q want response.incomplete (%v)", last.Name, eventNames(events))
	}
}

// TestStreamFailBeforeStart 直接以错误开场时也要先补 response.created。
func TestStreamFailBeforeStart(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := NewTranslator(rec, Meta{Model: "m"})
	tr.fail("upstream_error", "boom")
	if err := tr.flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	events := parseEvents(t, rec.Body.String())
	wantNames(t, events, []string{
		"response.created",
		"response.in_progress",
		"error",
		"response.failed",
	})
}

// TestStreamToolCallMissingIndex 缺 index 的延续分片必须归位到最近一次调用，
// 不能新开一个 item（否则并行调用会被拆碎）。
func TestStreamToolCallMissingIndex(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := NewTranslator(rec, Meta{Model: "m"})
	frames := []string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":2,"id":"call_a","function":{"name":"shell","arguments":"{\"a\":"}}]}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"1}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n",
	}
	for _, f := range frames {
		if err := tr.WriteFrame([]byte(f)); err != nil {
			t.Fatalf("write frame: %v", err)
		}
	}
	if err := tr.Finish(nil); err != nil {
		t.Fatalf("finish: %v", err)
	}
	events := parseEvents(t, rec.Body.String())
	var added int
	for _, e := range events {
		if e.Name == "response.output_item.added" {
			added++
		}
	}
	if added != 1 {
		t.Fatalf("output_item.added=%d want 1 (缺 index 分片必须归位)", added)
	}
	last := events[len(events)-1]
	resp, _ := last.Data["response"].(map[string]any)
	out, _ := resp["output"].([]any)
	item, _ := out[0].(map[string]any)
	if item["arguments"] != `{"a":1}` {
		t.Errorf("arguments=%v", item["arguments"])
	}
}

// TestErrorFieldsNumericCode 上游把 code 发成数字时也要原样透出。
func TestErrorFieldsNumericCode(t *testing.T) {
	code, message := errorFields(map[string]any{"code": float64(6004), "message": "rate limited"})
	if code != "6004" || message != "rate limited" {
		t.Fatalf("code=%q message=%q", code, message)
	}
}

// TestStreamCustomToolNameArrivesLate name 晚到（首片只有 id/arguments）时，
// 仍必须按最终 name 判定为 custom_tool_call，不能降级成 function_call。
func TestStreamCustomToolNameArrivesLate(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := NewTranslator(rec, Meta{Model: "m", CustomTools: map[string]bool{"apply_patch": true}})
	frames := []string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_p","function":{"arguments":"{\"input\":\"pat"}}]}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"apply_patch","arguments":"ch\"}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n",
	}
	for _, f := range frames {
		if err := tr.WriteFrame([]byte(f)); err != nil {
			t.Fatalf("write frame: %v", err)
		}
	}
	if err := tr.Finish(nil); err != nil {
		t.Fatalf("finish: %v", err)
	}
	events := parseEvents(t, rec.Body.String())
	var added map[string]any
	for _, e := range events {
		if e.Name == "response.output_item.added" {
			added, _ = e.Data["item"].(map[string]any)
		}
	}
	if added == nil || added["type"] != "custom_tool_call" || added["name"] != "apply_patch" {
		t.Fatalf("late-name item=%v want custom_tool_call/apply_patch", added)
	}
	last := events[len(events)-1]
	resp, _ := last.Data["response"].(map[string]any)
	out, _ := resp["output"].([]any)
	item, _ := out[0].(map[string]any)
	if item["input"] != "patch" {
		t.Errorf("input=%q", item["input"])
	}
}

// TestStreamZeroArgToolCall 无参工具的空 arguments 是合法形态，不能误判为畸形。
func TestStreamZeroArgToolCall(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := NewTranslator(rec, Meta{Model: "m"})
	frame := `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_z","function":{"name":"ping","arguments":""}}]},"finish_reason":"tool_calls"}]}` + "\n\n"
	if err := tr.WriteFrame([]byte(frame)); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	if err := tr.Finish(nil); err != nil {
		t.Fatalf("finish: %v", err)
	}
	events := parseEvents(t, rec.Body.String())
	last := events[len(events)-1]
	if last.Name != "response.completed" {
		t.Fatalf("zero-arg tool call should complete, got %q", last.Name)
	}
	resp, _ := last.Data["response"].(map[string]any)
	out, _ := resp["output"].([]any)
	item, _ := out[0].(map[string]any)
	if item["status"] != "completed" {
		t.Fatalf("item status=%v want completed", item["status"])
	}
}

// TestStreamManyChunks 长流不丢帧、序号连续、最终文本完整（回归分帧/缓冲）。
func TestStreamManyChunks(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := NewTranslator(rec, Meta{Model: "m"})
	const n = 5000
	for i := 0; i < n; i++ {
		frame := `data: {"choices":[{"index":0,"delta":{"content":"a"}}]}` + "\n\n"
		if err := tr.WriteFrame([]byte(frame)); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
	}
	if err := tr.Finish(nil); err != nil {
		t.Fatalf("finish: %v", err)
	}
	events := parseEvents(t, rec.Body.String())
	last := events[len(events)-1]
	if last.Name != "response.completed" {
		t.Fatalf("last=%q", last.Name)
	}
	resp, _ := last.Data["response"].(map[string]any)
	out, _ := resp["output"].([]any)
	item, _ := out[0].(map[string]any)
	content, _ := item["content"].([]any)
	part, _ := content[0].(map[string]any)
	text, _ := part["text"].(string)
	if len(text) != n {
		t.Fatalf("text len=%d want %d", len(text), n)
	}
	// 事件数 = 4 个开场帧 + n 个 delta + 4 个收尾帧。
	if len(events) != n+8 {
		t.Fatalf("events=%d want %d", len(events), n+8)
	}
}

// TestStreamLargeSingleFrame 单帧 >1MB（长工具参数）不能被缓冲上限误伤。
func TestStreamLargeSingleFrame(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := NewTranslator(rec, Meta{Model: "m"})
	big := strings.Repeat("x", 1<<20)
	frame := `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"f","arguments":"` + big + `"}}]},"finish_reason":"tool_calls"}]}` + "\n\n"
	if err := tr.WriteFrame([]byte(frame)); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	if err := tr.Finish(nil); err != nil {
		t.Fatalf("finish: %v", err)
	}
	events := parseEvents(t, rec.Body.String())
	last := events[len(events)-1]
	resp, _ := last.Data["response"].(map[string]any)
	out, _ := resp["output"].([]any)
	item, _ := out[0].(map[string]any)
	if len(item["arguments"].(string)) != 1<<20 {
		t.Fatalf("large arguments truncated: %d", len(item["arguments"].(string)))
	}
}

// TestStreamNonSSEPayloadFails 上游 200 但 body 是非 SSE（代理错误页/裸 JSON）：
// 必须在第一个完整块就判 upstream_parse，不能退化成空 completed。
func TestStreamNonSSEPayloadFails(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := NewTranslator(rec, Meta{Model: "m"})
	if err := tr.WriteFrame([]byte("<html>502 Bad Gateway</html>\n\n")); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	if err := tr.Finish(nil); err != nil {
		t.Fatalf("finish: %v", err)
	}
	events := parseEvents(t, rec.Body.String())
	last := events[len(events)-1]
	if last.Name != "response.failed" {
		t.Fatalf("last=%q want response.failed (%v)", last.Name, eventNames(events))
	}
}

// TestStreamErrorMsgField error.msg（非 message）也要进入失败事件的可读消息。
func TestStreamErrorMsgField(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := NewTranslator(rec, Meta{Model: "m"})
	if err := tr.WriteFrame([]byte(`data: {"error":{"msg":"request illegal"}}` + "\n\n")); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	if err := tr.Finish(nil); err != nil {
		t.Fatalf("finish: %v", err)
	}
	events := parseEvents(t, rec.Body.String())
	var failed map[string]any
	for _, e := range events {
		if e.Name == "response.failed" {
			failed = e.Data
		}
	}
	resp, _ := failed["response"].(map[string]any)
	errObj, _ := resp["error"].(map[string]any)
	if errObj["message"] != "request illegal" {
		t.Fatalf("failed message=%v want request illegal", errObj["message"])
	}
}

// TestStreamMalformedDataFrameFails 形如 data: {"message":"bad gateway"} 的
// 非 chat SSE 帧不能产出空 completed。
func TestStreamMalformedDataFrameFails(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := NewTranslator(rec, Meta{Model: "m"})
	if err := tr.WriteFrame([]byte(`data: {"message":"bad gateway"}` + "\n\n")); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	if err := tr.Finish(nil); err != nil {
		t.Fatalf("finish: %v", err)
	}
	events := parseEvents(t, rec.Body.String())
	if last := events[len(events)-1]; last.Name != "response.failed" {
		t.Fatalf("last=%q want response.failed", last.Name)
	}
}

// TestStreamSplitFrames 半帧/粘包输入也必须正确解析。
func TestStreamSplitFrames(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := NewTranslator(rec, Meta{Model: "m"})
	frame := `data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n"
	if err := tr.WriteFrame([]byte(frame[:20])); err != nil {
		t.Fatalf("partial: %v", err)
	}
	if err := tr.WriteFrame([]byte(frame[20:])); err != nil {
		t.Fatalf("rest: %v", err)
	}
	if err := tr.Finish(nil); err != nil {
		t.Fatalf("finish: %v", err)
	}
	events := parseEvents(t, rec.Body.String())
	var deltas int
	for _, e := range events {
		if e.Name == "response.output_text.delta" {
			deltas++
		}
	}
	if deltas != 1 {
		t.Fatalf("deltas=%d want 1", deltas)
	}
}
