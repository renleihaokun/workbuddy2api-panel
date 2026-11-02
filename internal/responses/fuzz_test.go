package responses

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// FuzzConvertRequest 任意请求字节不得 panic，且成功时输出必须是合法 chat JSON。
func FuzzConvertRequest(f *testing.F) {
	f.Add([]byte(`{"model":"m","input":"hi"}`))
	f.Add([]byte(`{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"x"}]}],"tools":[{"type":"function","name":"f"}]}`))
	f.Add([]byte(`{"model":"m","input":"x","tools":[{"type":"custom","name":"apply_patch"}]}`))
	f.Add([]byte(`{"model":"m","input":"x","previous_response_id":"resp_1"}`))
	f.Add([]byte(`not json`))

	f.Fuzz(func(t *testing.T, raw []byte) {
		chat, _, err := ConvertRequest(raw)
		if err != nil {
			return
		}
		var obj map[string]any
		if json.Unmarshal(chat, &obj) != nil {
			t.Fatalf("converted body is not valid JSON: %s", chat)
		}
		if _, ok := obj["messages"]; !ok {
			t.Fatalf("converted body missing messages: %s", chat)
		}
	})
}

// FuzzTranslator 对任意 SSE 字节做健壮性检查：不得 panic，且事件必须是可解析 JSON。
func FuzzTranslator(f *testing.F) {
	f.Add([]byte(`data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n"))
	f.Add([]byte("data: [DONE]\n\n"))
	f.Add([]byte(`data: {"error":{"message":"x","code":6004}}` + "\n\n"))
	f.Add([]byte(`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"f","arguments":"{"}}]}}]}` + "\n\n"))
	f.Add([]byte(""))

	f.Fuzz(func(t *testing.T, raw []byte) {
		rec := httptest.NewRecorder()
		tr := NewTranslator(rec, Meta{Model: "m", CustomTools: map[string]bool{"f": true}})
		_ = tr.WriteFrame(raw)
		_ = tr.WriteFrame([]byte("data: [DONE]\n\n"))
		_ = tr.Finish(nil)
		// 输出必须始终是成对 event/data 的可解析块（空流除外）。
		if rec.Body.Len() > 0 {
			_ = parseEvents(t, rec.Body.String())
		}
	})
}
