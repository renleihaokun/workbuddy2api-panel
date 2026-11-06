// catalog_fields_test.go /v1/models 的牌价兜底：上游 credits 缺失时落 2026-11-05
// 官方快照参考价并标记 credits_reference；远端有值时不标记、不覆盖。
package server

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func TestApplyModelInfoFieldsCreditsFallback(t *testing.T) {
	// 远端缺失 → 用快照参考价（hy4free 11-05 为 x0.00）。
	e := applyModelInfoFields(map[string]any{}, "cn", upstream.ModelInfo{ID: "hy4free"})
	if e["credits"] != "x0.00" {
		t.Errorf("credits=%v want x0.00", e["credits"])
	}
	if e["credits_reference"] != true {
		t.Errorf("参考价应标记 credits_reference: %v", e)
	}

	// 远端有值 → 原样优先，不标记参考价。
	e = applyModelInfoFields(map[string]any{}, "cn", upstream.ModelInfo{ID: "hy4free", Credits: "x0.99"})
	if e["credits"] != "x0.99" {
		t.Errorf("credits=%v want x0.99", e["credits"])
	}
	if _, has := e["credits_reference"]; has {
		t.Errorf("实时价不应标记 credits_reference: %v", e)
	}

	// 未知模型且远端缺失 → 省略字段，不编造。
	e = applyModelInfoFields(map[string]any{}, "cn", upstream.ModelInfo{ID: "no-such-model"})
	if _, has := e["credits"]; has {
		t.Errorf("未知模型不应输出 credits: %v", e)
	}
}
