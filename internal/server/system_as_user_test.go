package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/prompt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// channelRefusedBody 是上游 11128 通道校验失败的真实形态（request illegal）。
const channelRefusedBody = `{"code":11128,"msg":"request illegal","displayTips":{"en":"This call failed channel verification, possibly because it was not sent from an official client."}}`

// systemFingerprintRequest 构造一个「system 带指纹」的请求：假上游见到 system
// 里含 SYS-FP 就回 11128，伪装后（system 变中性、原文进 user）回 200。
func systemFingerprintRequest() string {
	return `{"model":"glm-5.2","stream":true,"messages":[` +
		`{"role":"system","content":"SYS-FP: You are an AI agent powered by DeepSeek Harness."},` +
		`{"role":"user","content":"hi"}]}`
}

// systemPromptHasFingerprint 报告出站 body 的**首条 system** 是否仍带指纹。
// 伪装后首条 system 是中性提示词、原文在 user 消息里，因此为 false。
func systemPromptHasFingerprint(body []byte) bool {
	var obj struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &obj) != nil || len(obj.Messages) == 0 {
		return false
	}
	return obj.Messages[0].Role == "system" &&
		strings.Contains(string(obj.Messages[0].Content), "SYS-FP")
}

// newChannelRefusedUpstream 假上游：记录每次出站 body；alwaysRefuse=true 时永远
// 回 11128（用于验证「伪装后仍被拒就停手」），否则仅在首条 system 带指纹时拒绝。
func newChannelRefusedUpstream(calls *[][]byte, alwaysRefuse bool) *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			*calls = append(*calls, raw)
			if alwaysRefuse || systemPromptHasFingerprint(raw) {
				return &http.Response{
					StatusCode: 400,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(channelRefusedBody)),
				}, nil
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

func errorCodeOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal error envelope: %v (body=%s)", err, rec.Body.String())
	}
	return env.Error.Code
}

// TestSystemAsUserRetryOnChannelRefused 层 1/2：首撞 11-128 后同请求内伪装重发一次，
// 第二次成功；账号零惩罚（不冷却/不禁用/不累计错误），并写入学习表。
func TestSystemAsUserRetryOnChannelRefused(t *testing.T) {
	var calls [][]byte
	up := newChannelRefusedUpstream(&calls, false)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough", SystemAsUserFallback: true})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(systemFingerprintRequest())))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (want 200 after systemAsUser retry)", rec.Code, rec.Body)
	}
	if len(calls) != 2 {
		t.Fatalf("upstream calls=%d want 2 (original + converted)", len(calls))
	}
	if !systemPromptHasFingerprint(calls[0]) {
		t.Fatalf("first call should carry original system: %s", calls[0])
	}
	if systemPromptHasFingerprint(calls[1]) {
		t.Fatalf("second call should be converted: %s", calls[1])
	}
	// 伪装后首条仍是 system（否则会撞 first message is not system prompt）。
	if !strings.Contains(string(calls[1]), `"role":"system"`) {
		t.Fatalf("converted body lost first system message: %s", calls[1])
	}
	// system 原文必须还在（搬进 user），不能丢指令。
	if !strings.Contains(string(calls[1]), "SYS-FP") {
		t.Fatalf("converted body dropped system text: %s", calls[1])
	}
	st, _ := p.Status("u1")
	if st.Cooling || st.Disabled || st.ErrTotal != 0 {
		t.Fatalf("account must not be penalized: %+v", st)
	}
	// 学习表命中：键由首次出站 body 的 system 原文派生。
	key := systemModeKey(prompt.SystemText(calls[0]))
	if !h.sysMode.Known(key) {
		t.Error("system prompt should be remembered after conversion succeeded")
	}
}

