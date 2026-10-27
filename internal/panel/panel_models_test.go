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
