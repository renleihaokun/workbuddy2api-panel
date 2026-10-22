package prompt

import (
	"encoding/json"
	"strings"
	"testing"
)

// decodeMessages 解析 body 的 messages 数组（测试断言用）。
func decodeMessages(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, body)
	}
	raw, ok := obj["messages"].([]any)
	if !ok {
		t.Fatalf("messages not array: %v", obj["messages"])
	}
	out := make([]map[string]any, 0, len(raw))
	for _, m := range raw {
		mm, ok := m.(map[string]any)
		if !ok {
			t.Fatalf("message not object: %v", m)
		}
		out = append(out, mm)
	}
	return out
}

func roleOf(m map[string]any) string {
	r, _ := m["role"].(string)
	return r
}

func TestSystemTextCollectsSystemAndDeveloper(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"system","content":"SYS-A"},
		{"role":"user","content":"hi"},
		{"role":"developer","content":"DEV-B"}
	]}`)
	if got := SystemText(body); got != "SYS-A\n\nDEV-B" {
		t.Errorf("SystemText=%q want %q", got, "SYS-A\n\nDEV-B")
	}
}

// 多模态 system：只拼 text part，忽略图片。
func TestSystemTextMultimodalParts(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":[
		{"type":"text","text":"A"},
		{"type":"image_url","image_url":{"url":"data:x"}},
		{"type":"text","text":"B"}
	]}]}`)
	if got := SystemText(body); got != "AB" {
		t.Errorf("SystemText=%q want AB", got)
	}
}

// 首条 system 的正文搬进首条 user，用 <instructions> 包住；system 位置留中性提示词。
func TestSystemAsUserMovesSystemIntoFirstUser(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"system","content":"SYS"},
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"yo"}
	]}`)
	out := SystemAsUser(body, "NEUTRAL")
	if out == nil {
		t.Fatal("expected a rewrite, got nil")
	}
	msgs := decodeMessages(t, out)
	if len(msgs) != 3 {
		t.Fatalf("len=%d want 3: %s", len(msgs), out)
	}
	if roleOf(msgs[0]) != "system" || contentTextOf(msgs[0]["content"]) != "NEUTRAL" {
		t.Fatalf("first message must be neutral system: %+v", msgs[0])
	}
	want := InstructionsOpen + "SYS" + InstructionsClose + "\n\nhi"
	if roleOf(msgs[1]) != "user" || contentTextOf(msgs[1]["content"]) != want {
		t.Fatalf("system text not moved into first user: %+v want %q", msgs[1], want)
	}
	if roleOf(msgs[2]) != "assistant" || contentTextOf(msgs[2]["content"]) != "yo" {
		t.Fatalf("assistant message changed: %+v", msgs[2])
	}
}

// 多模态 user：指令块以 text part 插到最前，图片 part 原样保留。
func TestSystemAsUserMergesMultimodalUser(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"system","content":"SYS"},
		{"role":"user","content":[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"data:x"}}]}
	]}`)
	out := SystemAsUser(body, "NEUTRAL")
	msgs := decodeMessages(t, out)
	parts, ok := msgs[1]["content"].([]any)
	if !ok || len(parts) != 3 {
		t.Fatalf("multimodal content not preserved: %+v", msgs[1]["content"])
	}
	if s, _ := parts[0].(map[string]any)["text"].(string); s != InstructionsOpen+"SYS"+InstructionsClose {
		t.Errorf("first part=%q want wrapped instructions", s)
	}
	if parts[1].(map[string]any)["text"] != "看图" {
		t.Errorf("original text part changed: %+v", parts[1])
	}
	if parts[2].(map[string]any)["type"] != "image_url" {
		t.Errorf("image part changed: %+v", parts[2])
	}
}

// 后续 system 就地降级为 user（内容逐字保留），不并入首条 user。
func TestSystemAsUserConvertsLaterSystemInPlace(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"system","content":"SYS"},
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"a"},
		{"role":"system","content":"Reminder: be brief."},
		{"role":"user","content":"next"}
	]}`)
	out := SystemAsUser(body, "NEUTRAL")
	msgs := decodeMessages(t, out)
	roles := make([]string, 0, len(msgs))
	for _, m := range msgs {
		roles = append(roles, roleOf(m))
	}
	want := []string{"system", "user", "assistant", "user", "user"}
	if strings.Join(roles, ",") != strings.Join(want, ",") {
		t.Fatalf("roles=%v want %v", roles, want)
	}
	if contentTextOf(msgs[3]["content"]) != "Reminder: be brief." {
		t.Errorf("later system content changed: %+v", msgs[3])
	}
}

// 没有 user 消息（纯 assistant/tool 历史）：在 system 之后补一条 user 承载指令块。
func TestSystemAsUserWithoutUserMessage(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"system","content":"SYS"},
		{"role":"assistant","content":"a"}
	]}`)
	out := SystemAsUser(body, "NEUTRAL")
	msgs := decodeMessages(t, out)
	if len(msgs) != 3 {
		t.Fatalf("len=%d want 3: %s", len(msgs), out)
	}
	if roleOf(msgs[0]) != "system" || roleOf(msgs[1]) != "user" || roleOf(msgs[2]) != "assistant" {
		t.Fatalf("roles=%v want [system user assistant]", []string{roleOf(msgs[0]), roleOf(msgs[1]), roleOf(msgs[2])})
	}
	if contentTextOf(msgs[1]["content"]) != InstructionsOpen+"SYS"+InstructionsClose {
		t.Errorf("inserted user content=%q", contentTextOf(msgs[1]["content"]))
	}
}

// 没有 system：返回 nil（无可搬内容，调用方跳过重试）。
func TestSystemAsUserWithoutSystemReturnsNil(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	if out := SystemAsUser(body, "NEUTRAL"); out != nil {
		t.Fatalf("nothing to move should return nil, got %s", out)
	}
}

// 幂等：搬过之后唯一的 system 就是中性提示词 → 再次调用返回 nil。
func TestSystemAsUserIdempotent(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"system","content":"SYS"},
		{"role":"user","content":"hi"}
	]}`)
	once := SystemAsUser(body, "NEUTRAL")
	if once == nil {
		t.Fatal("first call should rewrite")
	}
	if twice := SystemAsUser(once, "NEUTRAL"); twice != nil {
		t.Errorf("second call should be nil (already moved), got %s", twice)
	}
}

func TestSystemAsUserInvalidJSONReturnsNil(t *testing.T) {
	if out := SystemAsUser([]byte(`{not valid json`), "NEUTRAL"); out != nil {
		t.Errorf("invalid json should return nil, got %s", out)
	}
}

func TestSystemAsUserEmptyNeutralFallsBackToDegraded(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":"SYS"},{"role":"user","content":"hi"}]}`)
	msgs := decodeMessages(t, SystemAsUser(body, ""))
	if contentTextOf(msgs[0]["content"]) != Degraded {
		t.Errorf("neutral fallback=%q want Degraded", contentTextOf(msgs[0]["content"]))
	}
}

// SystemText 必须对「同一段原文」稳定（学习键依赖它，不做归一化）。
func TestSystemTextStableForSameInput(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":"  Mixed Case  "}]}`)
	if SystemText(body) != SystemText(body) {
		t.Error("SystemText must be deterministic")
	}
	if strings.TrimSpace(SystemText(body)) != "Mixed Case" {
		// 注意：SystemText 保留原文空白，trim 仅用于本断言的对照。
		t.Errorf("SystemText=%q", SystemText(body))
	}
}
