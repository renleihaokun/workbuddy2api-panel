package server

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/modelid"
)

// TestResolveModelDetailForms 请求侧协议：冒号/斜杠两种写法等价，裸名默认 cn；
// 非保留前缀一律当裸名（与历史语义一致）。
func TestResolveModelDetailForms(t *testing.T) {
	cases := []struct {
		in       string
		realm    string
		bare     string
		explicit bool
	}{
		{"", "cn", "", false},
		{"glm-5.2", "cn", "glm-5.2", false},
		{"deepseek-v4.1-flash", "cn", "deepseek-v4.1-flash", false},
		{"cn:glm-5.2", "cn", "glm-5.2", true},
		{"global:gpt-5.6-sol", "global", "gpt-5.6-sol", true},
		{"cn/glm-5.2", "cn", "glm-5.2", true},
		{"global/gpt-5.6-sol", "global", "gpt-5.6-sol", true},
		{"foo:bar", "cn", "foo:bar", false},
		{"foo/bar", "cn", "foo/bar", false},
		{"CN/x", "cn", "CN/x", false},
		{"cn:", "cn", "", true},
		{"cn/", "cn", "", true},
		{"cn/a/b", "cn", "a/b", true},
	}
	for _, c := range cases {
		realm, bare, explicit := resolveModelDetail(c.in)
		if realm != c.realm || bare != c.bare || explicit != c.explicit {
			t.Errorf("resolveModelDetail(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.in, realm, bare, explicit, c.realm, c.bare, c.explicit)
		}
		// 与 modelid 包同源：server 侧不再有第二套解析规则。
		r2, b2, e2 := modelid.Split(c.in)
		if realm != r2 || bare != b2 || explicit != e2 {
			t.Errorf("resolveModelDetail(%q) 与 modelid.Split 不一致: (%q,%q,%v) vs (%q,%q,%v)",
				c.in, realm, bare, explicit, r2, b2, e2)
		}
	}
}

// TestResolveModelExport 导出面（cmd/server 的会话粘性闭包用）同样支持斜杠写法。
func TestResolveModelExport(t *testing.T) {
	for _, c := range []struct {
		in, realm, bare string
	}{
		{"cn/glm-5.2", "cn", "glm-5.2"},
		{"global/gpt-5.6-sol", "global", "gpt-5.6-sol"},
		{"glm-5.2", "cn", "glm-5.2"},
	} {
		realm, bare := ResolveModel(c.in)
		if realm != c.realm || bare != c.bare {
			t.Errorf("ResolveModel(%q) = (%q,%q), want (%q,%q)", c.in, realm, bare, c.realm, c.bare)
		}
	}
}
