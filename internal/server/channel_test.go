// channel_test.go 通道适配状态机（channelAdapt）与请求路径上的劣化处理回归：
// 降级重试的归档标记、循环耗尽回 400、客户端断连不罚号、非流式 error 帧计数、
// 降级窗口只记一次事件、学习表只计新学。
package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/channelstat"
	"github.com/linguo2625469/workbuddy2api-panel/internal/prompt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

const channelTestBody = `{"model":"glm-5.2","stream":true,"messages":[` +
	`{"role":"system","content":"SYS"},{"role":"user","content":"hi"}]}`

// 提示词模式的四种形态：custom 替换、append 追加、passthrough 透传、
// 降级期 passthrough/append 换 Degraded。
func TestChannelAdaptRewriteModes(t *testing.T) {
	cases := []struct {
		name         string
		mode         string
		text         string
		active       bool
		wantSystem   string
		wantDegraded bool
	}{
		{"custom 替换", "custom", "CUSTOM-SYS", false, "CUSTOM-SYS", false},
		{"append 追加", "append", "APPEND-SYS", false, "APPEND-SYS", false},
		{"passthrough 透传", "passthrough", "", false, "SYS", false},
		{"passthrough 降级期", "passthrough", "", true, prompt.Degraded, true},
		{"append 降级期退化为替换", "append", "APPEND-SYS", true, prompt.Degraded, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHandler(Config{PromptMode: tc.mode, PromptText: tc.text})
			if tc.active {
				h.degrade.Trigger()
			}
			st := &chatStat{}
			a := newChannelAdapt(h, st)
			out := a.RewritePrompt([]byte(channelTestBody))
			if got := prompt.SystemText(out); !strings.Contains(got, tc.wantSystem) {
				t.Errorf("system=%q want contains %q", got, tc.wantSystem)
			}
			if st.degraded != tc.wantDegraded {
				t.Errorf("st.degraded=%v want %v", st.degraded, tc.wantDegraded)
			}
		})
	}
}

// 内容拦截降级：同一次请求内只触发一次，degrade_trigger 按窗口只记一次，
// 且 st.degraded 必须置位（reqlog 归档靠它）。
func TestChannelAdaptTryDegraded(t *testing.T) {
	h := NewHandler(Config{PromptMode: "passthrough"})
	cs := channelstat.New("")
	h.cfg.ChannelStats = cs
	st := &chatStat{}
	a := newChannelAdapt(h, st)

	out, ok := a.TryDegraded([]byte(channelTestBody), upstream.ErrContentBlocked)
	if !ok {
		t.Fatal("passthrough 首遇内容拦截应触发降级重试")
	}
	if !st.degraded {
		t.Error("st.degraded 未置位（reqlog degraded 字段会漏记）")
	}
	if !h.degrade.Active() {
		t.Error("降级 gate 应被触发")
	}
	if got := eventCounts(t, cs); got["degrade_trigger"] != 1 {
		t.Errorf("degrade_trigger=%d want 1", got["degrade_trigger"])
	}
	if strings.Contains(prompt.SystemText(out), "SYS") {
		t.Errorf("降级后 system 应为 Degraded，得到 %q", prompt.SystemText(out))
	}

	// 同一请求第二次内容拦截：不再重试、不再记事件。
	if _, ok := a.TryDegraded(out, upstream.ErrContentBlocked); ok {
		t.Error("同一请求不应降级重试两次")
	}
	if got := eventCounts(t, cs); got["degrade_trigger"] != 1 {
		t.Errorf("同一窗口 degrade_trigger=%d want 1（按窗口计，不按请求计）", got["degrade_trigger"])
	}
}

// custom 模式不降级（自有提示词已从源头消灭 system 指纹误报）。
func TestChannelAdaptTryDegradedCustomMode(t *testing.T) {
	h := NewHandler(Config{PromptMode: "custom", PromptText: "SYS"})
	st := &chatStat{}
	a := newChannelAdapt(h, st)
	if _, ok := a.TryDegraded([]byte(channelTestBody), upstream.ErrContentBlocked); ok {
		t.Fatal("custom 模式不应触发降级重试")
	}
	if h.degrade.Active() {
		t.Error("custom 模式不应触发降级 gate")
	}
}

