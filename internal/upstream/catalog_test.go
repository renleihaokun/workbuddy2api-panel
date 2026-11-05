// catalog_test.go 模型与档位静态兜底表的守门测试。
//
// 三类断言：
//   - 表结构自检：efforts 非空、default ∈ efforts、context/maxOutput 合法；
//   - 表与种子一致：context_catalog.go 的 contextCapFallback 与内嵌 model.json 逐条相等；
//   - 与官方客户端快照一致：2026-11-05 WorkBuddy 客户端 product-config 的 19 个
//     CLI 模型（testdata/cn_roster_20261105.json）逐项对照，防止手工改表漂移。
package upstream

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// rosterSnapshot 官方客户端 product-config 的精简快照（只含模型能力与价格字段）。
type rosterSnapshot struct {
	CapturedAt  string            `json:"captured_at"`
	Source      string            `json:"source"`
	Models      []rosterModel     `json:"models"`
	ExtraModels []rosterModel     `json:"extra_models"`
	Promotions  []rosterPromotion `json:"promotions"`
}

// rosterModel 单个模型的能力快照。
type rosterModel struct {
	ID              string   `json:"id"`
	CLI             bool     `json:"cli"`
	ContextLength   int64    `json:"context_length"`
	MaxOutputTokens int64    `json:"max_output_tokens"`
	Efforts         []string `json:"efforts"`
	DefaultEffort   string   `json:"default_effort"`
	Credits         string   `json:"credits"`
}

// rosterPromotion 促销条目快照（factor 缺失 = badge-only，与线上 v3ModelPromotion 同构）。
type rosterPromotion struct {
	ID                string              `json:"id"`
	Enabled           bool                `json:"enabled"`
	ModelIDs          []string            `json:"model_ids"`
	Badge             string              `json:"badge"`
	Factor            *float64            `json:"factor"`
	DiscountedCredits string              `json:"discounted_credits"`
	Daily             []rosterPromoWindow `json:"daily"`
	Timezone          string              `json:"timezone"`
	Note              string              `json:"note"`
	Priority          int                 `json:"priority"`
}

type rosterPromoWindow struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// loadRosterSnapshot 读取并解析官方客户端快照 fixture。
func loadRosterSnapshot(t *testing.T) rosterSnapshot {
	t.Helper()
	raw, err := os.ReadFile("testdata/cn_roster_20261105.json")
	if err != nil {
		t.Fatalf("读取快照 fixture 失败: %v", err)
	}
	var snap rosterSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("解析快照 fixture 失败: %v", err)
	}
	if snap.CapturedAt == "" || len(snap.Models) == 0 {
		t.Fatalf("快照 fixture 缺 captured_at/models: %+v", snap)
	}
	return snap
}

// equalStrings 有序比较两个字符串切片（档位顺序是产品语义，不能用集合比较）。
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestEffortCatalogWellFormed 两张静态表的条目自检：不允许空模型/空档位/
// default 不在 efforts 内的脏数据（default 防御在 EffortListing 里也有，但表本身应自洽）。
func TestEffortCatalogWellFormed(t *testing.T) {
	tables := map[string]map[string]effortCap{
		"cn":     cnEffortFallback,
		"global": globalEffortFallback,
	}
	for realm, table := range tables {
		if len(table) == 0 {
			t.Fatalf("%s 档位表为空", realm)
		}
		for id, cap := range table {
			if strings.TrimSpace(id) == "" {
				t.Errorf("%s 表存在空模型 id", realm)
			}
			if len(cap.efforts) == 0 {
				t.Errorf("%s/%s efforts 为空", realm, id)
			}
			seen := map[string]bool{}
			for _, e := range cap.efforts {
				if strings.TrimSpace(e) == "" {
					t.Errorf("%s/%s 存在空档位", realm, id)
				}
				if seen[e] {
					t.Errorf("%s/%s 档位重复: %q", realm, id, e)
				}
				seen[e] = true
			}
			if cap.defaultEffort != "" && !containsEffort(cap.efforts, cap.defaultEffort) {
				t.Errorf("%s/%s default=%q 不在 efforts=%v 内", realm, id, cap.defaultEffort, cap.efforts)
			}
		}
	}
}

