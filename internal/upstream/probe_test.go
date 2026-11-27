// probe_test.go 目录取证（ProbeCatalog / 指纹 diff）的契约。
//
// 钉住的是"报告如实反映上游"：三路 UA 各自的状态与模型集、disabled 单独列、
// 企业端点按 agents[cli] + disabled 过滤、账号只留短 hash、指纹 diff 能看出增删。
package upstream

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// errProbeNotFound 传输层失败占位（模拟 DNS/连接错误）。
var errProbeNotFound = errors.New("probe: not found")

// probeRouteByUA 按 UA 前缀分发三路 v3（与 identity.go 的 UA 形态一致）：
// IDE 历史 UA = CodeBuddyIDE/…，CLI 历史 UA = CLI/…，其余算桌面路。
func probeRouteByUA(ua string) string {
	switch {
	case strings.HasPrefix(ua, "CodeBuddyIDE/"):
		return "ide"
	case strings.HasPrefix(ua, "CLI/"):
		return "cli"
	default:
		return "desktop"
	}
}

func TestProbeCatalogRoutes(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/console/enterprises/personal/models") {
			// 三条：一条在 cli 名单（可上架）、一条 disabled、一条不在 cli 名单（上游按面过滤）。
			return jsonResp(200, `{"code":0,"data":{
				"models":[{"id":"glm-5.3"},{"id":"glm-4.6","disabled":true},{"id":"ide-only"}],
				"agents":[{"name":"cli","models":["glm-5.3","glm-4.6"]}]}}`), nil
		}
		switch probeRouteByUA(r.Header.Get("User-Agent")) {
		case "ide":
			return jsonResp(400, `{"code":12403,"msg":"bad ua"}`), nil
		case "cli":
			return jsonResp(200, `{"code":0,"data":{"models":[{"id":"deepseek-v4-flash"}]}}`), nil
		default:
			return jsonResp(200, `{"code":0,"data":{"models":[
				{"id":"glm-5.3"},{"id":"hy3","disabled":true}
			],"agent":{},"mcp":{},"features":{}}}`), nil
		}
	})
	rep := c.ProbeCatalog(&auth.Auth{AccessToken: "at", UID: "uid-that-must-not-leak"})

	if rep.Realm != "cn" {
		t.Errorf("realm=%q want cn", rep.Realm)
	}
	if len(rep.AccountTag) != 8 || strings.Contains(rep.AccountTag, "uid-that-must-not-leak") {
		t.Errorf("账号指纹不该是原 UID: %q", rep.AccountTag)
	}
	if len(rep.Routes) != 4 {
		t.Fatalf("routes=%d want 4（三路 v3 + 企业端点）: %+v", len(rep.Routes), rep.Routes)
	}

	byLabel := map[string]RouteProbe{}
	for _, r := range rep.Routes {
		byLabel[r.Label] = r
	}

	desk := byLabel["desktop-UA"]
	if desk.Status != 200 || desk.Total != 2 || desk.Enabled != 1 {
		t.Errorf("desktop: %+v", desk)
	}
	if len(desk.Disabled) != 1 || desk.Disabled[0] != "hy3" {
		t.Errorf("desktop disabled=%v want [hy3]", desk.Disabled)
	}
	if got, want := strings.Join(desk.EnvelopeKeys, ","), "agent,features,mcp,models"; got != want {
		t.Errorf("envelope=%q want %q（信封改版要能看出来）", got, want)
	}

	ide := byLabel["IDE-UA"]
	if ide.Status != 400 || ide.Err == "" {
		t.Errorf("IDE 路 400 应带 err: %+v", ide)
	}

	cli := byLabel["CLI-UA"]
	if cli.Status != 200 || cli.Enabled != 1 || cli.Models[0] != "deepseek-v4-flash" {
		t.Errorf("CLI 路: %+v", cli)
	}

	ent := byLabel["enterprise"]
	if ent.Status != 200 || ent.Total != 3 || ent.Enabled != 1 {
		t.Errorf("enterprise: %+v（ide-only 不在 cli 名单，不该算可上架）", ent)
	}
	if len(ent.Disabled) != 1 || ent.Disabled[0] != "glm-4.6" {
		t.Errorf("enterprise disabled=%v want [glm-4.6]", ent.Disabled)
	}
}

func TestDiffFingerprintShowsModelChurn(t *testing.T) {
	old := CatalogProbeReport{Routes: []RouteProbe{
		{Label: "desktop-UA", Status: 200, Models: []string{"glm-5.2", "glm-5.3"}},
		{Label: "IDE-UA", Status: 200, Models: []string{"o4-mini"}},
	}}.Fingerprint()
	cur := CatalogProbeReport{Routes: []RouteProbe{
		{Label: "desktop-UA", Status: 200, Models: []string{"glm-5.3", "glm-5.4"}, Disabled: []string{"glm-4.6"}},
		{Label: "IDE-UA", Status: 404},
	}}.Fingerprint()

	diff := strings.Join(DiffFingerprint(old, cur), "\n")
	for _, want := range []string{
		"desktop-UA: + glm-5.4",
		"desktop-UA: - glm-5.2",
		"desktop-UA: disabled + glm-4.6",
		"IDE-UA: - o4-mini",
		"IDE-UA: status 200 → 404",
	} {
		if !strings.Contains(diff, want) {
			t.Errorf("diff 缺 %q:\n%s", want, diff)
		}
	}

	// 同一份指纹自比必须为空：否则"每天都说变了"，监控等于没有。
	if d := DiffFingerprint(cur, cur); len(d) != 0 {
		t.Errorf("自比应无差异: %v", d)
	}
}

