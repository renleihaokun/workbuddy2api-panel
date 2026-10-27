package livecfg

import "testing"

// TestNormalizeHiddenModels 归一化语义：
//   - nil/空/全空白 → nil（不隐藏）
//   - 裸名 → 两域都藏（"cn:x" + "global:x"）
//   - "cn:"/"global:" 前缀 → 只藏该域
//   - "cn/"、"global/" 斜杠前缀与冒号等价（models.id_style=slash 时客户端会这么写）
//   - 非 cn/global 前缀（如 "xx:yy"、"CN/x"）视为裸名整体（与 resolveModel 对称：
//     前缀不合法时 realm=cn、bare=原串，网关侧检查 "cn:xx:yy" 命中）
func TestNormalizeHiddenModels(t *testing.T) {
	if got := NormalizeHiddenModels(nil); got != nil {
		t.Errorf("nil → %v, want nil", got)
	}
	if got := NormalizeHiddenModels([]string{}); got != nil {
		t.Errorf("empty → %v, want nil", got)
	}
	if got := NormalizeHiddenModels([]string{"  ", ""}); got != nil {
		t.Errorf("all blank → %v, want nil", got)
	}

	got := NormalizeHiddenModels([]string{"deepseek-v3"})
	for _, want := range []string{"cn:deepseek-v3", "global:deepseek-v3"} {
		if _, ok := got[want]; !ok {
			t.Errorf("bare name: missing %q in %v", want, got)
		}
	}
	if len(got) != 2 {
		t.Errorf("bare name: got %d entries %v, want 2", len(got), got)
	}

	got = NormalizeHiddenModels([]string{"cn:pro-model"})
	if _, ok := got["cn:pro-model"]; !ok {
		t.Errorf("cn: prefix: missing %q in %v", "cn:pro-model", got)
	}
	if _, ok := got["global:pro-model"]; ok {
		t.Errorf("cn: prefix: %q must not hide global realm", "global:pro-model")
	}

	// 前后空白被裁掉
	got = NormalizeHiddenModels([]string{"  glm-5.2  "})
	if _, ok := got["cn:glm-5.2"]; !ok {
		t.Errorf("trimmed bare name: missing %q in %v", "cn:glm-5.2", got)
	}

	// 斜杠写法与冒号等价：只藏该域，规范键仍是 realm:bare。
	got = NormalizeHiddenModels([]string{"cn/only-cn"})
	if _, ok := got["cn:only-cn"]; !ok {
		t.Errorf("cn/ prefix: missing %q in %v", "cn:only-cn", got)
	}
	if _, ok := got["global:only-cn"]; ok {
		t.Errorf("cn/ prefix: %q must not hide global realm", "global:only-cn")
	}
	got = NormalizeHiddenModels([]string{"global/only-global"})
	if _, ok := got["global:only-global"]; !ok {
		t.Errorf("global/ prefix: missing %q in %v", "global:only-global", got)
	}
	if _, ok := got["cn:only-global"]; ok {
		t.Errorf("global/ prefix: %q must not hide cn realm", "cn:only-global")
	}

	// 非法前缀视为裸名整体（大小写敏感，斜杠同理）
	got = NormalizeHiddenModels([]string{"xx:yy", "CN/x"})
	for _, want := range []string{"cn:xx:yy", "global:xx:yy", "cn:CN/x", "global:CN/x"} {
		if _, ok := got[want]; !ok {
			t.Errorf("odd prefix: missing %q in %v", want, got)
		}
	}
}