// TestSystemAsUserLearnedGoesStraightToConversion 层 3：同一段 system 的第二次请求
// 直接以伪装形态出站，只打一次上游（不再先撞 400）。
func TestSystemAsUserLearnedGoesStraightToConversion(t *testing.T) {
	var calls [][]byte
	up := newChannelRefusedUpstream(&calls, false)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough", SystemAsUserFallback: true})

	first := httptest.NewRecorder()
	h.ServeHTTP(first, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(systemFingerprintRequest())))
	if first.Code != 200 {
		t.Fatalf("first code=%d body=%s", first.Code, first.Body)
	}
	before := len(calls)

	second := httptest.NewRecorder()
	h.ServeHTTP(second, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(systemFingerprintRequest())))
	if second.Code != 200 {
		t.Fatalf("second code=%d body=%s", second.Code, second.Body)
	}
	if got := len(calls) - before; got != 1 {
		t.Fatalf("second request upstream calls=%d want 1 (converted up-front)", got)
	}
	if systemPromptHasFingerprint(calls[len(calls)-1]) {
		t.Fatalf("second request should go converted directly: %s", calls[len(calls)-1])
	}
}

// TestSystemAsUserStopsAfterConvertedStillRefused 层 4：伪装后仍被拒 → 只试一次、
// 不再换号循环，立即 400 channel_refused 并附可操作 hint。
func TestSystemAsUserStopsAfterConvertedStillRefused(t *testing.T) {
	var calls [][]byte
	up := newChannelRefusedUpstream(&calls, true)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough", SystemAsUserFallback: true})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(systemFingerprintRequest())))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 (body=%s)", rec.Code, rec.Body)
	}
	if code := errorCodeOf(t, rec); code != "channel_refused" {
		t.Errorf("error.code=%q want channel_refused", code)
	}
	if len(calls) != 2 {
		t.Fatalf("upstream calls=%d want exactly 2 (one conversion attempt, no rotation)", len(calls))
	}
	if !strings.Contains(rec.Body.String(), "moved into a user message") {
		t.Errorf("hint should explain the systemAsUser attempt: %s", rec.Body.String())
	}
	// 转换失败不写入学习表（避免把偶发失败学成永久转换）。
	key := systemModeKey(prompt.SystemText(calls[0]))
	if h.sysMode.Known(key) {
		t.Error("failed conversion must not be remembered")
	}
}

// TestSystemAsUserDisabledKeepsOriginalFlow 开关关闭：不做伪装，也不再白撞轮转
// （通道拒绝是请求形状问题，换号必然同样失败），客户端仍拿到 400 channel_refused。
func TestSystemAsUserDisabledKeepsOriginalFlow(t *testing.T) {
	var calls [][]byte
	up := newChannelRefusedUpstream(&calls, true)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough", SystemAsUserFallback: false})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(systemFingerprintRequest())))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 (body=%s)", rec.Code, rec.Body)
	}
	if len(calls) != 1 {
		t.Fatalf("upstream calls=%d want 1 (no conversion, no rotation)", len(calls))
	}
	if strings.Contains(rec.Body.String(), "moved into a user message") {
		t.Errorf("hint must not mention conversion when disabled: %s", rec.Body.String())
	}
}

// TestSystemAsUserRetryWithMaxRotateOne 重试是 inline 的：MaxRotate=1 也能伪装重发。
func TestSystemAsUserRetryWithMaxRotateOne(t *testing.T) {
	var calls [][]byte
	up := newChannelRefusedUpstream(&calls, false)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough", SystemAsUserFallback: true, MaxRotate: 1})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(systemFingerprintRequest())))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (want 200 with MaxRotate=1)", rec.Code, rec.Body)
	}
	if len(calls) != 2 {
		t.Fatalf("upstream calls=%d want 2", len(calls))
	}
}

// 学习表：空键不记；TTL 过期失效；超上限按最旧淘汰。
func TestSystemModeGateLifecycle(t *testing.T) {
	var g systemModeGate
	if g.Known("") || systemModeKey("") != "" {
		t.Fatal("empty key must never be learned")
	}
	g.Remember("k1")
	if !g.Known("k1") {
		t.Fatal("k1 should be known after Remember")
	}
	// 手动把时间戳推过 TTL：Known 应删除并返回 false。
	g.mu.Lock()
	g.m["k1"] = time.Now().Add(-systemModeTTL - time.Minute)
	g.mu.Unlock()
	if g.Known("k1") {
		t.Fatal("expired key should not be known")
	}
	for i := 0; i < systemModeMax+10; i++ {
		g.Remember(fmt.Sprintf("key-%04d", i))
	}
	g.mu.Lock()
	n := len(g.m)
	g.mu.Unlock()
	if n > systemModeMax {
		t.Fatalf("gate size=%d exceeds cap %d", n, systemModeMax)
	}
}
