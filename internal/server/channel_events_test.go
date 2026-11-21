// channel_events_test.go 反代通道事件埋点：11-128 被拦（含救回）、结构性拒绝、
// 流内 error 帧、成功请求不误计。这些计数是「反代劣化」看板的核心数据源，
// 与请求终态无关——救回的成功请求同样要留下痕迹。
package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/channelstat"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// upstreamWithBody 假上游：任何请求都回固定 400 body（用于构造特定分类）。
func upstreamWithBody(t *testing.T, body string) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 400,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

// eventCounts 取账本当前小时窗口内的事件计数。
func eventCounts(t *testing.T, l *channelstat.Ledger) map[string]int64 {
	t.Helper()
	now := time.Now()
	_, totals := l.Series(now.Add(-time.Hour), now.Add(time.Hour), false)
	return totals.Kinds
}

// 11-128 被 system→user 救回：终态是 200，但 channel_refused / sys_as_user 必须各记一次，
// 学习次数也要落账（进程内学习表重启清零，累计值靠账本）。
func TestChannelEventsRecoveredChannelRefused(t *testing.T) {
	var calls [][]byte
	up := newChannelRefusedUpstream(&calls, false)
	cs := channelstat.New("")
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up, PromptMode: "passthrough", SystemAsUserFallback: true,
		ChannelStats: cs,
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(systemFingerprintRequest())))
	if rec.Code != 200 {
		t.Fatalf("code=%d want 200（救回成功）", rec.Code)
	}
	got := eventCounts(t, cs)
	if got["channel_refused"] != 1 {
		t.Errorf("channel_refused=%d want 1（上游拦了一次，即使最终成功也要记）", got["channel_refused"])
	}
	if got["sys_as_user"] != 1 {
		t.Errorf("sys_as_user=%d want 1", got["sys_as_user"])
	}
	if _, _, learned := cs.Snapshot(); learned != 1 {
		t.Errorf("learned_total=%d want 1（伪装成功后学习一次）", learned)
	}
}

// 结构性 11-128（搬 system 也救不回来）：计一次 channel_refused，不计 sys_as_user。
func TestChannelEventsStructuralRefusal(t *testing.T) {
	cs := channelstat.New("")
	// 「conversation id missing」形态：IsSystemMovedRefusal=false → 不重发。
	up := upstreamWithBody(t, `{"code":11128,"msg":"conversation id missing"}`)
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up, PromptMode: "passthrough", SystemAsUserFallback: true,
		ChannelStats: cs,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(systemFingerprintRequest())))
	if rec.Code != 400 {
		t.Fatalf("code=%d want 400", rec.Code)
	}
	got := eventCounts(t, cs)
	if got["channel_refused"] != 1 {
		t.Errorf("channel_refused=%d want 1", got["channel_refused"])
	}
	if got["sys_as_user"] != 0 {
		t.Errorf("sys_as_user=%d want 0（结构性拒绝不搬移）", got["sys_as_user"])
	}
}

// 流内 error 帧：上游先 200、再在流里报 11-128 → channel_refused + stream_error 各一次。
func TestChannelEventsStreamErrorFrame(t *testing.T) {
	const sseErr = "data: {\"error\":{\"code\":\"11-128\",\"message\":\"request illegal\"}}\n\ndata: [DONE]\n\n"
	cs := channelstat.New("")
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseErr, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up, ChannelStats: cs,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`)))
	got := eventCounts(t, cs)
	if got["channel_refused"] != 1 {
		t.Errorf("channel_refused=%d want 1（流内 11-128 也要计）", got["channel_refused"])
	}
	if got["stream_error"] != 1 {
		t.Errorf("stream_error=%d want 1", got["stream_error"])
	}
}

// 正常成功请求：一条事件都不记（否则看板会被日常流量淹没）。
func TestChannelEventsNotCountedOnSuccess(t *testing.T) {
	cs := channelstat.New("")
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up, ChannelStats: cs,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`)))
	if got := eventCounts(t, cs); len(got) != 0 {
		t.Fatalf("成功请求不应记事件，得到 %+v", got)
	}
}
