package upstream

import (
	"net/http"
	"regexp"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestChatHeadersAlwaysSendConversationTrio 回归 issue #190：国际端点收紧通道
// 校验后，缺 X-Conversation-ID 的聊天请求会 400 / 11128（conversation id
// missing）。即使调用方没给会话 ID（零值 ChatMeta），三元组也必须齐全，
// 且兜底 ID 是 32 位 hex。
func TestChatHeadersAlwaysSendConversationTrio(t *testing.T) {
	c := &Client{}
	req, err := http.NewRequest(http.MethodPost, "https://example.com/v2/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	c.ChatHeaders(req, testAuth(), "", ChatMeta{})

	hex32 := regexp.MustCompile(`^[0-9a-f]{32}$`)
	for _, h := range []string{
		"X-Conversation-ID",
		"X-Conversation-Request-ID",
		"X-Conversation-Message-ID",
	} {
		v := req.Header.Get(h)
		if v == "" {
			t.Fatalf("%s missing (通道校验会拒绝缺少会话三元组的请求)", h)
		}
		if !hex32.MatchString(v) {
			t.Errorf("%s = %q, want 32 hex", h, v)
		}
	}

	// 兜底会话 ID 每次请求新生成（不是常量），且与 request/message ID 不冲突。
	req2, _ := http.NewRequest(http.MethodPost, "https://example.com/v2/chat/completions", nil)
	c.ChatHeaders(req2, testAuth(), "", ChatMeta{})
	if req.Header.Get("X-Conversation-ID") == req2.Header.Get("X-Conversation-ID") {
		t.Error("fallback X-Conversation-ID should be fresh per request")
	}
}

// TestChatHeadersPreservesConversationID 入站带来的会话 ID 优先透传，不被兜底覆盖。
func TestChatHeadersPreservesConversationID(t *testing.T) {
	c := &Client{}
	req, _ := http.NewRequest(http.MethodPost, "https://example.com/v2/chat/completions", nil)
	const conv = "0123456789abcdef0123456789abcdef"
	c.ChatHeaders(req, testAuth(), "", ChatMeta{ConversationID: conv})
	if got := req.Header.Get("X-Conversation-ID"); got != conv {
		t.Errorf("X-Conversation-ID = %q, want supplied %q", got, conv)
	}
}

// TestChatHeadersDerivedKeySanitizedAndIsolated 派生会话键（d-<hex>）必须转为 32 hex
// 并按账号 UID 隔离，不得向上游泄露 "d-" 前缀或跨账号共用同一会话 ID。
func TestChatHeadersDerivedKeySanitizedAndIsolated(t *testing.T) {
	c := &Client{}
	const derived = "d-0123456789abcdef0123456789abcdef"
	req1, _ := http.NewRequest(http.MethodPost, "https://example.com/v2/chat/completions", nil)
	req2, _ := http.NewRequest(http.MethodPost, "https://example.com/v2/chat/completions", nil)
	reqOther, _ := http.NewRequest(http.MethodPost, "https://example.com/v2/chat/completions", nil)

	c.ChatHeaders(req1, &auth.Auth{UID: "u1", AccessToken: "at"}, "", ChatMeta{ConversationID: derived})
	c.ChatHeaders(req2, &auth.Auth{UID: "u1", AccessToken: "at"}, "", ChatMeta{ConversationID: derived})
	c.ChatHeaders(reqOther, &auth.Auth{UID: "u2", AccessToken: "at"}, "", ChatMeta{ConversationID: derived})

	got1 := req1.Header.Get("X-Conversation-ID")
	got2 := req2.Header.Get("X-Conversation-ID")
	gotOther := reqOther.Header.Get("X-Conversation-ID")

	hex32 := regexp.MustCompile(`^[0-9a-f]{32}$`)
	if !hex32.MatchString(got1) {
		t.Fatalf("derived X-Conversation-ID = %q, want 32 hex without d- prefix", got1)
	}
	if got1 != got2 {
		t.Errorf("same account + same derived key should be stable: %q != %q", got1, got2)
	}
	if got1 == gotOther {
		t.Errorf("different accounts must not share derived X-Conversation-ID: %q", got1)
	}
}

// testAuth 返回最小的 CN 账号（ChatHeaders 会读 UID/凭证，nil 会解引用）。
func testAuth() *auth.Auth {
	return &auth.Auth{UID: "u1", AccessToken: "at"}
}
