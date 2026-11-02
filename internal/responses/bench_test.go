package responses

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// discardWriter 供基准测试使用的最小 ResponseWriter。
type discardWriter struct {
	h http.Header
	n int64
}

func (d *discardWriter) Header() http.Header {
	if d.h == nil {
		d.h = http.Header{}
	}
	return d.h
}
func (d *discardWriter) Write(p []byte) (int, error) { d.n += int64(len(p)); return len(p), nil }
func (d *discardWriter) WriteHeader(int)             {}
func (d *discardWriter) Flush()                      {}

// benchRequest 模拟一份带 10 个工具声明的 Codex 请求。
func benchRequest() []byte {
	var tools []string
	for i := 0; i < 10; i++ {
		tools = append(tools, `{"type":"function","name":"tool_`+string(rune('a'+i))+`","description":"desc","parameters":{"type":"object","properties":{"command":{"type":"array","items":{"type":"string"}}}}}`)
	}
	return []byte(`{"model":"deepseek-v4-pro","instructions":"You are Codex.","input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"` + strings.Repeat("请分析仓库结构。", 20) + `"}]},` +
		`{"type":"function_call","call_id":"c1","name":"tool_a","arguments":"{\"command\":[\"ls\"]}"},` +
		`{"type":"function_call_output","call_id":"c1","output":"a.txt"}` +
		`],"tools":[` + strings.Join(tools, ",") + `],"tool_choice":"auto","parallel_tool_calls":false,` +
		`"reasoning":{"effort":"high"},"store":false,"stream":true}`)
}

func BenchmarkConvertRequest(b *testing.B) {
	body := benchRequest()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, _, err := ConvertRequest(body); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTranslatorChunk(b *testing.B) {
	// 只测量「一个 chunk → 一个 output_text.delta 事件」的构造成本；flush 的
	// syscall 成本与上游 chunk 频率成正比，单独由集成压测覆盖。
	w := &discardWriter{}
	tr := NewTranslator(w, Meta{Model: "deepseek-v4"})
	_ = tr.Start()
	tr.out.Reset()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tr.appendText("这是一段典型的流式正文，长度大约几十个汉字。")
		tr.out.Reset()
	}
}

// BenchmarkTranslatorWriteFrame 覆盖「解析 chat SSE 帧 → 生成事件 → 写缓冲」的
// 全路径（含 json.Unmarshal）；这是线上每个上游 chunk 的真实成本。
func BenchmarkTranslatorWriteFrame(b *testing.B) {
	chunk := []byte(`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"这是一段典型的流式正文，长度大约几十个汉字。"}}]}` + "\n\n")
	w := &discardWriter{}
	tr := NewTranslator(w, Meta{Model: "deepseek-v4"})
	_ = tr.Start()
	tr.out.Reset()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := tr.WriteFrame(chunk); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFromChatCompletion(b *testing.B) {
	var chat map[string]any
	_ = json.Unmarshal([]byte(`{"choices":[{"message":{"role":"assistant","content":"`+strings.Repeat("结果", 500)+`","tool_calls":[{"id":"call_1","function":{"name":"shell","arguments":"{\"command\":[\"ls\"]}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1000,"completion_tokens":500,"total_tokens":1500}}`), &chat)
	meta := Meta{Model: "m", CustomTools: map[string]bool{}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = FromChatCompletion(chat, meta)
	}
}
