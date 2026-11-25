package upstream

import (
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// uaCaptureTransport 记录出站请求的 User-Agent。
type uaCaptureTransport struct {
	ua *string
}

func (t uaCaptureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	*t.ua = r.Header.Get("User-Agent")
	return jsonResp(200, `{"code":0}`), nil
}

// 三个 UA 家族的期望值（CN 形态，版本取内置默认）。
const (
	cnChatUA    = "WorkBuddy/5.11.3 WorkBuddy/5.11.3 CLI/2.176.0"
	cnRPCUA     = "WorkBuddy/5.11.3 WorkBuddy/5.11.3 CLI/2.176.0"
	cnBillingUA = "axios/1.16.1"
)

// TestUserAgentPerPathFamilies 三条路径三族 UA：chat 走 SDK 形态、RPC 走自建
// fetch 形态、billing 走 axios 形态。三者不可互换——官方客户端就是三条不同的
// 代码路径，混用等于让同一个号同时自称三个客户端。
func TestUserAgentPerPathFamilies(t *testing.T) {
	cases := []struct {
		name   string
		call   func(c *Client, a *auth.Auth) error
		wantUA string
	}{
		{
			name: "chat",
			call: func(c *Client, a *auth.Auth) error {
				rc, status, _, err := c.ChatStream(a, []byte(`{"model":"glm-5.2","messages":[]}`), "", ChatMeta{})
				if status != 200 {
					t.Fatalf("chat status=%d", status)
				}
				if rc != nil {
					rc.Close()
				}
				return err
			},
			wantUA: cnChatUA,
		},
		{
			name: "billing_report",
			call: func(c *Client, a *auth.Auth) error {
				return c.ReportChatActivity(a, "cid", "")
			},
			wantUA: cnBillingUA,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var ua string
			c := &Client{
				HTTP:          &http.Client{Transport: uaCaptureTransport{ua: &ua}},
				ChatHTTP:      &http.Client{Transport: uaCaptureTransport{ua: &ua}},
				ChatBaseCN:    "https://chat.example",
				BillingBaseCN: "https://billing.example",
			}
			if err := tc.call(c, &auth.Auth{AccessToken: "at", UID: "u1"}); err != nil {
				t.Fatalf("call: %v", err)
			}
			if ua != tc.wantUA {
				t.Errorf("UA = %q want %q", ua, tc.wantUA)
			}
		})
	}
}

// TestUserAgentNeverGoDefaultUA 全路径出站 UA 不得为空、不得是 Go 默认串。
// 这是最硬的一条：空 UA 或 `Go-http-client/1.1` 是最容易被识别的网关特征。
func TestUserAgentNeverGoDefaultUA(t *testing.T) {
	check := func(t *testing.T, path, ua string) {
		t.Helper()
		if ua == "" {
			t.Errorf("%s: UA 为空（客户端从不发空 UA）", path)
		}
		if strings.Contains(ua, "Go-http-client") {
			t.Errorf("%s: UA=%q 是 Go 默认串", path, ua)
		}
	}

	var lastUA string
	rec := rtFunc(func(r *http.Request) (*http.Response, error) {
		lastUA = r.Header.Get("User-Agent")
		return jsonResp(200, `{"code":0,"data":{"models":[],"agents":[]}}`), nil
	})
	c := &Client{
		HTTP:          &http.Client{Transport: rec},
		ChatHTTP:      &http.Client{Transport: rec},
		ChatBaseCN:    "https://chat.example",
		BillingBaseCN: "https://billing.example",
		WebBaseCN:     "https://www.workbuddy.cn",
	}
	a := &auth.Auth{AccessToken: "at", UID: "u1", RefreshToken: "rt"}

	// chat
	if rc, _, _, err := c.ChatStream(a, []byte(`{"model":"m","messages":[]}`), "", ChatMeta{}); err == nil && rc != nil {
		rc.Close()
	}
	check(t, "chat", lastUA)

	// refresh
	if err := c.RefreshToken(a); err == nil {
		check(t, "refresh", lastUA)
	}

	// billing
	if _, _, err := c.UserResource(a); err == nil {
		check(t, "billing", lastUA)
	}

	// desktop report（此前完全没有 UA）
	if err := c.ReportDesktopEvent(a, DesktopEvent{"eventCode": "x"}); err == nil {
		check(t, "desktop report", lastUA)
	}

	// 开学季（此前完全没有 UA）
	if err := c.schoolJSON(a, http.MethodGet, "/x", nil, nil); err == nil {
		check(t, "school", lastUA)
	}
}

