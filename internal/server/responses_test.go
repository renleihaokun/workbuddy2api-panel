package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/responses"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// sseToolCall 上游返回一次工具调用的 SSE（参数分两片 + finish + DONE）。
const sseToolCall = `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_9","type":"function","function":{"name":"shell","arguments":"{\"command\":"}}]}}]}` + "\n\n" +
	`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"[\"ls\"]}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n" +
	"data: [DONE]\n\n"

// doResponses 发起一次 /v1/responses 请求。
func doResponses(t *testing.T, h *Handler, apiKey, body string) *httptest.ResponseRecorder {
	t.Helper()
	// 预置模型目录缓存：本文件大量请求会触发 /v1/models 动态拉取，若不预置，
	// shuffle 顺序下会污染 TestModelsEndpoint 的全局缓存断言。
	presetDynamicModels(t, []upstream.ModelInfo{{ID: "glm-5.2"}, {ID: "deepseek-v3"}})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// responsesEvents 解析 /v1/responses 的 SSE 输出为事件名列表。
func responsesEvents(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, block := range strings.Split(strings.TrimSpace(raw), "\n\n") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		var data string
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "data: ") {
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if data == "" || data == "[DONE]" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatalf("invalid event JSON: %v (%s)", err, data)
		}
		out = append(out, payload)
	}
	return out
}

// TestResponsesNonStream 非流式：chat 聚合结果被翻译成 Response 对象。
func TestResponsesNonStream(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := doResponses(t, h, "", `{"model":"glm-5.2","input":"hi","stream":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v (%s)", err, rec.Body)
	}
	if resp["object"] != "response" {
		t.Errorf("object=%v", resp["object"])
	}
	if resp["status"] != "completed" {
		t.Errorf("status=%v", resp["status"])
	}
	output, _ := resp["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output len=%d: %v", len(output), output)
	}
	item, _ := output[0].(map[string]any)
	content, _ := item["content"].([]any)
	part, _ := content[0].(map[string]any)
	if part["text"] != "你好" {
		t.Errorf("text=%v", part["text"])
	}
	usage, _ := resp["usage"].(map[string]any)
	if v, _ := usage["input_tokens"].(float64); v != 1 {
		t.Errorf("input_tokens=%v", usage["input_tokens"])
	}
	if v, _ := usage["output_tokens"].(float64); v != 1 {
		t.Errorf("output_tokens=%v", usage["output_tokens"])
	}
}