// TestContextCatalogAndSeedParity context_catalog.go 与内嵌 model.json 必须逐条相等：
// 两者是同一份知识表的两级表示（第 2/3 级查找），任何一侧漏改都会造成回升到 1M 或省略。
func TestContextCatalogAndSeedParity(t *testing.T) {
	raw, err := modelSeedFS.ReadFile("model.json")
	if err != nil {
		t.Fatalf("读取内嵌 model.json 失败: %v", err)
	}
	var seed map[string]ModelCapEntry
	if err := json.Unmarshal(raw, &seed); err != nil {
		t.Fatalf("解析内嵌 model.json 失败: %v", err)
	}
	if len(seed) != len(contextCapFallback) {
		t.Fatalf("model.json 条目数=%d，contextCapFallback=%d，必须同步", len(seed), len(contextCapFallback))
	}
	for id, cap := range contextCapFallback {
		if cap.context <= 0 {
			t.Errorf("%s context=%d 必须 >0", id, cap.context)
		}
		if cap.maxOutput < 0 {
			t.Errorf("%s maxOutput=%d 不能为负", id, cap.maxOutput)
		}
		e, ok := seed[id]
		if !ok {
			t.Errorf("model.json 缺条目 %s", id)
			continue
		}
		if e.ContextLength != cap.context || e.MaxOutputTokens != cap.maxOutput {
			t.Errorf("%s 不一致: model.json=%+v contextCapFallback={%d %d}", id, e, cap.context, cap.maxOutput)
		}
	}
	for id := range seed {
		if _, ok := contextCapFallback[id]; !ok {
			t.Errorf("model.json 多出条目 %s（contextCapFallback 未收录）", id)
		}
	}
}

// TestEffortListingPrecedence 远端权威优先，缺失才落静态表；默认档不跨源拼接。
func TestEffortListingPrecedence(t *testing.T) {
	cases := []struct {
		name      string
		realm     string
		model     string
		remote    []string
		remoteDef string
		wantEff   []string
		wantDef   string
	}{
		{
			name: "远端列表压过静态表", realm: "cn", model: "glm-5.2",
			remote: []string{"low", "high"}, remoteDef: "low",
			wantEff: []string{"low", "high"}, wantDef: "low",
		},
		{
			name: "远端默认档不在列表内则省略", realm: "cn", model: "glm-5.2",
			remote: []string{"low", "high"}, remoteDef: "max",
			wantEff: []string{"low", "high"}, wantDef: "",
		},
		{
			name: "远端有列表无默认档不回填静态默认档", realm: "cn", model: "glm-5.2",
			remote: []string{"high"}, remoteDef: "",
			wantEff: []string{"high"}, wantDef: "",
		},
		{
			name: "CN 静态兜底", realm: "cn", model: "kimi-k3.1-pro",
			wantEff: []string{"low", "high", "max"}, wantDef: "high",
		},
		{
			name: "global 静态兜底", realm: "global", model: "deepseek-v4.1-flash",
			wantEff: []string{"low", "high", "max"}, wantDef: "high",
		},
		{
			name: "未知模型省略", realm: "cn", model: "no-such-model",
			wantEff: nil, wantDef: "",
		},
		{
			name: "CN 专有模型不污染 global", realm: "global", model: "space-bunny",
			wantEff: nil, wantDef: "",
		},
		{
			name: "global 专有模型不污染 CN", realm: "cn", model: "gpt-6-astra",
			wantEff: nil, wantDef: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotEff, gotDef := EffortListing(tc.realm, tc.model, tc.remote, tc.remoteDef)
			if !equalStrings(gotEff, tc.wantEff) {
				t.Errorf("efforts=%v want %v", gotEff, tc.wantEff)
			}
			if gotDef != tc.wantDef {
				t.Errorf("default=%q want %q", gotDef, tc.wantDef)
			}
		})
	}
}

// TestGlobalEffortMapMerge 静态表打底、远端桶覆盖；默认档同规则。
func TestGlobalEffortMapMerge(t *testing.T) {
	efforts, defs := globalEffortMap(
		map[string][]string{"deepseek-v4.1-flash": {"high"}},
		map[string]string{"deepseek-v4.1-flash": "high"},
	)
	if got := efforts["deepseek-v4.1-flash"]; !equalStrings(got, []string{"high"}) {
		t.Errorf("远端覆盖失效: %v", got)
	}
	if defs["deepseek-v4.1-flash"] != "high" {
		t.Errorf("默认档覆盖失效: %q", defs["deepseek-v4.1-flash"])
	}
	if got := efforts["gpt-6-astra"]; len(got) != 5 {
		t.Errorf("静态表打底丢失: %v", got)
	}
	if defs["gpt-6-astra"] != "high" {
		t.Errorf("静态默认档丢失: %q", defs["gpt-6-astra"])
	}
}

