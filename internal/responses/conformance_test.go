package responses

import (
	"net/http/httptest"
	"testing"
)

// validateEventStream 校验 Responses 事件流的通用不变量（模拟严格客户端的解析契约）：
//  1. 首帧必须是 response.created，末帧必须是 completed/incomplete/failed；
//  2. sequence_number 从 0 开始严格递增；
//  3. 任何 delta 事件引用的 item 必须已发出 output_item.added 且尚未 done；
//  4. 每个 output_item.done 的 item id 必须出现在最终 response.output 里；
//  5. 最终 output 的 item id 不得多于已 added 的 id 集合。
func validateEventStream(t *testing.T, events []sseEvent) {
	t.Helper()
	if len(events) < 2 {
		t.Fatalf("event stream too short: %d", len(events))
	}
	if events[0].Name != "response.created" {
		t.Fatalf("first event=%q want response.created", events[0].Name)
	}
	switch events[len(events)-1].Name {
	case "response.completed", "response.incomplete", "response.failed":
	default:
		t.Fatalf("last event=%q not terminal", events[len(events)-1].Name)
	}
	for i, e := range events {
		seq, ok := e.Data["sequence_number"].(float64)
		if !ok || int(seq) != i {
			t.Fatalf("event[%d] sequence_number=%v", i, e.Data["sequence_number"])
		}
	}

	added := map[string]bool{}
	done := map[string]bool{}
	for _, e := range events {
		switch e.Name {
		case "response.output_item.added":
			item, _ := e.Data["item"].(map[string]any)
			id, _ := item["id"].(string)
			if id == "" {
				t.Fatalf("output_item.added without id: %v", e.Data)
			}
			added[id] = true
		case "response.output_item.done":
			item, _ := e.Data["item"].(map[string]any)
			id, _ := item["id"].(string)
			if !added[id] {
				t.Fatalf("output_item.done for never-added id=%q", id)
			}
			done[id] = true
		case "response.output_text.delta", "response.function_call_arguments.delta",
			"response.custom_tool_call_input.delta", "response.output_text.done",
			"response.function_call_arguments.done", "response.custom_tool_call_input.done",
			"response.content_part.added", "response.content_part.done":
			id, _ := e.Data["item_id"].(string)
			if id == "" {
				t.Fatalf("%s without item_id: %v", e.Name, e.Data)
			}
			if !added[id] {
				t.Fatalf("%s references item %q before output_item.added", e.Name, id)
			}
			if done[id] {
				t.Fatalf("%s references item %q after output_item.done", e.Name, id)
			}
		}
	}

	last := events[len(events)-1]
	resp, _ := last.Data["response"].(map[string]any)
	if resp == nil {
		t.Fatalf("terminal event without response object: %v", last.Data)
	}
	output, _ := resp["output"].([]any)
	finalIDs := map[string]bool{}
	for _, raw := range output {
		item, _ := raw.(map[string]any)
		id, _ := item["id"].(string)
		finalIDs[id] = true
		if !added[id] {
			t.Fatalf("final output contains id=%q never added", id)
		}
	}
	for id := range done {
		if !finalIDs[id] {
			t.Fatalf("output_item.done id=%q missing from final output", id)
		}
	}
	// 非失败终态下，每个 added 都必须有配对的 done（失败流允许半截 item）。
	if events[len(events)-1].Name != "response.failed" {
		for id := range added {
			if !done[id] {
				t.Fatalf("item id=%q added but never done", id)
			}
		}
	}
}

// TestConformanceComplexStream 用一条「文本 + 2 个并行工具调用 + usage」的流
// 跑完整不变量校验。
func TestConformanceComplexStream(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := NewTranslator(rec, Meta{Model: "m", CustomTools: map[string]bool{}})
	frames := []string{
		`data: {"choices":[{"index":0,"delta":{"content":"先看看目录"}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"shell","arguments":"{\"command\":["}},{"index":1,"id":"call_2","function":{"name":"shell","arguments":"{\"command\":["}}]}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"]}"}},{"index":1,"function":{"arguments":"\"pwd\"]}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}` + "\n\n",
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
	validateEventStream(t, events)
	if len(events) == 0 {
		t.Fatal("no events")
	}
}

// TestConformanceIncompleteStream 截断场景同样满足不变量。
func TestConformanceIncompleteStream(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := NewTranslator(rec, Meta{Model: "m"})
	frames := []string{
		`data: {"choices":[{"index":0,"delta":{"content":"半截"}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}` + "\n\n",
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
	validateEventStream(t, events)
}