// TestResponsesStream 流式：输出必须是 Responses 事件流，且以 completed 收尾。
func TestResponsesStream(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := doResponses(t, h, "", `{"model":"glm-5.2","input":"hi","stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type=%q", ct)
	}
	events := responsesEvents(t, rec.Body.String())
	if len(events) < 6 {
		t.Fatalf("too few events: %v", events)
	}
	if events[0]["type"] != "response.created" {
		t.Errorf("first event=%v", events[0]["type"])
	}
	last := events[len(events)-1]
	if last["type"] != "response.completed" {
		t.Fatalf("last event=%v want response.completed", last["type"])
	}
	resp, _ := last["response"].(map[string]any)
	if resp["status"] != "completed" {
		t.Errorf("status=%v", resp["status"])
	}
	out, _ := resp["output"].([]any)
	item, _ := out[0].(map[string]any)
	content, _ := item["content"].([]any)
	part, _ := content[0].(map[string]any)
	if part["text"] != "你好" {
		t.Errorf("text=%v", part["text"])
	}
	// 不得出现 chat 的 [DONE] 帧。
	if strings.Contains(rec.Body.String(), "[DONE]") {
		t.Errorf("responses stream must not contain [DONE]")
	}
}

// TestResponsesStreamToolCall 端到端验证工具调用链路：上游 chat SSE 的
// delta.tool_calls 经 StreamHint 规范化 + 垫片翻译，最终产出 function_call item。
func TestResponsesStreamToolCall(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseToolCall, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := doResponses(t, h, "", `{"model":"glm-5.2","input":"list","stream":true,"tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	events := responsesEvents(t, rec.Body.String())
	last := events[len(events)-1]
	if last["type"] != "response.completed" {
		t.Fatalf("last=%v", last["type"])
	}
	resp, _ := last["response"].(map[string]any)
	output, _ := resp["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output len=%d", len(output))
	}
	item, _ := output[0].(map[string]any)
	if item["type"] != "function_call" || item["call_id"] != "call_9" || item["name"] != "shell" {
		t.Fatalf("item=%v", item)
	}
	if item["arguments"] != `{"command":["ls"]}` {
		t.Errorf("arguments=%v", item["arguments"])
	}
}

// TestResponsesNonStreamToolCall 非流式工具调用：聚合后的 chat JSON 被翻译成
// function_call item，arguments 与 call_id 保持完整。
func TestResponsesNonStreamToolCall(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseToolCall, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := doResponses(t, h, "", `{"model":"glm-5.2","input":"list","stream":false,"tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v (%s)", err, rec.Body)
	}
	output, _ := resp["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output len=%d want 1", len(output))
	}
	item, _ := output[0].(map[string]any)
	if item["type"] != "function_call" || item["call_id"] != "call_9" || item["name"] != "shell" {
		t.Fatalf("item=%v", item)
	}
	if item["arguments"] != `{"command":["ls"]}` {
		t.Errorf("arguments=%v", item["arguments"])
	}
}

// TestResponsesStreamOverHTTP 走真实 net/http 连接验证 SSE 头与逐帧写出，
// 覆盖 httptest.ResponseRecorder 测不到的 WriteHeader/Flush 行为。
func TestResponsesStreamOverHTTP(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/responses", strings.NewReader(`{"model":"glm-5.2","input":"hi","stream":true}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type=%q", ct)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	events := responsesEvents(t, string(raw))
	if len(events) == 0 || events[len(events)-1]["type"] != "response.completed" {
		t.Fatalf("bad event stream: %v", events)
	}
}

// TestResponsesEmptyUpstreamStream 上游 200 + 空流：必须翻译成 failed，
// 不能给客户端一个「成功但没有内容」的回合。
func TestResponsesEmptyUpstreamStream(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, "", true })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := doResponses(t, h, "", `{"model":"glm-5.2","input":"hi","stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	events := responsesEvents(t, rec.Body.String())
	var sawFailed bool
	for _, e := range events {
		if e["type"] == "response.failed" {
			sawFailed = true
		}
		if e["type"] == "response.completed" {
			t.Fatalf("empty upstream stream must not report completed: %v", events)
		}
	}
	if !sawFailed {
		t.Fatalf("response.failed missing: %v", events)
	}
}

// failingWriter 模拟客户端断连：任何 Write 都失败。
type failingWriter struct{ h http.Header }

func (f *failingWriter) Header() http.Header {
	if f.h == nil {
		f.h = http.Header{}
	}
	return f.h
}
func (f *failingWriter) Write([]byte) (int, error) { return 0, errors.New("client gone") }
func (f *failingWriter) WriteHeader(int)           {}

// TestResponsesWriterPropagatesClientDisconnect 客户端断连必须把错误回灌给
// StreamHint（否则上游继续跑、账号还会被记成功）。
func TestResponsesWriterPropagatesClientDisconnect(t *testing.T) {
	rw := &responsesWriter{ResponseWriter: &failingWriter{}, meta: responses.Meta{Model: "m"}}
	rw.Header().Set("Content-Type", "text/event-stream")
	if _, err := rw.Write([]byte(`data: {"choices":[{"index":0,"delta":{"content":"x"}}]}` + "\n\n")); err == nil {
		t.Fatal("write error must propagate to StreamHint")
	}
}

// TestResponsesStreamWithoutDoneFails 没有收尾 [DONE] 的流（上游读错误）必须
// 翻译成 response.failed，不能伪装成 completed。
func TestResponsesStreamWithoutDoneFails(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &responsesWriter{ResponseWriter: rec, meta: responses.Meta{Model: "m"}}
	rw.Header().Set("Content-Type", "text/event-stream")
	if _, err := rw.Write([]byte(`data: {"choices":[{"index":0,"delta":{"content":"半截"}}]}` + "\n\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := rw.finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}
	events := responsesEvents(t, rec.Body.String())
	last := events[len(events)-1]
	if last["type"] != "response.failed" {
		t.Fatalf("last event=%v want response.failed", last["type"])
	}
}

// TestResponsesCustomToolEndToEnd custom/freeform 工具走完整链路：
// ConvertRequest 记录 CustomTools → 流式响应还原成 custom_tool_call。
func TestResponsesCustomToolEndToEnd(t *testing.T) {
	sse := `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_p","function":{"name":"apply_patch","arguments":"{\"input\":\"*** Begin Patch\\n*** End Patch\"}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sse, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	body := `{"model":"glm-5.2","input":"patch it","stream":true,"tools":[{"type":"custom","name":"apply_patch","description":"patch"}]}`
	rec := doResponses(t, h, "", body)
	events := responsesEvents(t, rec.Body.String())
	last := events[len(events)-1]
	resp, _ := last["response"].(map[string]any)
	out, _ := resp["output"].([]any)
	if len(out) != 1 {
		t.Fatalf("output=%v", out)
	}
	item, _ := out[0].(map[string]any)
	if item["type"] != "custom_tool_call" {
		t.Fatalf("item type=%v want custom_tool_call", item["type"])
	}
	if item["input"] != "*** Begin Patch\n*** End Patch" {
		t.Errorf("input=%q", item["input"])
	}
}

// TestResponsesEchoFields HTTP 级断言 tool_choice/truncation/service_tier 回显。
func TestResponsesEchoFields(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	body := `{"model":"glm-5.2","input":"hi","stream":false,
		"tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}],
		"tool_choice":{"type":"function","name":"shell"},"truncation":"auto","service_tier":"flex"}`
	rec := doResponses(t, h, "", body)
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v (%s)", err, rec.Body)
	}
	tc, _ := resp["tool_choice"].(map[string]any)
	if tc == nil || tc["name"] != "shell" {
		t.Errorf("tool_choice=%v", resp["tool_choice"])
	}
	if resp["truncation"] != "auto" || resp["service_tier"] != "flex" {
		t.Errorf("truncation=%v service_tier=%v", resp["truncation"], resp["service_tier"])
	}
}

// errAfterReader 首次返回一帧 SSE，第二次 Read 返回传输错误（模拟上游中途断流）。
type errAfterReader struct {
	frame []byte
	read  bool
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if !r.read {
		r.read = true
		return copy(p, r.frame), nil
	}
	return 0, errors.New("connection reset by peer")
}
func (r *errAfterReader) Close() error { return nil }

// TestResponsesMidStreamReadErrorFails 上游读错误（StreamHint 提前返回、不写
// [DONE]）必须翻译成 response.failed，不能是 response.completed。
func TestResponsesMidStreamReadErrorFails(t *testing.T) {
	frame := `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"半截"}}]}` + "\n\n"
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       &errAfterReader{frame: []byte(frame)},
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := doResponses(t, h, "", `{"model":"glm-5.2","input":"hi","stream":true}`)
	events := responsesEvents(t, rec.Body.String())
	last := events[len(events)-1]
	if last["type"] == "response.completed" {
		t.Fatalf("mid-stream read error must not be completed: %v", events)
	}
	if last["type"] != "response.failed" {
		t.Fatalf("last event=%v want response.failed", last["type"])
	}
}

// TestResponsesMarkupRepairToFunctionCall 上游把工具调用吐成原生标记文本时，
// StreamHint 的修复层应还原成 delta.tool_calls，响应侧再翻译成 function_call
// item（Codex 只执行结构化条目，这段修复是网关的核心价值之一）。
func TestResponsesMarkupRepairToFunctionCall(t *testing.T) {
	pipe := "\uFF5C\uFF5C"
	markup := "<" + pipe + "DSML" + pipe + " calls>\n" +
		"<" + pipe + "DSML" + pipe + " invoke name=\"shell\">\n" +
		"<" + pipe + "DSML" + pipe + " parameter name=\"command\" string=\"true\">[\"ls\"]</" + pipe + "DSML" + pipe + " parameter>\n" +
		"</" + pipe + "DSML" + pipe + " invoke>\n" +
		"</" + pipe + "DSML" + pipe + " calls>"
	chunk1, _ := json.Marshal(map[string]any{
		"id": "c1", "object": "chat.completion.chunk",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": markup}}},
	})
	chunk2, _ := json.Marshal(map[string]any{
		"id": "c1", "object": "chat.completion.chunk",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
	})
	sse := "data: " + string(chunk1) + "\n\n" + "data: " + string(chunk2) + "\n\n" + "data: [DONE]\n\n"
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sse, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	body := `{"model":"glm-5.2","input":"list","stream":true,"tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}]}`
	rec := doResponses(t, h, "", body)
	events := responsesEvents(t, rec.Body.String())
	last := events[len(events)-1]
	if last["type"] != "response.completed" {
		t.Fatalf("last=%v", last["type"])
	}
	resp, _ := last["response"].(map[string]any)
	out, _ := resp["output"].([]any)
	if len(out) != 1 {
		t.Fatalf("output=%v want one function_call", out)
	}
	item, _ := out[0].(map[string]any)
	if item["type"] != "function_call" || item["name"] != "shell" {
		t.Fatalf("item=%v", item)
	}
	if !strings.Contains(item["arguments"].(string), "ls") {
		t.Errorf("arguments=%v", item["arguments"])
	}
}

// errorReader 首次 Read 即返回错误（上游 200 响应头后、首帧前断流）。
type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }
func (errorReader) Close() error             { return nil }

