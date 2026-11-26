// relogin_test.go 面板单号运维：重登（就地改写 + uid 校验 + 自动解禁）、手动刷新、
// 导入默认不覆盖、移除需 force。
//
// 这些端点存在的理由：真吊销之后**没有代码能救回 RT**，唯一出路是人再扫一次码。
// 那就必须让这条路足够便宜且不出错——扫错号不许动文件、重登不许冲掉 device_token、
// 重登成功必须自动解禁（否则「重登成功了但池子里还是不动」）。
package panel

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// stubRT 测试用 HTTP transport。
type stubRT func(*http.Request) (*http.Response, error)

func (f stubRT) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonHTTP(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// loginStub 拦截设备授权三端点（login.go 的 loginHTTP 是包级 var，可整体替换）。
func loginStub(t *testing.T, accountUID string) *int32 {
	t.Helper()
	var acctCalls int32
	old := loginHTTP
	loginHTTP = &http.Client{Transport: stubRT(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(r.URL.Path, "/plugin/auth/state"):
			return jsonHTTP(200, `{"code":0,"data":{"state":"s1","authUrl":"https://auth.example/x"}}`), nil
		case strings.Contains(r.URL.Path, "/plugin/auth/token"):
			return jsonHTTP(200, `{"code":0,"data":{"accessToken":"at-new","refreshToken":"rt-new","expiresIn":3600,"domain":""}}`), nil
		case strings.Contains(r.URL.Path, "/plugin/login/account"):
			atomic.AddInt32(&acctCalls, 1)
			return jsonHTTP(200, `{"code":0,"data":{"uid":"`+accountUID+`","nickname":"n"}}`), nil
		}
		return jsonHTTP(404, `{"code":404,"msg":"unexpected"}`), nil
	})}
	t.Cleanup(func() { loginHTTP = old })
	return &acctCalls
}

// billingStub 让签到/余额等顺带的调用安静返回成功信封。
func billingStub() *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: stubRT(func(r *http.Request) (*http.Response, error) {
			return jsonHTTP(200, `{"code":0,"msg":"ok","data":{}}`), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

// writeAuthFile 落一份带 device_token 与自定义键的嵌套形 auth 文件（模拟用户手写/导入）。
func writeAuthFile(t *testing.T, dir, name, uid string) string {
	t.Helper()
	fp := filepath.Join(dir, name)
	doc := `{"auth":{"accessToken":"at-old","refreshToken":"rt-old","expiresAt":1,"domain":""},` +
		`"account":{"uid":"` + uid + `","nickname":"old"},"device_token":"dev-keep","proxy":"line-a"}`
	if err := os.WriteFile(fp, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return fp
}

func newReloginPanel(t *testing.T, uid, fp string) (*Panel, *pool.Pool) {
	t.Helper()
	p := pool.New("")
	p.Add(&auth.Auth{UID: uid, AccessToken: "at-old", RefreshToken: "rt-old", ExpiresAt: 1,
		Nickname: "old", FilePath: fp, DeviceToken: "dev-keep"})
	// 先造出「已判死、待重登」的状态。
	for i := 0; i < pool.SessionDeadThreshold(); i++ {
		p.NoteSessionDead(uid)
	}
	pn := New(Config{Pool: p, Upstream: billingStub(), AuthDir: filepath.Dir(fp)})
	return pn, p
}

// TestReloginRewritesInPlaceAndRecovers 重登：就地写同一份文件、保留 device_token/proxy、
// 成功后自动解禁并清掉 RT 判死标记；不会在 auths 里多出一份同 uid 的文件。
func TestReloginRewritesInPlaceAndRecovers(t *testing.T) {
	dir := t.TempDir()
	fp := writeAuthFile(t, dir, "workbuddy-custom-name.json", "u1")
	pn, p := newReloginPanel(t, "u1", fp)
	loginStub(t, "u1")

	// 1) 发起重登：realm 跟账号走、返回 state。
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, httptest.NewRequest("POST", "/panel/api/accounts/u1/relogin/start", strings.NewReader("{}")))
	if rec.Code != 200 {
		t.Fatalf("relogin start: code=%d body=%s", rec.Code, rec.Body)
	}
	var started map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	if started["uid"] != "u1" {
		t.Fatalf("start 应回显目标 uid: %v", started)
	}

	// 2) 轮询完成 → 就地改写 + 解禁。
	rec = httptest.NewRecorder()
	pn.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/login/poll?state=s1", nil))
	if rec.Code != 200 {
		t.Fatalf("poll: code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"relogin":true`) {
		t.Errorf("响应应标 relogin=true: %s", rec.Body)
	}

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"at-new", "rt-new", "dev-keep", "line-a"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("就地改写后缺 %q: %s", want, raw)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "workbuddy-u1.json")); err == nil {
		t.Error("不应在 auths 里新建同 uid 的第二份文件")
	}
	st, _ := p.Status("u1")
	if st.Disabled || st.NeedsRelogin || st.RefreshTokenDead {
		t.Fatalf("重登成功应解禁并清 RT 判死标记: %+v", st)
	}
}

// TestReloginMismatchedAccountRejected 扫到别的号：一个字节都不许动（否则等于又加了一个号）。
func TestReloginMismatchedAccountRejected(t *testing.T) {
	dir := t.TempDir()
	fp := writeAuthFile(t, dir, "workbuddy-custom.json", "u1")
	pn, _ := newReloginPanel(t, "u1", fp)
	loginStub(t, "u2") // 扫回来的是 u2

	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, httptest.NewRequest("POST", "/panel/api/accounts/u1/relogin/start", strings.NewReader("{}")))
	if rec.Code != 200 {
		t.Fatalf("start: code=%d", rec.Code)
	}
	rec = httptest.NewRecorder()
	pn.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/login/poll?state=s1", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("uid 不一致应 409，got %d body=%s", rec.Code, rec.Body)
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "at-old") || strings.Contains(string(raw), "at-new") {
		t.Fatalf("目标文件不得被改动: %s", raw)
	}
}