// TestCNRosterMatchesSnapshot 官方快照 19 个 CLI 模型 + 2 个参考模型的档位/上下文
// 必须与静态表一致：档位逐字比对，context 允许高估（我们保留 models.dev 高值），
// max_output 必须等于官方产品生效上限。
func TestCNRosterMatchesSnapshot(t *testing.T) {
	snap := loadRosterSnapshot(t)
	if len(snap.Models) != 19 {
		t.Fatalf("CLI roster=%d，期望 19（快照口径变化时请同步更新本断言）", len(snap.Models))
	}
	check := func(m rosterModel) {
		cap, ok := cnEffortFallback[m.ID]
		if !ok {
			t.Errorf("%s 未收录进 cnEffortFallback", m.ID)
			return
		}
		if len(m.Efforts) > 0 {
			if !equalStrings(cap.efforts, m.Efforts) {
				t.Errorf("%s efforts=%v want %v", m.ID, cap.efforts, m.Efforts)
			}
			if cap.defaultEffort != m.DefaultEffort {
				t.Errorf("%s default=%q want %q", m.ID, cap.defaultEffort, m.DefaultEffort)
			}
			gotEff, gotDef := EffortListing("cn", m.ID, nil, "")
			if !equalStrings(gotEff, m.Efforts) || gotDef != m.DefaultEffort {
				t.Errorf("%s EffortListing=%v/%q want %v/%q", m.ID, gotEff, gotDef, m.Efforts, m.DefaultEffort)
			}
		} else if m.DefaultEffort != "" {
			// 产品只声明默认档 → 静态表记单档，且默认档一致。
			if !equalStrings(cap.efforts, []string{m.DefaultEffort}) || cap.defaultEffort != m.DefaultEffort {
				t.Errorf("%s 固定档=%v/%q want [%s]/%s", m.ID, cap.efforts, cap.defaultEffort, m.DefaultEffort, m.DefaultEffort)
			}
		}

		cc, ok := contextCapFallback[m.ID]
		if !ok {
			t.Errorf("%s 未收录进 contextCapFallback", m.ID)
			return
		}
		if cc.context < m.ContextLength {
			t.Errorf("%s context=%d 低于官方 %d（只允许高估）", m.ID, cc.context, m.ContextLength)
		}
		if cc.maxOutput != m.MaxOutputTokens {
			t.Errorf("%s maxOutput=%d want %d", m.ID, cc.maxOutput, m.MaxOutputTokens)
		}
	}
	for _, m := range snap.Models {
		check(m)
	}
	for _, m := range snap.ExtraModels {
		check(m)
	}
}

// TestPromotionSnapshotFixture 快照里的促销条目必须完整保留结构化字段：
// factor 缺失（badge-only）与 factor=0（限免）语义不同，不能混淆。
func TestPromotionSnapshotFixture(t *testing.T) {
	snap := loadRosterSnapshot(t)
	if len(snap.Promotions) < 6 {
		t.Fatalf("促销条目=%d，期望至少 6 条", len(snap.Promotions))
	}
	byID := map[string]rosterPromotion{}
	for _, p := range snap.Promotions {
		byID[p.ID] = p
		if len(p.ModelIDs) == 0 {
			t.Errorf("%s 缺 model_ids", p.ID)
		}
	}
	night, ok := byID["glm-52-night-discount-202607"]
	if !ok || night.Factor == nil || *night.Factor != 0.5 || night.DiscountedCredits != "0.50x" {
		t.Errorf("glm-5.2 夜间折扣快照不完整: %+v", night)
	}
	free, ok := byID["hy3-free-trial-202608"]
	if !ok || free.Factor == nil || *free.Factor != 0 {
		t.Errorf("hy3 限免快照应 factor=0: %+v", free)
	}
	badgeOnly, ok := byID["space-bunny-discount-202610"]
	if !ok || badgeOnly.Factor != nil || badgeOnly.Badge != "限时折扣" {
		t.Errorf("space-bunny 应为 badge-only: %+v", badgeOnly)
	}
}