// TestResponsesImmediateReadErrorFails 上游 200 后首帧前断流：不能返回
// 「200 + 空 body」，必须补一条完整的 response.failed 流。
func TestResponsesImmediateReadErrorFails(t *testing.T) {
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       errorReader{},
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := doResponses(t, h, "", `{"model":"glm-5.2","input":"hi","stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 (SSE 已开流)", rec.Code)
	}
	if rec.Body.Len() == 0 {
		t.Fatalf("empty body on immediate read error (status=%d)", rec.Code)
	}
	events := responsesEvents(t, rec.Body.String())
	if len(events) < 2 {
		t.Fatalf("too few events: %v", events)
	}
	if events[0]["type"] != "response.created" {
		t.Fatalf("first event=%v want response.created", events[0]["type"])
	}
	var errorBeforeFailed bool
	for _, e := range events {
		if e["type"] == "error" {
			errorBeforeFailed = true
			continue
		}
		if e["type"] == "response.failed" {
			if !errorBeforeFailed {
				t.Fatalf("response.failed before error event: %v", events)
			}
			return
		}
	}
	t.Fatalf("response.failed missing: %v", events)
}

// TestResponsesNonSSEUpstreamFails 上游 200 但 body 是 HTML/裸 JSON（代理错误页）：
// 必须 response.failed，不能 completed。
func TestResponsesNonSSEUpstreamFails(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, "<html>502 Bad Gateway</html>", true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := doResponses(t, h, "", `{"model":"glm-5.2","input":"hi","stream":true}`)
	events := responsesEvents(t, rec.Body.String())
	last := events[len(events)-1]
	if last["type"] != "response.failed" {
		t.Fatalf("last=%v want response.failed (%v)", last["type"], events)
	}
}

// TestResponsesNonStreamUpstreamErrorFrame 上游 200 + error 帧（非流式聚合）：
// 必须回错误，不能翻译成 status=completed 的空响应。
func TestResponsesNonStreamUpstreamErrorFrame(t *testing.T) {
	raw := `data: {"id":"c1","choices":[{"index":0,"delta":{}}],"error":{"code":6004,"message":"rate limited"}}` + "\n\n" + "data: [DONE]\n\n"
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, raw, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := doResponses(t, h, "", `{"model":"glm-5.2","input":"hi","stream":false}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d body=%s (must not fake 200 completed)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "rate limited") {
		t.Errorf("upstream message lost: %s", rec.Body)
	}
	var payload map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &payload)
	if _, hasStatus := payload["status"]; hasStatus {
		t.Fatalf("response object leaked on error: %v", payload)
	}
}

