// relogin_frontend_test.go 前端「待重登 / RT 已失效」标签与重登入口的接线断言。
//
// 单独成文件的原因：它守的是 app.js/index.html 的内容，与后端重登流程的用例
// （relogin_test.go）关注点不同、稳定性也不同——后端改了这里不会红，前端改了才红。
package panel

import (
	"os"
	"strings"
	"testing"
)

// TestFrontendWiresReloginAndRTDeadBadges 前端必须真的把「待重登 / RT 已失效」
// 与「重登」入口渲染出来——后端字段齐了但前端不显示，运维就永远看不见。
func TestFrontendWiresReloginAndRTDeadBadges(t *testing.T) {
	src, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(src)
	for _, want := range []string{
		"s.needs_relogin",      // 账号行按 needs_relogin 区分标签
		"s.refresh_token_dead", // RT 判死黄标
		"RT 已失效",               // 黄标文案
		"待重登",                  // 红标文案
		`data-a="relogin"`,     // 行内重登按钮
		"relogin/start",        // 重登发起端点
		"d.needs_relogin",      // 概况里的待重登计数
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js 缺少 %q（面板看不到/点不到该状态）", want)
		}
	}
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(html), `id="addTitle"`) {
		t.Error("index.html 的添加账号弹窗标题缺 id=addTitle（重登弹窗无法改标题）")
	}
}
