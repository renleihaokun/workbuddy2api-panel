package panel

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/modelid"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestPanelModelEntryStyle 面板模型条目的 ID 写法/结构化域字段/名称前缀：
// 与 /v1/models 同口径，且新增字段不改变既有能力/档位字段。
func TestPanelModelEntryStyle(t *testing.T) {
	mi := upstream.ModelInfo{
		ID:                "glm-5.2",
		Name:              "GLM-5.2",
		SupportsReasoning: true,
		Efforts:           []string{"high"},
		DefaultEffort:     "high",
	}

	// 默认 colon：与历史输出一致，并补 realm/model 字段。
	e := panelModelEntry(modelid.StyleColon, false, "cn", mi, nil, "", nil)
	if e["id"] != "cn:glm-5.2" {
		t.Errorf("id=%v want cn:glm-5.2", e["id"])
	}
	if e["realm"] != "cn" || e["model"] != "glm-5.2" {
		t.Errorf("realm/model=%v/%v want cn/glm-5.2", e["realm"], e["model"])
	}
	if e["name"] != "GLM-5.2" {
		t.Errorf("name=%v want GLM-5.2", e["name"])
	}
	if e["supports_reasoning"] != true {
		t.Errorf("supports_reasoning 丢失: %v", e["supports_reasoning"])
	}

	// slash：只换 ID 写法。
	e = panelModelEntry(modelid.StyleSlash, false, "global", upstream.ModelInfo{ID: "gpt-5.6-sol", Name: "GPT-5.6 Sol"}, nil, "", nil)
	if e["id"] != "global/gpt-5.6-sol" {
		t.Errorf("id=%v want global/gpt-5.6-sol", e["id"])
	}
	if e["realm"] != "global" || e["model"] != "gpt-5.6-sol" {
		t.Errorf("realm/model=%v/%v want global/gpt-5.6-sol", e["realm"], e["model"])
	}

	// 名称前缀：仅在开启且 name 非空时生效。
	e = panelModelEntry(modelid.StyleSlash, true, "cn", mi, nil, "", nil)
	if e["name"] != "CN · GLM-5.2" {
		t.Errorf("prefixed name=%v want CN · GLM-5.2", e["name"])
	}
	e = panelModelEntry(modelid.StyleSlash, true, "cn", upstream.ModelInfo{ID: "no-name"}, nil, "", nil)
	if e["name"] != "" {
		t.Errorf("空 name 不应被编造: %v", e["name"])
	}
}

// TestPanelModelEntryPricing 定价字段透出：牌价（credits）、生效价（promo_factor/
// promo_credits）、促销标签与说明；badge-only 条目不得伪造 promo_factor。
// 价格本身来自上游实时目录，这里只锁「有值就透出、无值不编造」的契约。
func TestPanelModelEntryPricing(t *testing.T) {
	factor := 0.5
	mi := upstream.ModelInfo{
		ID:           "glm-5.2",
		Name:         "GLM-5.2",
		Credits:      "x0.79",
		PromoFactor:  &factor,
		PromoCredits: "0.50x",
		PromoLabel:   "夜间折扣",
		PromoNote:    "每晚 23:00—次日 8:00 积分限时立减",
	}
	e := panelModelEntry(modelid.StyleColon, false, "cn", mi, nil, "", nil)
	if e["credits"] != "x0.79" {
		t.Errorf("credits=%v want x0.79", e["credits"])
	}
	if v, ok := e["promo_factor"].(float64); !ok || v != 0.5 {
		t.Errorf("promo_factor=%v want 0.5", e["promo_factor"])
	}
	if e["promo_credits"] != "0.50x" || e["promo_label"] != "夜间折扣" || e["promo_note"] == "" {
		t.Errorf("促销字段透出不全: %v", e)
	}

	// badge-only（错峰类）：只有标签/说明，没有机器可读折扣 → 不应出现 promo_factor。
	e = panelModelEntry(modelid.StyleColon, false, "cn", upstream.ModelInfo{
		ID: "space-bunny", Credits: "x0.08", PromoLabel: "限时折扣", PromoNote: "限时活动",
	}, nil, "", nil)
	if _, has := e["promo_factor"]; has {
		t.Errorf("badge-only 不应携带 promo_factor: %v", e)
	}
	if e["promo_label"] != "限时折扣" {
		t.Errorf("badge-only 标签丢失: %v", e)
	}

	// 无促销：只有牌价，不编造 promo_*。
	e = panelModelEntry(modelid.StyleColon, false, "cn", upstream.ModelInfo{ID: "hy3", Credits: "0"}, nil, "", nil)
	for _, k := range []string{"promo_factor", "promo_credits", "promo_label", "promo_note"} {
		if _, has := e[k]; has {
			t.Errorf("无促销时不应有 %s: %v", k, e)
		}
	}
}