// TestChatNonStreamUpstreamErrorFrame chat 非流式同样受益于 Aggregate 的
// error 帧识别：200 + error 不能变成空成功，必须映射成可重试错误。
func TestChatNonStreamUpstreamErrorFrame(t *testing.T) {
	presetDynamicModels(t, []upstream.ModelInfo{{ID: "glm-5.2"}, {ID: "deepseek-v3"}})
	raw := `data: {"id":"c1","choices":[{"index":0,"delta":{}}],"error":{"code":6004,"message":"rate limited"}}` + "\n\n" + "data: [DONE]\n\n"
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, raw, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "rate limited") {
		t.Errorf("message=%s", rec.Body)
	}
}

// TestResponsesHardCreditMapping 200 + 余额不足错误帧：与既有 HTTP 402 路径
// 保持同一契约——全池耗尽统一 503，但上游 message/hint 必须保留。
func TestResponsesHardCreditMapping(t *testing.T) {
	raw := `data: {"id":"c1","choices":[{"index":0,"delta":{}}],"error":{"code":14018,"message":"credits exhausted"}}` + "\n\n" + "data: [DONE]\n\n"
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, raw, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := doResponses(t, h, "", `{"model":"glm-5.2","input":"hi","stream":false}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s want 503 (既有 no_healthy_account 契约)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "credits exhausted") {
		t.Errorf("upstream message lost: %s", rec.Body)
	}
}

// TestResponsesNonStreamErrorFrameRotates 200+error 帧属于可重试类时，应换号
// 重试而不是直接失败（两个账号 → 两次上游调用，最终 429）。
func TestResponsesNonStreamErrorFrameRotates(t *testing.T) {
	rawErr := `data: {"id":"c1","choices":[{"index":0,"delta":{}}],"error":{"code":6004,"message":"rate limited"}}` + "\n\n" + "data: [DONE]\n\n"
	calls := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, rawErr, true
	})
	h := NewHandler(Config{
		Pool: testPoolWith(
			&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
			&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
		),
		Upstream: up,
	})
	rec := doResponses(t, h, "", `{"model":"glm-5.2","input":"hi","stream":false}`)
	if calls != 2 {
		t.Fatalf("upstream calls=%d want 2 (可重试错误应换号)", calls)
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

// TestResponsesProxyErrorPageFails 上游 200 + data: {"message":"bad gateway"}
// 这类非 chat JSON：StreamHint 不得 normalize 成空 chunk，Responses 必须 failed。
func TestResponsesProxyErrorPageFails(t *testing.T) {
	raw := `data: {"message":"bad gateway"}` + "\n\n" + "data: [DONE]\n\n"
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, raw, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := doResponses(t, h, "", `{"model":"glm-5.2","input":"hi","stream":true}`)
	events := responsesEvents(t, rec.Body.String())
	last := events[len(events)-1]
	if last["type"] != "response.failed" {
		t.Fatalf("last=%v want response.failed (%v)", last["type"], events)
	}
}

// TestResponsesRawHintPreserved 200+error 帧的 Raw 必须进入 GatewayHint：
// 11133 这类 code 只在原始 payload 里，不能被 error.message 覆盖丢失。
func TestResponsesRawHintPreserved(t *testing.T) {
	raw := `data: {"error":{"code":11133,"message":"model does not support image input"}}` + "\n\n" + "data: [DONE]\n\n"
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, raw, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	body := `{"model":"glm-5.2","stream":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"看图"},{"type":"input_image","image_url":"http://example.com/x.png"}]}]}`
	rec := doResponses(t, h, "", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "gateway_hint") {
		t.Fatalf("raw payload hint lost: %s", rec.Body)
	}
}

// TestResponsesTopLevelErrorEnvelopeFails 顶层 {code,msg} 业务信封（不带 error
// 包装）在 Responses 流式里也必须翻译成 response.failed。
func TestResponsesTopLevelErrorEnvelopeFails(t *testing.T) {
	raw := `data: {"code":6004,"msg":"rate limited"}` + "\n\n" + "data: [DONE]\n\n"
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, raw, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := doResponses(t, h, "", `{"model":"glm-5.2","input":"hi","stream":true}`)
	events := responsesEvents(t, rec.Body.String())
	last := events[len(events)-1]
	if last["type"] != "response.failed" {
		t.Fatalf("last=%v want response.failed (%v)", last["type"], events)
	}
}

// TestResponsesNestedErrorEnvelopeFails 嵌套 error.data 信封（真实上游形态）
// 在 Responses 流式里必须 failed，不能空 completed。
func TestResponsesNestedErrorEnvelopeFails(t *testing.T) {
	raw := `data: {"error":{"data":{"code":11140,"msg":"request illegal"}}}` + "\n\n" + "data: [DONE]\n\n"
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, raw, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := doResponses(t, h, "", `{"model":"glm-5.2","input":"hi","stream":true}`)
	events := responsesEvents(t, rec.Body.String())
	last := events[len(events)-1]
	if last["type"] != "response.failed" {
		t.Fatalf("last=%v want response.failed (%v)", last["type"], events)
	}
}

// TestResponsesModelRateLimit11140 11140 的模型限流变体不能误判成账号故障：
// 200+error 帧应回 429 rate_limit_exceeded。
func TestResponsesModelRateLimit11140(t *testing.T) {
	raw := `data: {"error":{"code":11140,"message":"The model provider is rate-limiting requests. Please wait."}}` + "\n\n" + "data: [DONE]\n\n"
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, raw, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := doResponses(t, h, "", `{"model":"glm-5.2","input":"hi","stream":false}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d body=%s want 429", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "account-level fault") {
		t.Errorf("misclassified as account fault: %s", rec.Body)
	}
}