// TestUserAgentExplicitOverrideAllPaths 显式 config user_agent 在所有路径上生效。
func TestUserAgentExplicitOverrideAllPaths(t *testing.T) {
	const ua = "CustomAgent/1"
	var lastUA string
	rec := rtFunc(func(r *http.Request) (*http.Response, error) {
		lastUA = r.Header.Get("User-Agent")
		return jsonResp(200, `{"code":0}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1", RefreshToken: "rt"}
	c := &Client{
		HTTP:          &http.Client{Transport: rec},
		ChatHTTP:      &http.Client{Transport: rec},
		ChatBaseCN:    "https://chat.example",
		BillingBaseCN: "https://billing.example",
		UserAgent:     ua,
	}

	if rc, _, _, err := c.ChatStream(a, []byte(`{"model":"m","messages":[]}`), "", ChatMeta{}); err == nil && rc != nil {
		rc.Close()
	}
	if lastUA != ua {
		t.Errorf("chat UA = %q want %q", lastUA, ua)
	}
	c.HTTP.Transport = rtFunc(func(r *http.Request) (*http.Response, error) {
		lastUA = r.Header.Get("User-Agent")
		return jsonResp(200, `{"code":0,"data":{"accessToken":"nat","refreshToken":"nrt"}}`), nil
	})
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if lastUA != ua {
		t.Errorf("refresh UA = %q want %q", lastUA, ua)
	}
}

// TestClientVersionPerRealm 版本号按 realm 取内置默认（两个发行档不同）。
func TestClientVersionPerRealm(t *testing.T) {
	c := &Client{}
	cn := &auth.Auth{UID: "u1", Domain: "copilot.tencent.com"}
	cn.BackfillRealm()
	if got, want := c.clientVersion(cn), defaultClientVersionCN; got != want {
		t.Errorf("cn clientVersion = %q want %q", got, want)
	}
	if got, want := c.cliVersion(cn), defaultCliVersionCN; got != want {
		t.Errorf("cn cliVersion = %q want %q", got, want)
	}

	gl := &auth.Auth{UID: "u2", Domain: "www.workbuddy.ai"}
	gl.BackfillRealm()
	if got, want := c.clientVersion(gl), defaultClientVersionGlobal; got != want {
		t.Errorf("global clientVersion = %q want %q", got, want)
	}
	if got, want := c.cliVersion(gl), defaultCliVersionGlobal; got != want {
		t.Errorf("global cliVersion = %q want %q", got, want)
	}

	// 显式配置覆盖两域。
	c.ClientVersion = "9.9.9"
	c.CliVersion = "1.1.1"
	if got := c.clientVersion(gl); got != "9.9.9" {
		t.Errorf("override clientVersion = %q want 9.9.9", got)
	}
	if got := c.cliVersion(cn); got != "1.1.1" {
		t.Errorf("override cliVersion = %q want 1.1.1", got)
	}
}

// TestV3ConfigUAIsTwoSegment /v3/config 的 UA 是两段单品牌形态，
// 不能再是旧版 IDE 客户端串（WorkBuddy 分发包里不存在那两个串）。
func TestV3ConfigUAIsTwoSegment(t *testing.T) {
	c := &Client{}
	a := &auth.Auth{UID: "u1", Domain: "copilot.tencent.com"}
	a.BackfillRealm()
	got := c.v3ConfigUA(a)
	if want := "WorkBuddy/5.11.3 CLI/2.176.0"; got != want {
		t.Errorf("v3ConfigUA = %q want %q", got, want)
	}
	if strings.Contains(got, "CodeBuddy") {
		t.Errorf("v3ConfigUA = %q 仍含旧版 IDE 客户端品牌串", got)
	}
}
