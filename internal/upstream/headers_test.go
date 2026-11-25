package upstream

import (
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestChatHeadersAlwaysSendConversationTrio 回归 issue #190：国际端点收紧通道
// 校验后，缺 X-Conversation-ID 的聊天请求会 400 / 11128（conversation id
// missing）。即使调用方没给会话 ID（零值 ChatMeta），三件套也必须齐全。
//
// 形态要求（真机 5.9.x 实测）：X-Conversation-ID 是**带连字符的 UUIDv7**，
// 而 Request-ID / Message-ID 是 **32 位无连字符 hex**——两者形态本来就不同，
// 旧实现把会话 ID 也发 32 hex，与客户端对不上。
func TestChatHeadersAlwaysSendConversationTrio(t *testing.T) {
	c := &Client{}
	req, err := http.NewRequest(http.MethodPost, "https://example.com/v2/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	c.ChatHeaders(req, testAuth(), "", ChatMeta{})
	requireConversationTrio(t, req)

	// 兜底会话 ID 每次请求新生成（不是常量）。
	req2, _ := http.NewRequest(http.MethodPost, "https://example.com/v2/chat/completions", nil)
	c.ChatHeaders(req2, testAuth(), "", ChatMeta{})
	if req.Header.Get("X-Conversation-ID") == req2.Header.Get("X-Conversation-ID") {
		t.Error("fallback X-Conversation-ID should be fresh per request")
	}
}

// requireConversationTrio 断言会话头族齐全且**每个头形态正确**。
func requireConversationTrio(t *testing.T, req *http.Request) {
	t.Helper()
	hex32 := regexp.MustCompile(`^[0-9a-f]{32}$`)
	uuidV7 := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

	for _, h := range []string{
		"X-Conversation-ID",
		"X-Conversation-Request-ID",
		"X-Conversation-Message-ID",
		"X-Request-ID",
		"X-Root-Request-ID",
		"X-Trace-ID",
		"X-B3-TraceId",
		"X-B3-SpanId",
		"X-B3-ParentSpanId",
		"b3",
		"traceparent",
		"baggage",
	} {
		if req.Header.Get(h) == "" {
			t.Fatalf("%s missing（客户端头族是整体形态，缺一个就可判别）", h)
		}
	}

	conv := req.Header.Get("X-Conversation-ID")
	if !uuidV7.MatchString(conv) {
		t.Errorf("X-Conversation-ID = %q, want 带连字符 UUIDv7", conv)
	}
	for _, h := range []string{
		"X-Conversation-Request-ID", "X-Conversation-Message-ID",
		"X-Request-ID", "X-Trace-ID", "X-B3-TraceId",
	} {
		if v := req.Header.Get(h); !hex32.MatchString(v) {
			t.Errorf("%s = %q, want 32 hex", h, v)
		}
	}
	// X-Request-ID 与 X-Conversation-Message-ID 是同一个值（真机形态）。
	if a, b := req.Header.Get("X-Request-ID"), req.Header.Get("X-Conversation-Message-ID"); a != b {
		t.Errorf("X-Request-ID=%q 应等于 X-Conversation-Message-ID=%q", a, b)
	}
	// SpanId 必须是 16 hex 且独立随机（不得是 messageID 的前缀切片）。
	span := req.Header.Get("X-B3-SpanId")
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(span) {
		t.Errorf("X-B3-SpanId = %q, want 16 hex", span)
	}
	if span == req.Header.Get("X-Request-ID")[:16] {
		t.Error("X-B3-SpanId 复用了 messageID 前缀（会带出时间前缀，是可推导关联）")
	}
	// Trace 与轮级 request id 必须不同（真机两族恒不相等）。
	if req.Header.Get("X-Trace-ID") == req.Header.Get("X-Conversation-Request-ID") {
		t.Error("X-Trace-ID 不应回落成 X-Conversation-Request-ID")
	}
	// baggage 成员集合固定 4 个，两个 buddy 留空值。
	wantBag := "buddyId=,buddyName=,codebuddy.session_id=" + conv +
		",codebuddy.conversation_request_id=" + req.Header.Get("X-Conversation-Request-ID")
	if got := req.Header.Get("baggage"); got != wantBag {
		t.Errorf("baggage = %q, want %q", got, wantBag)
	}
}

// TestChatHeadersNoSelfMadeFingerprint 官方客户端**从不发** X-Machine-ID /
// X-Session-ID（设备标识在 /v2/report 的 body 里）。发它们等于自报网关身份。
// 同时 chat 也不带 Origin/Referer/Accept-Language（那是浏览器语义，RPC 层都没有）。
func TestChatHeadersNoSelfMadeFingerprint(t *testing.T) {
	c := &Client{}
	req, _ := http.NewRequest(http.MethodPost, "https://example.com/v2/chat/completions", nil)
	c.ChatHeaders(req, testAuth(), "", ChatMeta{})
	for _, h := range []string{"X-Machine-ID", "X-Session-ID", "Origin", "Referer", "Accept-Language"} {
		if v := req.Header.Get(h); v != "" {
			t.Errorf("%s = %q，客户端 chat 请求不带该头（自造指纹）", h, v)
		}
	}
	// X-No-* 哨兵头是 CodeBuddy CLI 的约定，桌面端个人号不发。
	for _, h := range []string{"X-No-Authorization", "X-No-User-Id", "X-No-Enterprise-Id", "X-No-Department-Info"} {
		if v := req.Header.Get(h); v != "" {
			t.Errorf("%s = %q，桌面端个人号不发哨兵头", h, v)
		}
	}
}

// TestChatHeadersRealDeviceFingerprints chat 必须带真机的 SDK / agent 段头。
// 缺这批头，上游看到的是残缺的头集合——这正是改造前「没有出站指纹」的实指。
func TestChatHeadersRealDeviceFingerprints(t *testing.T) {
	c := &Client{}
	req, _ := http.NewRequest(http.MethodPost, "https://example.com/v2/chat/completions", nil)
	c.ChatHeaders(req, testAuth(), "", ChatMeta{})

	want := map[string]string{
		"X-Requested-With":             "XMLHttpRequest",
		"X-CodeBuddy-Request":          "1",
		"X-Agent-Intent":               "craft",
		"X-Agent-Type":                 "main",
		"X-Agent-Runtime":              "local-agent",
		"X-Agent-Purpose":              "conversation",
		"X-Stainless-Lang":             "js",
		"X-Stainless-Package-Version":  "6.25.0",
		"X-Stainless-OS":               "Windows",
		"X-Stainless-Arch":             "x64",
		"X-Stainless-Runtime":          "node",
		"X-Stainless-Runtime-Version":  "v22.21.1",
		"X-Stainless-Retry-Count":      "0",
		"X-Private-Data":               "false", // CN 个人号
		"X-Product":                    "SaaS",
		"X-IDE-Name":                   "WorkBuddy",
		"X-IDE-Type":                   "WorkBuddy",
		"Accept":                       "application/json",
	}
	for h, w := range want {
		if got := req.Header.Get(h); got != w {
			t.Errorf("%s = %q, want %q", h, got, w)
		}
	}
	// X-Domain 恒发裸主机名（不是完整 URL、也不能空）。
	if d := req.Header.Get("X-Domain"); d == "" || strings.Contains(d, "://") {
		t.Errorf("X-Domain = %q, want 裸主机名", d)
	}
}

// TestChatHeadersAuxiliaryAgentType 网关自发任务用 auxiliary，不冒充用户主对话。
func TestChatHeadersAuxiliaryAgentType(t *testing.T) {
	c := &Client{}
	req, _ := http.NewRequest(http.MethodPost, "https://example.com/v2/chat/completions", nil)
	c.ChatHeaders(req, testAuth(), "", ChatMeta{AgentType: AgentTypeAuxiliary})
	if got := req.Header.Get("X-Agent-Type"); got != "auxiliary" {
		t.Errorf("X-Agent-Type = %q, want auxiliary", got)
	}
}

// TestRPCHeadersDropChatOnlyHeaders RPC 层（CommonHeaders）不发 chat 专属头：
// X-Requested-With / X-CodeBuddy-Request 只有 /v2/chat/completions 带。
// 旧实现在 CommonHeaders 里「全出站覆盖」，等于给 refresh 等轻请求多发两个头。
func TestRPCHeadersDropChatOnlyHeaders(t *testing.T) {
	c := &Client{}
	req, _ := http.NewRequest(http.MethodGet, "https://example.com/v3/config", nil)
	c.CommonHeaders(req, testAuth())
	for _, h := range []string{"X-Requested-With", "X-CodeBuddy-Request", "X-Machine-ID", "X-Session-ID", "Origin", "Referer"} {
		if v := req.Header.Get(h); v != "" {
			t.Errorf("RPC 层不应带 %s = %q", h, v)
		}
	}
	if got := req.Header.Get("Accept"); got != "application/json, text/plain, */*" {
		t.Errorf("RPC Accept = %q, want 真机宽松值", got)
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