// TestResponsesMixedToolCalls 同一个响应里同时有 function 与 custom 工具调用时，
// 按 meta.CustomTools 分别翻译成 function_call / custom_tool_call，顺序不串。
func TestResponsesMixedToolCalls(t *testing.T) {
	raw := `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_s","type":"function","function":{"name":"shell","arguments":"{\"command\":[\"ls\"]}"}},{"index":1,"id":"call_p","type":"function","function":{"name":"apply_patch","arguments":"{\"input\":\"*** Begin Patch\"}"}}]}}]}` + "\n\n" +
		`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, raw, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	body := `{"model":"glm-5.2","input":"patch it","stream":true,"tools":[
		{"type":"function","name":"shell","parameters":{"type":"object"}},
		{"type":"custom","name":"apply_patch","format":{"type":"grammar","syntax":"lark","definition":"start: patch"}}]}`
	rec := doResponses(t, h, "", body)
	events := responsesEvents(t, rec.Body.String())
	var out []any
	for _, e := range events {
		if e["type"] == "response.completed" {
			resp, _ := e["response"].(map[string]any)
			out, _ = resp["output"].([]any)
		}
	}
	if len(out) != 2 {
		t.Fatalf("output=%v want 2 items", out)
	}
	if item, _ := out[0].(map[string]any); item["type"] != "function_call" || item["name"] != "shell" {
		t.Fatalf("first=%v want function_call shell", out[0])
	}
	item, _ := out[1].(map[string]any)
	if item["type"] != "custom_tool_call" || item["name"] != "apply_patch" {
		t.Fatalf("second=%v want custom_tool_call apply_patch", out[1])
	}
	if !strings.Contains(item["input"].(string), "Begin Patch") {
		t.Errorf("custom input=%v", item["input"])
	}
}

// TestResponsesAuth 未带 key 时按既有鉴权回 401。
func TestResponsesAuth(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
		APIKey:   "secret",
	})
	rec := doResponses(t, h, "", `{"model":"glm-5.2","input":"hi"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

// TestResponsesPreviousResponseID 明确拒绝有状态请求。
func TestResponsesPreviousResponseID(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
	})
	rec := doResponses(t, h, "", `{"model":"glm-5.2","input":"hi","previous_response_id":"resp_1"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var payload map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &payload)
	errObj, _ := payload["error"].(map[string]any)
	if errObj["code"] != "unsupported_parameter" {
		t.Errorf("error=%v", errObj)
	}
}

// TestResponsesUpstreamError 上游 429 透传为 429 + 上游原文。
func TestResponsesUpstreamError(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 429, `{"error":{"message":"rate limited","code":"6004"}}`, false
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := doResponses(t, h, "", `{"model":"glm-5.2","input":"hi","stream":true}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "rate limited") {
		t.Errorf("upstream message lost: %s", rec.Body)
	}
	var payload map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &payload)
	errObj, _ := payload["error"].(map[string]any)
	if errObj["type"] != "rate_limit_error" {
		t.Errorf("error.type=%v want rate_limit_error（chat 的 api_error 词表应被改写）", errObj["type"])
	}
}

// TestResponsesWriterNonStreamTranslateFallback 非流式翻译失败时也必须给出
// 可解析的错误体，而不是 200 空 body。
func TestResponsesWriterNonStreamTranslateFallback(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &responsesWriter{ResponseWriter: rec, meta: responses.Meta{Model: "m"}, status: http.StatusOK, headerWritten: true}
	_, _ = rw.buf.WriteString("not-a-json")
	if err := rw.finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("fallback body is not JSON: %v (%s)", err, rec.Body)
	}
	errObj, _ := payload["error"].(map[string]any)
	if errObj["code"] != "upstream_parse" {
		t.Errorf("fallback error=%v", payload)
	}
}

// TestResponsesModelHidden 隐藏模型直接 400（既有策略在 Responses 入口同样生效）。
func TestResponsesModelHidden(t *testing.T) {
	h := NewHandler(Config{
		Pool:         testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:     newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
		ModelsHidden: []string{"glm-5.2"},
	})
	rec := doResponses(t, h, "", `{"model":"glm-5.2","input":"hi"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "model_unavailable") {
		t.Errorf("body=%s", rec.Body)
	}
}
