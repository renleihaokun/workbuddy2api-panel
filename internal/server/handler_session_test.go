// handler_session_test.go 「会话失效」处置的端到端用例：401 求证三态、三振、
// AT 仍有效时的凭证层标记。
//
// 为什么单开文件：这些用例要按**路径**分派 fake 响应（chat 401 而 refresh 成功），
// 与 handler_test.go 里按 Authorization 分派的 newFakeUpstream 不同；分开写避免
// 把既有 helper 改得越来越绕。
package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// newPathUpstream 构造按路径分派的 fake 上游：/token/refresh 与其余（chat/搜索）
// 各给一套响应；refreshCalls 统计刷新请求次数（验证求证节流）。
func newPathUpstream(
	chat func() (status int, body string),
	refresh func() (status int, body string),
) (*upstream.Client, *int32) {
	var refreshCalls int32
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			status, body := 0, ""
			if strings.Contains(r.URL.Path, "/plugin/auth/token/refresh") {
				atomic.AddInt32(&refreshCalls, 1)
				status, body = refresh()
			} else {
				status, body = chat()
			}
			ct := "application/json"
			if strings.HasPrefix(body, "data: ") {
				ct = "text/event-stream"
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{ct}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	return up, &refreshCalls
}

const (
	sessionDeadBody  = `{"code":12153,"msg":"Offline user session not found"}`
	refreshOKBody    = `{"code":0,"msg":"ok","data":{"accessToken":"at-new","refreshToken":"rt-new","expiresIn":3600}}`
	refreshDeadBody  = `{"code":12153,"msg":"invalid_grant: Token is not active"}`
	sessionChatBody  = `{"model":"glm-5.2","messages":[]}`
	sessionChatRoute = "/v1/chat/completions"
)

func doChat(h *Handler) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", sessionChatRoute, strings.NewReader(sessionChatBody)))
	return rec
}

// TestChatSessionDeadVerifyAliveDoesNotCount 401 + 求证=活（refresh 成功）：
// 只做 30s 短冷却，不计数、不禁用，且新 token 已写回内存。
func TestChatSessionDeadVerifyAliveDoesNotCount(t *testing.T) {
	up, refreshCalls := newPathUpstream(
		func() (int, string) { return 401, sessionDeadBody },
		func() (int, string) { return 200, refreshOKBody },
	)
	a := &auth.Auth{UID: "u1", AccessToken: "at-old", RefreshToken: "rt-old", ExpiresAt: 9999999999}
	p := testPoolWith(a)
	h := NewHandler(Config{Pool: p, Upstream: up, VerifySessionOn401: true})

	rec := doChat(h)
	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("u1")
	if st.Disabled {
		t.Fatalf("求证=活时不得禁用: %+v", st)
	}
	if !st.Cooling || st.CoolRemaining < int64(verifyAliveCooldown/time.Second)-3 {
		t.Errorf("应做 %s 级别短冷却: %+v", verifyAliveCooldown, st)
	}
	if got := atomic.LoadInt32(refreshCalls); got != 1 {
		t.Errorf("refresh calls=%d want 1（求证只打一次）", got)
	}
	if a.AccessTokenValue() != "at-new" {
		t.Errorf("求证成功应写回新 token，got %q", a.AccessTokenValue())
	}
}

// TestChatSessionDeadVerifyDeadCountsToThreshold 401 + 求证=死：走三振，
// 第三次才禁用；窗口内复用上次结论，只打一次 refresh。
func TestChatSessionDeadVerifyDeadCountsToThreshold(t *testing.T) {
	up, refreshCalls := newPathUpstream(
		func() (int, string) { return 401, sessionDeadBody },
		func() (int, string) { return 401, refreshDeadBody },
	)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", RefreshToken: "rt1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, VerifySessionOn401: true})
	// 默认节流窗口 60s：三次请求落在同一窗口内，只应发一次求证。
	for i := 1; i < pool.SessionDeadThreshold(); i++ {
		doChat(h)
		if st, _ := p.Status("u1"); st.Disabled {
			t.Fatalf("第 %d 次不应禁用: %+v", i, st)
		}
	}
	doChat(h)
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Fatalf("连续 %d 次求证=死应禁用: %+v", pool.SessionDeadThreshold(), st)
	}
	if st.DisabledKind != pool.DisableKindSessionDead || !st.NeedsRelogin {
		t.Errorf("disabled_kind=%q needs_relogin=%v want session_dead/true", st.DisabledKind, st.NeedsRelogin)
	}
	if got := atomic.LoadInt32(refreshCalls); got != 1 {
		t.Errorf("refresh calls=%d want 1（窗口内复用结论）", got)
	}
}

