// rate_catalog_test.go 牌价静态兜底表守门测试。
//
// 口径：远端 credits 永远优先；远端缺失才用 2026-11-05 官方客户端快照价；
// 两者皆无返回空串（不编造）。静态表只服务展示，不参与选号/计费。
package upstream

import (
	"strings"
	"testing"
)

// TestRateCatalogWellFormed 兜底表条目必须是非空的 credits 原文。
func TestRateCatalogWellFormed(t *testing.T) {
	tables := map[string]map[string]string{"cn": cnRateFallback, "global": globalRateFallback}
	for realm, table := range tables {
		if len(table) == 0 {
			t.Fatalf("%s 牌价兜底表为空", realm)
		}
		for id, rate := range table {
			if strings.TrimSpace(id) == "" || strings.TrimSpace(rate) == "" {
				t.Errorf("%s 存在空条目: id=%q rate=%q", realm, id, rate)
			}
		}
	}
	// 11-05 快照的 19 个 CLI 模型都应能给出参考价（remote 缺失时）。
	for _, id := range []string{
		"fast-model", "balanced-model", "deep-model", "hy4free", "hy4std", "hy3", "hy3-x",
		"deepseek-v4.1-flash", "glm-5.3", "glm-5.3-flash", "glm-5.2", "space-bunny",
		"minimax-m3.1-flash", "kimi-k3.1-pro", "kimi-k3-1", "kimi-k2.8-preview",
		"deepseek-v4-pro", "step-5-preview", "mimo-v2.6-pro",
	} {
		if got := CreditsListing("cn", id, ""); got == "" {
			t.Errorf("CN 快照模型 %s 缺牌价兜底", id)
		}
	}
}

// TestCreditsListingPrecedence 远端优先、缺省落静态、未知返回空、realm 隔离。
func TestCreditsListingPrecedence(t *testing.T) {
	cases := []struct {
		name   string
		realm  string
		model  string
		remote string
		want   string
	}{
		{name: "远端优先", realm: "cn", model: "hy4free", remote: "x0.99", want: "x0.99"},
		{name: "远端带空格", realm: "cn", model: "hy4free", remote: "  x0.88  ", want: "x0.88"},
		{name: "CN 静态兜底", realm: "cn", model: "kimi-k3.1-pro", want: "x0.97"},
		{name: "global 静态兜底", realm: "global", model: "deepseek-v4.1-flash", want: "x0.11"},
		{name: "未知模型不编造", realm: "cn", model: "no-such-model", want: ""},
		{name: "CN 专有不污染 global", realm: "global", model: "space-bunny", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CreditsListing(tc.realm, tc.model, tc.remote); got != tc.want {
				t.Errorf("CreditsListing(%q,%q,%q)=%q want %q", tc.realm, tc.model, tc.remote, got, tc.want)
			}
		})
	}
}
