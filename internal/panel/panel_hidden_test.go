// panel_hidden_test.go 面板模型页与 /v1/models 的黑名单口径。
//
// 面板此前完全不看 models.hidden：藏了之后 API 列表少了、面板还全列着，
// 看上去像"配置没生效"。这里钉住纯函数的三条边界：
// 裸名藏两域、cn: 只藏 CN、nil 黑名单原样返回。
package panel

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/modelid"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func TestFilterHiddenModels(t *testing.T) {
	cn := []upstream.ModelInfo{{ID: "glm-5.3"}, {ID: "glm-4.6"}, {ID: "auto"}}
	gl := []upstream.ModelInfo{{ID: "glm-5.3"}, {ID: "o4-mini"}, {ID: "auto"}}

	hidden := livecfg.NormalizeHiddenModels([]string{"cn:glm-4.6", "global:o4-mini", "auto"})
	if hidden == nil {
		t.Fatal("NormalizeHiddenModels 不应返回 nil")
	}

	gotCN, hCN := filterHiddenModels(hidden, modelid.RealmCN, cn)
	if hCN != 2 || len(gotCN) != 1 || gotCN[0].ID != "glm-5.3" {
		t.Errorf("CN 过滤: got=%v hidden=%d want [glm-5.3]/2", gotCN, hCN)
	}
	gotGL, hGL := filterHiddenModels(hidden, modelid.RealmGlobal, gl)
	if hGL != 2 || len(gotGL) != 1 || gotGL[0].ID != "glm-5.3" {
		t.Errorf("global 过滤: got=%v hidden=%d want [glm-5.3]/2", gotGL, hGL)
	}

	// nil 黑名单 = 不隐藏（面板 Live 快照缺失时的形态）。
	all, n := filterHiddenModels(nil, modelid.RealmCN, cn)
	if n != 0 || len(all) != len(cn) {
		t.Errorf("nil 黑名单应原样返回: got=%d hidden=%d", len(all), n)
	}
}

// 全被藏掉 ≠ 没有账号：handler 用 hidden_count 区分这两条路径（200 + 空列表 vs 503）。
func TestHiddenModelsHelperNilLive(t *testing.T) {
	p := &Panel{}
	if got := p.hiddenModels(); got != nil {
		t.Errorf("Live 缺失时应返回 nil，got=%v", got)
	}
	live := livecfg.New(livecfg.Snapshot{HiddenModels: livecfg.NormalizeHiddenModels([]string{"glm-4.6"})})
	p.cfg.Live = live
	if _, ok := p.hiddenModels()[modelid.Key(modelid.RealmCN, "glm-4.6")]; !ok {
		t.Error("livecfg 快照里的黑名单没有生效")
	}
}
