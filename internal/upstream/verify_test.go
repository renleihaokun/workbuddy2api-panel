package upstream

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestClassifyVerifyError 三态判据表：refresh 的成功/明确拒绝/其他，各归各位。
func TestClassifyVerifyError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want SessionVerdict
	}{
		{"refresh 成功", nil, SessionAlive},
		{"401 业务信封 12153", &Error{Kind: ErrSessionDead, Status: 401, Msg: `{"code":12153,"msg":"invalid_grant"}`}, SessionDead},
		{"400 invalid_grant", &Error{Kind: ErrClient, Status: 400, Msg: `{"error":"invalid_grant"}`}, SessionDead},
		{"403 业务信封", &Error{Kind: ErrAccountFault, Status: 403, Msg: `{"code":11140,"msg":"request illegal"}`}, SessionDead},
		{"429 限流", &Error{Kind: ErrSoftRate, Status: 429, Msg: `{"code":1,"msg":"rate limit"}`}, SessionInconclusive},
		{"408 超时", &Error{Kind: ErrClient, Status: 408}, SessionInconclusive},
		{"500 上游故障", &Error{Kind: ErrServer, Status: 500}, SessionInconclusive},
		{"网关 401 HTML", &Error{Kind: ErrClient, Status: 401, Msg: "<html><head><title>401 Authorization Required</title></head></html>"}, SessionInconclusive},
		{"WAF 403 HTML", &Error{Kind: ErrWafBlock, Status: 403, Msg: "<html>blocked</html>"}, SessionInconclusive},
		{"传输层错误", errors.New("connection refused"), SessionInconclusive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyVerifyError(tc.err); got != tc.want {
				t.Errorf("classifyVerifyError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestVerifySessionAlivePersistsTokens 求证=活时必须落盘：上游已轮换 RT，不写回则
// 重启回滚到旧票 —— 本来没死的号会被自己弄死（同类项目踩过的坑）。
func TestVerifySessionAlivePersistsTokens(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u1.json")
	seed := `{"auth":{"accessToken":"at-old","refreshToken":"rt-old","expiresAt":1},` +
		`"account":{"uid":"u1","nickname":"n"},"device_token":"dev-1"}`
	if err := os.WriteFile(fp, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"msg":"ok","data":{"accessToken":"at-new","refreshToken":"rt-new","expiresIn":3600}}`), nil
	})
	a := &auth.Auth{UID: "u1", AccessToken: "at-old", RefreshToken: "rt-old", ExpiresAt: 1,
		FilePath: fp, DeviceToken: "dev-1"}

	if v := c.VerifySession(a); v != SessionAlive {
		t.Fatalf("verdict=%v want alive", v)
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"at-new", "rt-new", "dev-1"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("落盘内容缺 %q：%s", want, raw)
		}
	}
}

// TestVerifySessionThrottlesPerAccount 同一账号节流窗口内只打一次上游，复用上次结论。
func TestVerifySessionThrottlesPerAccount(t *testing.T) {
	var calls int32
	c := testClient(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return &http.Response{
			StatusCode: 401,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"code":12153,"msg":"invalid_grant"}`)),
		}, nil
	})
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1}

	if v := c.VerifySession(a); v != SessionDead {
		t.Fatalf("第一次 verdict=%v want dead", v)
	}
	if v := c.VerifySession(a); v != SessionDead {
		t.Fatalf("窗口内 verdict=%v want dead（复用结论）", v)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("窗口内 refresh calls=%d want 1", got)
	}
	// 窗口过去后重新求证（节流只省调用，不改变判据）。
	c.SetSessionVerifyInterval(time.Nanosecond)
	time.Sleep(2 * time.Millisecond)
	if v := c.VerifySession(a); v != SessionDead {
		t.Fatalf("窗口过后 verdict=%v want dead", v)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("窗口过后 refresh calls=%d want 2", got)
	}
}

// TestVerifySessionInconclusiveOnGateway401 网关 HTML 401（issue #215 那种形态）：
// refresh 端点被同一层边缘拦下，没有判词 —— 不能判死。
func TestVerifySessionInconclusiveOnGateway401(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 401,
			Header:     http.Header{"Content-Type": []string{"text/html"}},
			Body:       io.NopCloser(strings.NewReader(`<html><head><title>401 Authorization Required</title></head><body>openresty</body></html>`)),
		}, nil
	})
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1}
	if v := c.VerifySession(a); v != SessionInconclusive {
		t.Fatalf("verdict=%v want inconclusive", v)
	}
}

// TestVerifySessionWithoutRefreshToken 没有 RT 就无法求证：不发请求、结论无判词。
func TestVerifySessionWithoutRefreshToken(t *testing.T) {
	var calls int32
	c := testClient(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return nil, errors.New("不应发请求")
	})
	a := &auth.Auth{UID: "u1", AccessToken: "at"}
	if v := c.VerifySession(a); v != SessionInconclusive {
		t.Fatalf("verdict=%v want inconclusive", v)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("无 RT 不应发请求，calls=%d", got)
	}
	if v := c.VerifySession(nil); v != SessionInconclusive {
		t.Fatalf("nil auth verdict=%v want inconclusive", v)
	}
}

// TestAccessUsableWithoutRefresh 判据只看 AT 是否还有效（不看上游文案/azp）。
func TestAccessUsableWithoutRefresh(t *testing.T) {
	if !AccessUsableWithoutRefresh(&auth.Auth{ExpiresAt: time.Now().Add(time.Hour).Unix()}) {
		t.Error("AT 未过期应判可用")
	}
	if AccessUsableWithoutRefresh(&auth.Auth{ExpiresAt: time.Now().Add(-time.Minute).Unix()}) {
		t.Error("AT 已过期应判不可用")
	}
	if AccessUsableWithoutRefresh(&auth.Auth{}) {
		t.Error("无 expiry 信息应判不可用（保守）")
	}
	if AccessUsableWithoutRefresh(nil) {
		t.Error("nil 应判不可用")
	}
}

// TestIsGatewayUnauthorized 只认「401 + 无业务信封」；带 code/msg 的业务 401 不算。
func TestIsGatewayUnauthorized(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{401, "<html>401 Authorization Required</html>", true},
		{401, "", true},
		{401, "Unauthorized", true},
		{401, `{"code":12153,"msg":"invalid_grant"}`, false},
		{403, "<html>blocked</html>", false},
		{200, "", false},
	}
	for _, tc := range cases {
		if got := IsGatewayUnauthorized(tc.status, tc.body); got != tc.want {
			t.Errorf("IsGatewayUnauthorized(%d, %q) = %v, want %v", tc.status, tc.body, got, tc.want)
		}
	}
}