// TestAccountRefreshReportsNeedsRelogin 手动刷新：上游判死 → needs_relogin=true，
// 且 RT 判死标记落库（面板据此挂黄标）。
func TestAccountRefreshReportsNeedsRelogin(t *testing.T) {
	dir := t.TempDir()
	fp := writeAuthFile(t, dir, "workbuddy-u1.json", "u1")
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at-old", RefreshToken: "rt-old",
		ExpiresAt: time.Now().Add(time.Hour).Unix(), FilePath: fp})
	up := &upstream.Client{
		HTTP: &http.Client{Transport: stubRT(func(r *http.Request) (*http.Response, error) {
			return jsonHTTP(401, `{"code":12153,"msg":"invalid_grant"}`), nil
		})},
		ChatBaseCN: "https://fake.example", BillingBaseCN: "https://fake.example",
	}
	pn := New(Config{Pool: p, Upstream: up, AuthDir: dir})

	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, httptest.NewRequest("POST", "/panel/api/accounts/u1/refresh", nil))
	if rec.Code != 200 {
		t.Fatalf("refresh: code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["ok"] != false || resp["needs_relogin"] != true {
		t.Fatalf("应回 needs_relogin=true: %v", resp)
	}
	if resp["access_usable"] != true {
		t.Errorf("AT 仍有效时应回 access_usable=true: %v", resp)
	}
	if st, _ := p.Status("u1"); !st.RefreshTokenDead {
		t.Errorf("应记下 RT 判死标记: %+v", st)
	}
}

// TestImportSkipsExistingByDefault 导入默认不覆盖已有账号（过期导出会把好号写成死号）。
func TestImportSkipsExistingByDefault(t *testing.T) {
	dir := t.TempDir()
	fp := writeAuthFile(t, dir, "workbuddy-u1.json", "u1")
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at-old", RefreshToken: "rt-old", ExpiresAt: 1, FilePath: fp})
	pn := New(Config{Pool: p, Upstream: billingStub(), AuthDir: dir})

	body, ct := cockpitForm(t, []map[string]any{{
		"uid": "u1", "access_token": "at-imported", "refresh_token": "rt-imported",
		"expires_at": time.Now().Add(time.Hour).UnixMilli(),
	}}, "")
	req := httptest.NewRequest("POST", "/panel/api/import/cockpit", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("import: code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"skipped":1`) {
		t.Fatalf("已存在应跳过: %s", rec.Body)
	}
	raw, _ := os.ReadFile(fp)
	if strings.Contains(string(raw), "at-imported") {
		t.Fatalf("默认不得覆盖现有凭证: %s", raw)
	}

	// overwrite=1 才覆盖，且保留 device_token/proxy。
	body, ct = cockpitForm(t, []map[string]any{{
		"uid": "u1", "access_token": "at-imported", "refresh_token": "rt-imported",
		"expires_at": time.Now().Add(time.Hour).UnixMilli(),
	}}, "1")
	req = httptest.NewRequest("POST", "/panel/api/import/cockpit", body)
	req.Header.Set("Content-Type", ct)
	rec = httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `"overwritten":1`) {
		t.Fatalf("overwrite=1 应覆盖: %s", rec.Body)
	}
	raw, _ = os.ReadFile(fp)
	for _, want := range []string{"at-imported", "dev-keep", "line-a"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("覆盖后应保留 %q: %s", want, raw)
		}
	}
}

// TestRemoveRequiresForceForHealthyAccount 健康账号移除需 force（删除不可逆）。
func TestRemoveRequiresForceForHealthyAccount(t *testing.T) {
	dir := t.TempDir()
	fp := writeAuthFile(t, dir, "workbuddy-u1.json", "u1")
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt",
		ExpiresAt: time.Now().Add(time.Hour).Unix(), FilePath: fp})
	pn := New(Config{Pool: p, Upstream: billingStub(), AuthDir: dir})

	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, httptest.NewRequest("POST", "/panel/api/accounts/u1/remove", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("健康账号无 force 应 409，got %d body=%s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(fp); err != nil {
		t.Fatal("被拒时不得删文件")
	}

	rec = httptest.NewRecorder()
	pn.ServeHTTP(rec, httptest.NewRequest("POST", "/panel/api/accounts/u1/remove?force=1", nil))
	if rec.Code != 200 {
		t.Fatalf("force=1 应删除，got %d body=%s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(fp); !os.IsNotExist(err) {
		t.Fatal("force=1 后文件应被删除")
	}
}

// cockpitForm 构造 cockpit 导入的 multipart 请求体；overwrite 非空时带上该字段。
func cockpitForm(t *testing.T, accounts []map[string]any, overwrite string) (*bytes.Buffer, string) {
	t.Helper()
	raw, err := json.Marshal(accounts)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("file", "accounts.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if overwrite != "" {
		_ = w.WriteField("overwrite", overwrite)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, w.FormDataContentType()
}