// mustPromotions 把 JSON 促销定义解析成 []v3ModelPromotion（匿名结构体的唯一简洁构造法）。
func mustPromotions(t *testing.T, raw string) []v3ModelPromotion {
	t.Helper()
	var out []v3ModelPromotion
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("解析促销 JSON 失败: %v", err)
	}
	return out
}

// TestApplyModelPromotionsAt 促销挂载：夜间/白天 priority 切换、限免 factor=0、
// badge-only 不伪造折扣、窗口外不挂、高 priority 胜出。
func TestApplyModelPromotionsAt(t *testing.T) {
	promos := mustPromotions(t, `[
	  {"enabled":true,"priority":100,"modelIds":["glm-5.2"],
	   "badge":{"label":"夜间折扣"},"discount":{"factor":0.5,"discountedCredits":"0.50x"},
	   "hover":{"textZh":"夜间五折"},"schedule":{"daily":[{"start":"23:00","end":"7:50"}]}},
	  {"enabled":true,"priority":50,"modelIds":["glm-5.2"],
	   "badge":{"label":"夜间折扣"},"hover":{"textZh":"白天预告"},
	   "schedule":{"daily":[{"start":"7:50","end":"23:00"}]}},
	  {"enabled":true,"priority":200,"modelIds":["hy3"],
	   "badge":{"label":"限时免费"},"discount":{"factor":0,"discountedCredits":"0x"},
	   "hover":{"textZh":"限免"}},
	  {"enabled":true,"priority":100,"modelIds":["space-bunny"],
	   "badge":{"label":"限时折扣"},"hover":{"textZh":"限时折扣"}},
	  {"enabled":true,"priority":10,"modelIds":["mimo-v2.6-pro"],"badge":{"label":"低优先"}},
	  {"enabled":true,"priority":20,"modelIds":["mimo-v2.6-pro"],"badge":{"label":"高优先"}},
	  {"enabled":true,"priority":10,"modelIds":["step-5-preview"],"badge":{"label":"未生效"},
	   "schedule":{"daily":[{"start":"23:00","end":"23:30"}]}}
	]`)

	newOut := func() map[string]ModelInfo {
		out := map[string]ModelInfo{}
		for _, id := range []string{"glm-5.2", "hy3", "space-bunny", "mimo-v2.6-pro", "step-5-preview"} {
			out[id] = ModelInfo{ID: id}
		}
		return out
	}

	// 23:30 → 夜间五折生效；hy3 限免；badge-only 仍挂标签；高 priority 胜出；
	// step-5 的 23:00-23:30 窗口已过，不挂。
	out := newOut()
	applyModelPromotionsAt(out, promos, time.Date(2026, 11, 5, 23, 30, 0, 0, promoZone))
	if mi := out["glm-5.2"]; mi.PromoFactor == nil || *mi.PromoFactor != 0.5 || mi.PromoCredits != "0.50x" || mi.PromoLabel != "夜间折扣" {
		t.Errorf("夜间档挂载错误: %+v", mi)
	}
	if mi := out["hy3"]; mi.PromoFactor == nil || *mi.PromoFactor != 0 || mi.PromoCredits != "0x" || mi.PromoLabel != "限时免费" {
		t.Errorf("限免档挂载错误: %+v", mi)
	}
	if mi := out["space-bunny"]; mi.PromoFactor != nil || mi.PromoLabel != "限时折扣" || mi.PromoNote == "" {
		t.Errorf("badge-only 不应伪造 factor: %+v", mi)
	}
	if mi := out["mimo-v2.6-pro"]; mi.PromoLabel != "高优先" {
		t.Errorf("priority 胜出错误: %+v", mi)
	}
	if mi := out["step-5-preview"]; mi.PromoLabel != "" {
		t.Errorf("窗口外不应挂促销: %+v", mi)
	}

	// 12:00 → 白天 badge-only 生效（factor nil），夜间五折不生效。
	out = newOut()
	applyModelPromotionsAt(out, promos, time.Date(2026, 11, 5, 12, 0, 0, 0, promoZone))
	if mi := out["glm-5.2"]; mi.PromoFactor != nil || mi.PromoLabel != "夜间折扣" || mi.PromoNote != "白天预告" {
		t.Errorf("白天档挂载错误: %+v", mi)
	}
}
