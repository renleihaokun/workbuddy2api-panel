package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// withChatBodyLimit 临时下调请求体上限（生产恒 64 MiB），测试结束自动还原。
// 包内测试没有 t.Parallel，改全局 var 不会与其他用例交叉。
func withChatBodyLimit(t *testing.T, n int64) {
	t.Helper()
	old := maxChatBodyBytes
	maxChatBodyBytes = n
	t.Cleanup(func() { maxChatBodyBytes = old })
}

// newBareHandler 空账号池的 handler：足以覆盖「读 body 阶段」的所有分支
// （越限在读之前就返回，限内则落到选号失败的 503）。
func newBareHandler() *Handler {
	return NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New()})
}

// TestChatBodyLimitRejectsOversize 越限必须 413 + request_too_large，且不落到上游。
func TestChatBodyLimitRejectsOversize(t *testing.T) {
	withChatBodyLimit(t, 1024)
	h := newBareHandler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(strings.Repeat("x", 4096)))
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", rec.Code, rec.Body.String())
	}
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if e.Error.Code != "request_too_large" {
		t.Errorf("error.code = %q, want request_too_large", e.Error.Code)
	}
}

// TestChatBodyLimitAllowsUnderLimit 限内请求不受安全网影响（空池 → 503，不是 413）。
func TestChatBodyLimitAllowsUnderLimit(t *testing.T) {
	withChatBodyLimit(t, 1<<20)
	h := newBareHandler()

	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("限内请求被 413：%s", rec.Body.String())
	}
}

// TestResponsesBodyLimitRejectsOversize Responses API 共用同一道安全网。
func TestResponsesBodyLimitRejectsOversize(t *testing.T) {
	withChatBodyLimit(t, 512)
	h := newBareHandler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(strings.Repeat("x", 4096)))
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", rec.Code, rec.Body.String())
	}
}

// TestMessagesBodyLimitRejectsOversize 搜索端点超限回 413，而不是被 LimitReader
// 截断后再报「body must be a JSON object」那种误导性 400。
func TestMessagesBodyLimitRejectsOversize(t *testing.T) {
	h := newBareHandler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/search/messages",
		strings.NewReader(strings.Repeat("x", messagesBodyLimit+1024)))
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", rec.Code, rec.Body.String())
	}
}