// 无鉴权探测只用来判断路径在不在：状态码与 content-type 如实带回，不解析 body。
func TestProbePublicEndpoints(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/v3/config":
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: http.NoBody}, nil
		case "/v2/chat/completions":
			return &http.Response{StatusCode: 401, Header: http.Header{"Content-Type": []string{"text/html"}}, Body: http.NoBody}, nil
		}
		return nil, errProbeNotFound
	})
	got := c.ProbePublicEndpoints("https://chat.example", []string{"/v3/config", "/v2/chat/completions", "/nope"})
	if len(got) != 3 {
		t.Fatalf("got=%d want 3", len(got))
	}
	if got[0].Status != 200 || got[1].Status != 401 || got[1].ContentType != "text/html" {
		t.Errorf("状态/类型没有如实带回: %+v", got)
	}
	if got[2].Err == "" {
		t.Errorf("传输层失败应带 err: %+v", got[2])
	}
}

// TestLegacyCatalogProbeDisabledSkipsOldUAs 关闭 legacyCatalogProbe 后，
// FetchModels（CN）与 FetchGlobalModels（global）均不得发出 CodeBuddyIDE/ 或 CLI/2.63.2 旧 UA。
func TestLegacyCatalogProbeDisabledSkipsOldUAs(t *testing.T) {
	var mu sync.Mutex
	var seenUAs []string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		ua := r.Header.Get("User-Agent")
		mu.Lock()
		seenUAs = append(seenUAs, ua)
		mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/v3/config"):
			return jsonResp(200, `{"code":0,"data":{"models":[{"id":"glm-5.3","name":"GLM-5.3"}]}}`), nil
		case strings.HasSuffix(r.URL.Path, "/console/enterprises/personal/models"),
			strings.HasSuffix(r.URL.Path, "/v2/enterprises/personal/models"):
			return jsonResp(200, `{"code":0,"data":{"models":[{"id":"glm-5.3","name":"GLM-5.3"}],"agents":[{"name":"cli","models":["glm-5.3"]}]}}`), nil
		default:
			return jsonResp(404, `{"code":404}`), nil
		}
	})
	c.GlobalEnabled = true
	c.SetLegacyCatalogProbe(false)

	if _, err := c.FetchModels(&auth.Auth{AccessToken: "at", UID: "cn-1"}); err != nil {
		t.Fatalf("FetchModels(cn): %v", err)
	}
	if names := c.FetchGlobalModels(&auth.Auth{AccessToken: "at", UID: "gl-1", Domain: "www.workbuddy.ai"}); len(names) == 0 {
		t.Fatal("FetchGlobalModels(global) 返回空")
	}
	mu.Lock()
	uas := append([]string(nil), seenUAs...)
	mu.Unlock()
	for _, ua := range uas {
		if strings.HasPrefix(ua, "CodeBuddyIDE/") || strings.HasPrefix(ua, "CLI/") {
			t.Errorf("关闭 legacyCatalogProbe 后仍发出了旧探测 UA: %q", ua)
		}
	}
}

// TestModelsSequentialProbesAndSharedCache 验证：
// 1) FetchModels 对同一账号顺序探测企业端点与 /v3/config（并发峰值为 1，不再同毫秒并发）；
// 2) CNModelsCached 纯读缓存不打上游；
// 3) RefreshCNModels 在冷却窗口内防抖跳过重复探测。
func TestModelsSequentialProbesAndSharedCache(t *testing.T) {
	var cur, peak, calls atomic.Int32
	c := testClient(func(r *http.Request) (*http.Response, error) {
		n := cur.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		calls.Add(1)
		time.Sleep(10 * time.Millisecond)
		defer cur.Add(-1)
		switch {
		case strings.HasSuffix(r.URL.Path, "/v3/config"):
			return jsonResp(200, `{"code":0,"data":{"models":[{"id":"glm-5.3","name":"GLM-5.3"}]}}`), nil
		case strings.HasSuffix(r.URL.Path, "/console/enterprises/personal/models"):
			return jsonResp(200, `{"code":0,"data":{"models":[{"id":"glm-5.3","name":"GLM-5.3"}],"agents":[{"name":"cli","models":["glm-5.3"]}]}}`), nil
		default:
			return jsonResp(404, `{"code":404}`), nil
		}
	})
	c.SetLegacyCatalogProbe(false)
	acct := &auth.Auth{AccessToken: "at", UID: "cn-1"}

	// 冷缓存纯读不发请求。
	if cached, _ := c.CNModelsCached(time.Minute); len(cached) != 0 || calls.Load() != 0 {
		t.Fatalf("冷缓存不应发请求或非空: len=%d calls=%d", len(cached), calls.Load())
	}

	if _, err := c.RefreshCNModels(acct, time.Minute); err != nil {
		t.Fatalf("RefreshCNModels: %v", err)
	}
	if peak.Load() != 1 {
		t.Fatalf("FetchModels 峰值并发=%d want 1（顺序探测不得并发）", peak.Load())
	}
	firstCalls := calls.Load()
	if firstCalls != 2 {
		t.Fatalf("首次刷新调用次数=%d want 2", firstCalls)
	}

	// 冷却窗口内再次刷新应直接复用缓存，0 上游请求。
	if _, err := c.RefreshCNModels(acct, time.Minute); err != nil {
		t.Fatalf("cooldown RefreshCNModels: %v", err)
	}
	if calls.Load() != firstCalls {
		t.Fatalf("冷却期内再次刷新触发了上游请求: calls=%d want %d", calls.Load(), firstCalls)
	}
	if cached, at := c.CNModelsCached(time.Minute); len(cached) != 1 || at.IsZero() {
		t.Fatalf("CNModelsCached 异常: len=%d at=%v", len(cached), at)
	}
}
