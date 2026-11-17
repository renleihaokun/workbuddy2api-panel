package panel

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pricing"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// writePriceFile 写一份测试用牌价文件（glm-5.3 = 8 / 28 / 2 元）。
func writePriceFile(t *testing.T, dir string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"version":  1,
		"updated":  "2026-11-17",
		"currency": "CNY",
		"unit":     "per_1m_tokens",
		"models": map[string]any{
			"glm-5.3": map[string]any{"input": 8, "output": 28, "cache_hit": 2},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "prices.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// /panel/api/usage 必须把「已省」折算透出到 totals / pricing 元信息两处；
// 未接线牌价表时 enabled=false 且金额为零（而不是 500 / 501）。
func TestUsageHandlerPricing(t *testing.T) {
	rec := usage.New("")
	rec.Add(time.Now(), "cn", "u1", "glm-5.3", usage.Delta{
		PromptTokens: 1_000_000, HasPromptTokens: true,
		CompletionTokens: 100_000, HasCompletion: true,
		CacheHitTokens: 500_000, CacheMissTokens: 500_000, HasCacheTokens: true,
	}, true)
	rec.Add(time.Now(), "cn", "u1", "no-price-model", usage.Delta{
		PromptTokens: 500, HasPromptTokens: true, CompletionTokens: 500, HasCompletion: true,
	}, true)

	prices := pricing.NewStore(writePriceFile(t, t.TempDir()))
	p := New(Config{Version: "test", APIKey: "k", Usage: rec, Pool: pool.New(""), Pricing: prices})

	get := func(url string) map[string]any {
		t.Helper()
		req := httptest.NewRequest("GET", url, nil)
		req.Header.Set("Authorization", "Bearer k")
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("%s -> %d %s", url, rr.Code, rr.Body)
		}
		var out map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	out := get("/panel/api/usage?hours=24")
	totals := out["totals"].(map[string]any)
	if got := totals["saved_yuan"].(float64); got < 7.79 || got > 7.81 {
		t.Fatalf("totals.saved_yuan=%v want 7.8", got)
	}
	if got := totals["priced_tokens"].(float64); got != 1_100_000 {
		t.Fatalf("priced_tokens=%v want 1100000", got)
	}
	if got := totals["unpriced_tokens"].(float64); got != 1000 {
		t.Fatalf("unpriced_tokens=%v want 1000", got)
	}
	pinfo := out["pricing"].(map[string]any)
	if pinfo["enabled"] != true || pinfo["currency"] != "CNY" || pinfo["unit"] != "per_1m_tokens" {
		t.Fatalf("pricing meta=%+v", pinfo)
	}
	if pinfo["source"] != "seed+file" || pinfo["updated"] != "2026-11-17" {
		t.Fatalf("pricing source/updated=%v/%v", pinfo["source"], pinfo["updated"])
	}
	if got := pinfo["saved_yuan"].(float64); got < 7.79 || got > 7.81 {
		t.Fatalf("pricing.saved_yuan=%v want 7.8", got)
	}
	list := pinfo["unpriced_models"].([]any)
	if len(list) != 1 || list[0] != "no-price-model" {
		t.Fatalf("unpriced_models=%v", list)
	}

	// 模型行带单价摘要（面板在模型名下展示「入 8 · 出 28 · 缓存 2」这类文字）。
	byModel := out["by_model"].([]any)
	var note, unpricedNote string
	for _, raw := range byModel {
		row := raw.(map[string]any)
		if row["key"] == "glm-5.3" {
			note, _ = row["price_note"].(string)
		}
		if row["key"] == "no-price-model" {
			unpricedNote, _ = row["price_note"].(string)
		}
	}
	if !strings.Contains(note, "入 8") {
		t.Fatalf("glm-5.3 price_note=%q, want 含「入 8」", note)
	}
	if unpricedNote != "" {
		t.Fatalf("未定价行不应有 price_note: %q", unpricedNote)
	}

	// 未接线：仍 200，但明确 enabled=false、金额为零。
	p2 := New(Config{Version: "test", APIKey: "k", Usage: rec, Pool: pool.New("")})
	req := httptest.NewRequest("GET", "/panel/api/usage?hours=24", nil)
	req.Header.Set("Authorization", "Bearer k")
	rr := httptest.NewRecorder()
	p2.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("未接线牌价表应 200，得到 %d", rr.Code)
	}
	var out2 map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out2); err != nil {
		t.Fatal(err)
	}
	p2info := out2["pricing"].(map[string]any)
	if p2info["enabled"] != false || p2info["saved_yuan"].(float64) != 0 {
		t.Fatalf("未接线时 pricing=%+v", p2info)
	}
}

// 用量页前端必须真有「已省」区块与「官方价」列：漏了页面照样渲染得出来，
// 所以在测试里钉死这些标记。
func TestUsagePricingFrontendWiring(t *testing.T) {
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, must := range []string{
		`id="usSaveNum"`, `id="usSaveRows"`, `id="usSaveMore"`, `id="usSaveFoot"`,
		`id="usSaveCovBar"`, "已省",
	} {
		if !strings.Contains(string(html), must) {
			t.Fatalf("index.html 缺 %q（用量页已省区块）", must)
		}
	}
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, must := range []string{"function saveRowHtml(", "function saveRowsHtml(", "function renderSaveStats(", "renderSaveStats(usageData)", "function fmtMoney(", "function usCostCell(", ">官方价</th>"} {
		if !strings.Contains(string(js), must) {
			t.Fatalf("app.js 缺 %q", must)
		}
	}
}