// 轮转循环内触发的降级重试必须进 reqlog 归档（v1.22.0 漏记 degraded）。
func TestDegradedFlagRecordedOnRetry(t *testing.T) {
	calls := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		if calls == 1 {
			return 400, `{"code":"11-128","msg":"blocked by security policy"}`, false
		}
		return 200, sseOK, true
	})
	reqLog := reqlog.New(reqlog.Config{})
	h := NewHandler(Config{
		Pool:       testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:   up,
		PromptMode: "passthrough",
		RequestLog: reqLog,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(channelTestBody)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s（降级重试后应 200）", rec.Code, rec.Body)
	}
	s := reqLog.Snapshot()
	if len(s.Recent) != 1 {
		t.Fatalf("recent=%d want 1", len(s.Recent))
	}
	e := s.Recent[0]
	if !e.Degraded {
		t.Errorf("归档 degraded=false（触发降级重试的请求必须标记）: %+v", e)
	}
	if e.Outcome != reqlog.OutcomeSuccess {
		t.Errorf("outcome=%q want success", e.Outcome)
	}
}

// 循环耗尽（MaxRotate=1）时，内容拦截必须回 400 content_blocked，
// 不能报成 503 no_healthy_account。
func TestContentBlockedExhaustedReturns400(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 400, `{"code":"11-128","msg":"blocked by security policy"}`, false
	})
	h := NewHandler(Config{
		Pool:       testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:   up,
		PromptMode: "passthrough",
		MaxRotate:  1,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(channelTestBody)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400（内容拦截不是「无号可用」）body=%s", rec.Code, rec.Body)
	}
	if got := errorCodeOf(t, rec); got != "content_blocked" {
		t.Errorf("error.code=%q want content_blocked", got)
	}
}

// 客户端中途断连（ctx 已取消）不罚号、不喂连败、不记失败归因：
// 5 次断连后账号连败计数仍为 0，归档记 interrupted 且 err_kind 为空。
func TestClientDisconnectDoesNotPenalize(t *testing.T) {
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, context.Canceled
		})},
		ChatBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	reqLog := reqlog.New(reqlog.Config{})
	h := NewHandler(Config{Pool: p, Upstream: up, RequestLog: reqLog})

	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // 客户端已走
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(channelTestBody)).WithContext(ctx)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("account missing")
	}
	if st.ConsecutiveFails != 0 {
		t.Errorf("consecutive_fails=%d want 0（客户端断连不是账号的错）", st.ConsecutiveFails)
	}
	if !st.DegradeUntil.IsZero() {
		t.Errorf("degrade_until=%v want 零值（不应触发连败降权）", st.DegradeUntil)
	}
	s := reqLog.Snapshot()
	if len(s.Recent) != 5 {
		t.Fatalf("recent=%d want 5", len(s.Recent))
	}
	e := s.Recent[len(s.Recent)-1]
	if e.Outcome != reqlog.OutcomeInterrupted {
		t.Errorf("outcome=%q want interrupted", e.Outcome)
	}
	if e.ErrKind != "" {
		t.Errorf("err_kind=%q want 空（断连不是通道失败）", e.ErrKind)
	}
}

// 非流式 200+error 帧：与流式同口径记通道事件（此前一条都不计）。
func TestNonStreamErrorFrameCountsChannelEvent(t *testing.T) {
	const sseErr = "data: {\"error\":{\"code\":\"11-128\",\"message\":\"request illegal\"}}\n\ndata: [DONE]\n\n"
	cs := channelstat.New("")
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseErr, true })
	h := NewHandler(Config{
		Pool:         testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:     up,
		ChannelStats: cs,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 channel_refused body=%s", rec.Code, rec.Body)
	}
	got := eventCounts(t, cs)
	if got["channel_refused"] != 1 {
		t.Errorf("channel_refused=%d want 1（非流式 error 帧也要计）", got["channel_refused"])
	}
	if got["stream_error"] != 0 {
		t.Errorf("stream_error=%d want 0（非流式不是流中断）", got["stream_error"])
	}
}

// 学习表单锁只计「新学」：同一个键第二次 Learn 返回 false。
func TestSystemModeLearnCountsOnce(t *testing.T) {
	var g systemModeGate
	if !g.Learn("k1") {
		t.Fatal("first Learn should report newly learned")
	}
	if g.Learn("k1") {
		t.Fatal("second Learn should not report newly learned")
	}
	if g.Len() != 1 {
		t.Errorf("len=%d want 1", g.Len())
	}
	if g.Learn("") {
		t.Fatal("empty key must never be learned")
	}
}