// TestChatSessionDeadVerifyInconclusiveDoesNotCount 401 + 求证无判词（refresh 500）：
// 既不判活也不判死——不计数、不禁用，只做短冷却。
func TestChatSessionDeadVerifyInconclusiveDoesNotCount(t *testing.T) {
	up, refreshCalls := newPathUpstream(
		func() (int, string) { return 401, sessionDeadBody },
		func() (int, string) { return 500, `{"code":500,"msg":"boom"}` },
	)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", RefreshToken: "rt1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, VerifySessionOn401: true})

	for i := 0; i < pool.SessionDeadThreshold()+1; i++ {
		doChat(h)
	}
	st, _ := p.Status("u1")
	if st.Disabled {
		t.Fatalf("无判词不得禁用: %+v", st)
	}
	if !st.Cooling {
		t.Errorf("无判词应做短冷却（不计数但也不放行）: %+v", st)
	}
	if got := atomic.LoadInt32(refreshCalls); got != 1 {
		t.Errorf("refresh calls=%d want 1（无判词也在节流窗口内复用）", got)
	}
}

// TestChatSkipsPreRefreshWhenRefreshTokenKnownDead RT 已被判死的号不再撞预刷新端点：
// AT 临近过期也直接用当前 AT 发请求——明知会失败的刷新只给上游多一个异常信号。
func TestChatSkipsPreRefreshWhenRefreshTokenKnownDead(t *testing.T) {
	up, refreshCalls := newPathUpstream(
		func() (int, string) { return 200, sseOK },
		func() (int, string) { return 401, sessionDeadBody },
	)
	a := &auth.Auth{UID: "u1", AccessToken: "at-old", RefreshToken: "rt-old",
		ExpiresAt: time.Now().Add(5 * time.Minute).Unix()}
	p := testPoolWith(a)
	p.MarkRefreshTokenDead("u1")
	h := NewHandler(Config{Pool: p, Upstream: up, VerifySessionOn401: true})

	if rec := doChat(h); rec.Code != 200 {
		t.Fatalf("应直接用当前 AT 服务: code=%d body=%s", rec.Code, rec.Body)
	}
	if got := atomic.LoadInt32(refreshCalls); got != 0 {
		t.Errorf("RT 已判死时不该再撞预刷新端点，refresh calls=%d", got)
	}
}

// TestChatBare401VerifiesDead 裸 401（网关 HTML 页，无业务信封）也要能识别真死号：
// 求证=Dead 才计数，连续 3 次禁用；窗口内只打一次 refresh。
func TestChatBare401VerifiesDead(t *testing.T) {
	gateway401 := `<html><head><title>401 Authorization Required</title></head><body>openresty</body></html>`
	up, refreshCalls := newPathUpstream(
		func() (int, string) { return 401, gateway401 },
		func() (int, string) { return 401, sessionDeadBody },
	)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", RefreshToken: "rt1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, VerifySessionOn401: true})

	for i := 1; i < pool.SessionDeadThreshold(); i++ {
		doChat(h)
		if st, _ := p.Status("u1"); st.Disabled {
			t.Fatalf("第 %d 次不应禁用: %+v", i, st)
		}
	}
	doChat(h)
	st, _ := p.Status("u1")
	if !st.Disabled || !st.NeedsRelogin {
		t.Fatalf("裸 401 + 求证=死 连续 %d 次应禁用: %+v", pool.SessionDeadThreshold(), st)
	}
	if got := atomic.LoadInt32(refreshCalls); got != 1 {
		t.Errorf("refresh calls=%d want 1（节流窗口内复用结论）", got)
	}
}

