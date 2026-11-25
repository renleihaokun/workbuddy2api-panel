// identity_mode_test.go 出站身份两种模式的形态契约。
//
// legacy 是**逃生门**：生产上若上游对新形态有意外反应，改一行配置就要能立刻
// 回到已知可跑的形态。所以这里逐条钉住两者的差异，任何一侧漂移都会红。
package upstream

import (
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// newModeClient 构造指定模式的最小 Client。
func newModeClient(mode IdentityMode) *Client {
	c := &Client{}
	c.SetIdentityMode(mode)
	return c
}

// TestLegacyModeRestoresSelfMadeHeaders legacy 模式必须恢复改造前的自造设备头
// 与浏览器语义头（X-Machine-ID / X-Session-ID / Origin / Referer / Accept-Language）。
func TestLegacyModeRestoresSelfMadeHeaders(t *testing.T) {
	c := newModeClient(ModeLegacy)
	a := &auth.Auth{UID: "u1", AccessToken: "at"}
	req, _ := http.NewRequest(http.MethodPost, "https://example.com/v2/chat/completions", nil)
	c.ChatHeaders(req, a, "", ChatMeta{})

	// 改造前的 36 hex 无连字符派生必须原样复现（回退要真能回到「已知可跑」的形态，
	// 换了值就不算回退）。
	if got, want := req.Header.Get("X-Machine-ID"), legacyDeviceID("u1", "machine"); got != want {
		t.Errorf("X-Machine-ID = %q, want %q", got, want)
	}
	if got, want := req.Header.Get("X-Session-ID"), legacyDeviceID("u1", "session"); got != want {
		t.Errorf("X-Session-ID = %q, want %q", got, want)
	}
	if !regexp.MustCompile(`^[0-9a-f]{36}$`).MatchString(req.Header.Get("X-Machine-ID")) {
		t.Error("legacy 的 X-Machine-ID 应是 36 hex 无连字符（改造前形态）")
	}
	for _, h := range []string{"Origin", "Referer", "Accept-Language"} {
		if req.Header.Get(h) == "" {
			t.Errorf("legacy 模式应恢复 %s", h)
		}
	}
	// 改造前的旧三段式 UA（版本 5.5.4 / CLI 2.137.1）。
	if got, want := req.Header.Get("User-Agent"), legacyUA(a); got != want {
		t.Errorf("legacy UA = %q, want %q", got, want)
	}
	// 改造前的会话头族：Trace 回落轮级（两族恒等）。
	if req.Header.Get("X-Trace-ID") != req.Header.Get("X-Conversation-Request-ID") {
		t.Error("legacy 模式下 X-Trace-ID 应回落成 X-Conversation-Request-ID（改造前行为）")
	}
	// 改造前的 SpanId = messageID[:16]。
	if got, want := req.Header.Get("X-B3-SpanId"), req.Header.Get("X-Request-ID")[:16]; got != want {
		t.Errorf("legacy 的 X-B3-SpanId = %q, want %q（messageID 前缀）", got, want)
	}
}

// TestCaptureModeDropsLegacyHeaders capture（默认）下这些头一个都不能有。
func TestCaptureModeDropsLegacyHeaders(t *testing.T) {
	c := newModeClient(ModeCapture)
	a := &auth.Auth{UID: "u1", AccessToken: "at"}
	req, _ := http.NewRequest(http.MethodPost, "https://example.com/v2/chat/completions", nil)
	c.ChatHeaders(req, a, "", ChatMeta{})

	for _, h := range []string{
		"X-Machine-ID", "X-Session-ID", // 官方客户端从不发
		"Origin", "Referer", "Accept-Language", // chat 走 SDK 链，无浏览器语义
		"X-No-Authorization", "X-No-User-Id", "X-No-Enterprise-Id", "X-No-Department-Info",
	} {
		if v := req.Header.Get(h); v != "" {
			t.Errorf("capture 模式不应带 %s = %q", h, v)
		}
	}
	// 这次改造要修掉的形态问题在 capture 下必须已修正。
	if req.Header.Get("X-Trace-ID") == req.Header.Get("X-Conversation-Request-ID") {
		t.Error("capture 下 X-Trace-ID 不应等于 X-Conversation-Request-ID")
	}
	if req.Header.Get("X-B3-SpanId") == req.Header.Get("X-Request-ID")[:16] {
		t.Error("capture 下 X-B3-SpanId 不应复用 messageID 前缀")
	}
}

// TestBothModesKeepConversationTrio 两种模式都必须把会话三件套发齐——
// 缺了它上游直接 400 / 11-128（conversation id missing），与模式无关。
func TestBothModesKeepConversationTrio(t *testing.T) {
	for _, mode := range []IdentityMode{ModeCapture, ModeLegacy} {
		c := newModeClient(mode)
		req, _ := http.NewRequest(http.MethodPost, "https://example.com/v2/chat/completions", nil)
		c.ChatHeaders(req, &auth.Auth{UID: "u1", AccessToken: "at"}, "", ChatMeta{})
		for _, h := range []string{
			"X-Conversation-ID", "X-Conversation-Request-ID", "X-Conversation-Message-ID",
		} {
			if req.Header.Get(h) == "" {
				t.Errorf("mode=%v 缺 %s（上游会判 conversation id missing）", mode, h)
			}
		}
	}
}

// TestBothModesSendUserAgent 两种模式的出站 UA 都不得是 Go 默认串。
func TestBothModesSendUserAgent(t *testing.T) {
	for _, mode := range []IdentityMode{ModeCapture, ModeLegacy} {
		c := newModeClient(mode)
		for _, mk := range []func(*http.Request){
			func(r *http.Request) { c.CommonHeaders(r, &auth.Auth{UID: "u1"}) },
			func(r *http.Request) { c.ChatHeaders(r, &auth.Auth{UID: "u1", AccessToken: "at"}, "", ChatMeta{}) },
			func(r *http.Request) { c.BillingHeaders(r, &auth.Auth{UID: "u1", AccessToken: "at"}) },
		} {
			req, _ := http.NewRequest(http.MethodPost, "https://example.com/x", nil)
			mk(req)
			ua := req.Header.Get("User-Agent")
			if ua == "" || strings.Contains(ua, "Go-http-client") {
				t.Errorf("mode=%v UA=%q 缺失或是 Go 默认串", mode, ua)
			}
		}
	}
}

// TestParseIdentityMode 枚举归一化；非法值必须报错。
func TestParseIdentityMode(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want IdentityMode
		ok   bool
	}{
		{"", ModeCapture, true},
		{"capture", ModeCapture, true},
		{"  CAPTURE  ", ModeCapture, true},
		{"official", ModeCapture, true},
		{"legacy", ModeLegacy, true},
		{"compat", ModeLegacy, true},
		{"leagcy", ModeCapture, false},
		{"1", ModeCapture, false},
	} {
		got, err := ParseIdentityMode(tc.in)
		if tc.ok && err != nil {
			t.Errorf("ParseIdentityMode(%q) 意外失败: %v", tc.in, err)
			continue
		}
		if !tc.ok {
			if err == nil {
				t.Errorf("ParseIdentityMode(%q) 应报错", tc.in)
			}
			continue
		}
		if got != tc.want {
			t.Errorf("ParseIdentityMode(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestIdentityModeZeroValueIsCapture 零值 Client（未调用过 SetIdentityMode）
// 必须是 capture——绝大多数测试与生产装配都依赖这一点。
func TestIdentityModeZeroValueIsCapture(t *testing.T) {
	c := &Client{}
	if c.identityModeIsLegacy() {
		t.Error("零值 Client 应为 capture 模式")
	}
	if (*Client)(nil).identityModeIsLegacy() {
		// nil Client 回退 legacy 会让「构造失败」静默变成回退模式。
		// 这里仅记录现状：nil 走 legacy 分支是为避免 nil 解引用，实际不会出现在生产路径。
		t.Log("nil Client 走 legacy 分支（防空指针）")
	}
}

// TestChatHeadersNilAuth 回归：a == nil 时不得 panic，且 X-Domain 恒发。
//
// X-Domain 是「真机每条 chat 必带」的头，缺了本身就是可判别形态。首版实现里
// nil auth 会走进一个不发 X-Domain 的分支，这里钉住「无论有没有 auth 都发」。
func TestChatHeadersNilAuth(t *testing.T) {
	for _, mode := range []IdentityMode{ModeCapture, ModeLegacy} {
		c := newModeClient(mode)
		req, _ := http.NewRequest(http.MethodPost, "https://example.com/v2/chat/completions", nil)
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("mode=%v: ChatHeaders(nil auth) panic: %v", mode, r)
				}
			}()
			c.ChatHeaders(req, nil, "", ChatMeta{})
		}()
		if mode == ModeCapture {
			if got := req.Header.Get("X-Domain"); got == "" {
				t.Error("capture: nil auth 时 X-Domain 仍应发出（回落 chat base 主机名）")
			}
		}
		// 会话三件套与 nil auth 无关，必须齐全。
		if req.Header.Get("X-Conversation-ID") == "" {
			t.Errorf("mode=%v: nil auth 下仍应发会话头族", mode)
		}
	}
}

// TestChatDomainNeverEmpty chatDomain 在任何输入下都必须返回非空主机名。
func TestChatDomainNeverEmpty(t *testing.T) {
	c := &Client{ChatBaseCN: "https://copilot.tencent.com"}
	for _, a := range []*auth.Auth{
		nil,
		{UID: "u1"},
		{UID: "u2", Domain: "www.workbuddy.cn"},
		{UID: "u3", Domain: "https://weird.example/path"},
	} {
		if got := c.chatDomain(a); got == "" {
			t.Errorf("chatDomain(%v) 返回空串（该头必须恒发）", a)
		}
	}
	// global 恒为国际域。
	gl := &auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"}
	gl.BackfillRealm()
	if got := c.chatDomain(gl); got != "www.workbuddy.ai" {
		t.Errorf("global chatDomain = %q want www.workbuddy.ai", got)
	}
}
