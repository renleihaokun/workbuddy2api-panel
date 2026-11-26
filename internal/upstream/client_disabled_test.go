// client_disabled_test.go /v3/config 的 disabled 过滤。
//
// 背景：/console/enterprises/personal/models 一直按 disabled 剔除，/v3/config 只解析
// 不过滤——同一份上游数据两条路两套"在架判定"，下架模型会从 v3 漏进 /v1/models。
// 这里钉住三条边界：disabled 被剔除、全 disabled 仍是「空目录」错误、试用横幅在
// 目标 disabled 时回落成 ID-only（不 panic、不继承已下架条目的能力）。
package upstream

import (
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestFetchModelsSkipsDisabledV3(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/console/enterprises/personal/models"):
			return jsonResp(200, `{"code":0,"data":{
				"models":[{"id":"glm-5.3","name":"GLM-5.3","credits":"x0.79"}],
				"agents":[{"name":"cli","models":["glm-5.3"]}]}}`), nil
		case strings.HasSuffix(r.URL.Path, "/v3/config"):
			return jsonResp(200, `{"code":0,"data":{"models":[
				{"id":"glm-4.6","name":"GLM-4.6","disabled":true,"maxInputTokens":168000,"maxOutputTokens":32000},
				{"id":"glm-5.3","name":"GLM-5.3","maxInputTokens":1000000,"maxOutputTokens":16000}
			]}}`), nil
		}
		return jsonResp(404, `{}`), nil
	})
	infos, err := c.FetchModels(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("fetch models: %v", err)
	}
	got := map[string]bool{}
	for _, mi := range infos {
		got[mi.ID] = true
	}
	if got["glm-4.6"] {
		t.Error("disabled 模型 glm-4.6 漏进目录了")
	}
	if !got["glm-5.3"] {
		t.Errorf("正常模型 glm-5.3 被误删: %v", infos)
	}
}

// 全 disabled：仍走「v3/config returned empty models」的既有降级语义，
// 不因为"过滤掉了"就变成"目录正常但为空"这种更含糊的形态。
func TestFetchV3ModelsAllDisabled(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"models":[
			{"id":"glm-4.6","disabled":true},
			{"id":"o4-mini","disabled":true}
		]}}`), nil
	})
	_, err := c.fetchV3ConfigModelMap(&auth.Auth{AccessToken: "at", UID: "u1"}, "")
	if err == nil {
		t.Fatal("want error（全部 disabled 应视为空目录）")
	}
	if !strings.Contains(err.Error(), "empty models") {
		t.Errorf("err=%v want contains 'empty models'", err)
	}
}

// 试用横幅模型（ModelTrialBanner）在目标 disabled 时只保留 ID：
// 能力字段来自 targetModelId 的条目，目标被剔除后不能凭空编造能力。
func TestV3TrialBannerTargetDisabled(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{
			"models":[{"id":"hy4-preview","disabled":true,"maxInputTokens":1000000,"credits":"x0.29"}],
			"productFeaturesConfig":{"ModelTrialBanner":{"banners":[
				{"modelId":"hy4-preview-f","targetModelId":"hy4-preview"}
			]}}
		}}`), nil
	})
	out, err := c.fetchV3ConfigModelMap(&auth.Auth{AccessToken: "at", UID: "u1"}, "")
	if err != nil {
		t.Fatalf("fetch v3: %v", err)
	}
	if _, ok := out["hy4-preview"]; ok {
		t.Error("disabled 的 target 仍在目录里")
	}
	mi, ok := out["hy4-preview-f"]
	if !ok {
		t.Fatalf("试用横幅模型丢了: %v", out)
	}
	if mi.Credits != "" || mi.ContextWindow != 0 {
		t.Errorf("ID-only 回落被污染: %+v", mi)
	}
}
