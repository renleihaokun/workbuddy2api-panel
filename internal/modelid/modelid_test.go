package modelid

import (
	"testing"
)

// TestSplit 解析协议钉死：冒号优先、斜杠次之、其余一律裸名（大小写敏感）。
func TestSplit(t *testing.T) {
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
		{"global/auto", "global", "auto", true},
		// 非保留前缀：整体当裸名，网关不猜。
		{"foo:bar", "cn", "foo:bar", false},
		{"foo/bar", "cn", "foo/bar", false},
		{"CN/x", "cn", "CN/x", false},
		{"Global/x", "cn", "Global/x", false},
		{"cnx/foo", "cn", "cnx/foo", false},
		{"xcn/foo", "cn", "xcn/foo", false},
		// 裸尾为空：与历史 cn: 语义保持一致，不做特判。
		{"cn:", "cn", "", true},
		{"cn/", "cn", "", true},
		{"global/", "global", "", true},
		// 余下部分原样保留（含再出现的分隔符）。
		{"cn/a/b", "cn", "a/b", true},
		{"global/a/b", "global", "a/b", true},
		{"cn:global:x", "cn", "global:x", true},
		{"cn/global:x", "cn", "global:x", true},
	}
	for _, c := range cases {
		realm, bare, explicit := Split(c.in)
		if realm != c.realm || bare != c.bare || explicit != c.explicit {
			t.Errorf("Split(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.in, realm, bare, explicit, c.realm, c.bare, c.explicit)
		}
	}
}

// TestFormat 列表侧格式化：非 slash（含零值）一律按 colon，保证默认零回归。
func TestFormat(t *testing.T) {
	cases := []struct {
		style Style
		realm string
		bare  string
		want  string
	}{
		{StyleColon, "cn", "glm-5.2", "cn:glm-5.2"},
		{StyleColon, "global", "gpt-5.6-sol", "global:gpt-5.6-sol"},
		{StyleSlash, "cn", "glm-5.2", "cn/glm-5.2"},
		{StyleSlash, "global", "gpt-5.6-sol", "global/gpt-5.6-sol"},
		{"", "cn", "glm-5.2", "cn:glm-5.2"},
		{"unknown", "cn", "glm-5.2", "cn:glm-5.2"},
	}
	for _, c := range cases {
		if got := Format(c.style, c.realm, c.bare); got != c.want {
			t.Errorf("Format(%q,%q,%q) = %q, want %q", c.style, c.realm, c.bare, got, c.want)
		}
	}
}

// TestParseStyle 配置值校验：空串=默认 colon、忽略空白与大小写、非法值报错。
func TestParseStyle(t *testing.T) {
	ok := []struct {
		in   string
		want Style
	}{
		{"", StyleColon},
		{"colon", StyleColon},
		{"COLON", StyleColon},
		{" Slash ", StyleSlash},
		{"Slash", StyleSlash},
	}
	for _, c := range ok {
		got, err := ParseStyle(c.in)
		if err != nil || got != c.want {
			t.Errorf("ParseStyle(%q) = (%q,%v), want %q,nil", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{"path", "cn:", "slash/", "slash;", "both"} {
		if _, err := ParseStyle(bad); err == nil {
			t.Errorf("ParseStyle(%q) 应报错", bad)
		}
	}
	// 宽松读取：非法值一律回落 colon，不把错误带进请求路径。
	for _, s := range []string{"", "slash", "bogus", "SLASH"} {
		want := StyleColon
		if s == "slash" || s == "SLASH" {
			want = StyleSlash
		}
		if got := StyleOf(s); got != want {
			t.Errorf("StyleOf(%q) = %q, want %q", s, got, want)
		}
	}
}

// TestKeyLabel 内部规范键与展示标签。
func TestKeyLabel(t *testing.T) {
	cases := []struct {
		realm, bare, key string
	}{
		{"cn", "glm-5.2", "cn:glm-5.2"},
		{"global", "gpt-5.6-sol", "global:gpt-5.6-sol"},
		{"cn", "a/b", "cn:a/b"},
		{"cn", "", "cn:"},
	}
	for _, c := range cases {
		if got := Key(c.realm, c.bare); got != c.key {
			t.Errorf("Key(%q,%q) = %q, want %q", c.realm, c.bare, got, c.key)
		}
	}
	if Label("cn") != "CN" || Label("global") != "Global" || Label("") != "CN" {
		t.Errorf("Label 不符合预期: %q %q %q", Label("cn"), Label("global"), Label(""))
	}
}