// TestChatBare401VerifyAliveNoPenalty 裸 401 + 求证=活：完全维持旧行为——
// 不冷却、不禁用（只有连败计数照旧），换号继续服务。
func TestChatBare401VerifyAliveNoPenalty(t *testing.T) {
	gateway401 := `<html><head><title>401 Authorization Required</title></head><body>openresty</body></html>`
	up, refreshCalls := newPathUpstream(
		func() (int, string) { return 401, gateway401 },
		func() (int, string) { return 200, refreshOKBody },
	)
	a := &auth.Auth{UID: "u1", AccessToken: "at1", RefreshToken: "rt1", ExpiresAt: 9999999999}
	p := testPoolWith(a)
	h := NewHandler(Config{Pool: p, Upstream: up, VerifySessionOn401: true})

	for i := 0; i < pool.SessionDeadThreshold()+1; i++ {
		doChat(h)
	}
	st, _ := p.Status("u1")
	if st.Disabled || st.Cooling {
		t.Fatalf("求证=活时不得罚号: %+v", st)
	}
	if st.RefreshTokenDead {
		t.Errorf("求证成功不该留 RT 判死标记: %+v", st)
	}
	if got := atomic.LoadInt32(refreshCalls); got != 1 {
		t.Errorf("refresh calls=%d want 1", got)
	}
	if a.AccessTokenValue() != "at-new" {
		t.Errorf("求证成功应写回新 token，got %q", a.AccessTokenValue())
	}
}

// TestChatPreRefreshSessionDeadKeepsServingWhenAccessValid RT 判死但 AT 未过期：
// 请求前预刷新失败不应换号——继续用当前 AT 把这一发请求打完。
func TestChatPreRefreshSessionDeadKeepsServingWhenAccessValid(t *testing.T) {
	up, refreshCalls := newPathUpstream(
		func() (int, string) { return 200, sseOK },
		func() (int, string) { return 401, sessionDeadBody },
	)
	// AT 还剩 5 分钟（落在默认 10 分钟预刷新窗口内）→ 会先尝试刷新。
	a := &auth.Auth{UID: "u1", AccessToken: "at-old", RefreshToken: "rt-old",
		ExpiresAt: time.Now().Add(5 * time.Minute).Unix()}
	p := testPoolWith(a)
	h := NewHandler(Config{Pool: p, Upstream: up, VerifySessionOn401: true})

	rec := doChat(h)
	if rec.Code != 200 {
		t.Fatalf("AT 仍有效时应继续服务: code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("u1")
	if st.Disabled {
		t.Fatalf("不得禁用: %+v", st)
	}
	if st.Cooling {
		t.Errorf("不该冷却（号还能干活）: %+v", st)
	}
	if !st.RefreshTokenDead {
		t.Errorf("应记下 refresh_token_dead 凭证层事实: %+v", st)
	}
	if got := atomic.LoadInt32(refreshCalls); got != 1 {
		t.Errorf("refresh calls=%d want 1", got)
	}
}

// TestChatPreRefreshSessionDeadCountsWhenAccessExpired AT 已过期且刷新判死：
// 才计入 session dead（不变的是仍需三振）。
func TestChatPreRefreshSessionDeadCountsWhenAccessExpired(t *testing.T) {
	up, _ := newPathUpstream(
		func() (int, string) { return 200, sseOK },
		func() (int, string) { return 401, sessionDeadBody },
	)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at-old", RefreshToken: "rt-old", ExpiresAt: 1})
	h := NewHandler(Config{Pool: p, Upstream: up, VerifySessionOn401: true})

	if rec := doChat(h); rec.Code != 503 {
		t.Fatalf("AT 过期且刷新失败应换号至 503，code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("u1")
	if st.Disabled {
		t.Fatalf("单次不得禁用: %+v", st)
	}
	if !st.RefreshTokenDead {
		t.Errorf("应同时记下 refresh_token_dead: %+v", st)
	}
}
