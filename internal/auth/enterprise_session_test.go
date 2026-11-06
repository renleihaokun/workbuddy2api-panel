package auth

import (
	"encoding/base64"
	"testing"
)

// mustJWT 用给定 payload 拼一个最小 JWT（header.payload.signature）。
// IsEnterpriseSession 不校验签名，只解 payload，故签名段可为任意占位。
func mustJWT(t *testing.T, payload string) string {
	t.Helper()
	return "hdr." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".sig"
}

// TestIsEnterpriseSession 企业上下文判定（2026-11-06 改判据）：
// 只认 JWT 里**非空 enterprise_id** 或 auth 文件的 EnterpriseID；
// token_source=enterprise_switch 不再单独作为判据——2026-11 起 CN 侧
// CLI 登录会给个人号也打该标记（enterprise_id 为空），实测计分正常。
func TestIsEnterpriseSession(t *testing.T) {
	cases := []struct {
		name  string
		token string
		want  bool
	}{
		// 2026-11 新形态：个人号被签成 enterprise_switch，但 enterprise_id 为空。
		{"enterprise_switch + 空 enterprise_id", mustJWT(t, `{"sub":"u1","token_source":"enterprise_switch","enterprise_id":""}`), false},
		{"enterprise_switch + 无 enterprise_id 键", mustJWT(t, `{"sub":"u1","token_source":"enterprise_switch"}`), false},
		// 真企业上下文。
		{"enterprise_switch + 非空 enterprise_id", mustJWT(t, `{"sub":"u1","token_source":"enterprise_switch","enterprise_id":"e-123"}`), true},
		{"仅 enterprise_id（无 token_source）", mustJWT(t, `{"sub":"u1","enterprise_id":"e-123"}`), true},
		{"enterprise_id 仅空白", mustJWT(t, `{"sub":"u1","enterprise_id":"   "}`), false},
		// 其它形态零回归。
		{"无 token_source 声明", mustJWT(t, `{"sub":"u1","scope":"openid profile"}`), false},
		{"其他 token_source", mustJWT(t, `{"sub":"u1","token_source":"password"}`), false},
		{"带填充 base64url + 非空 enterprise_id", "hdr." + base64.URLEncoding.EncodeToString([]byte(`{"enterprise_id":"e-123"}`)) + ".sig", true},
		{"payload 非 JSON", "hdr." + base64.RawURLEncoding.EncodeToString([]byte("not-json")) + ".sig", false},
		{"非 JWT 形态", "opaque-token", false},
		{"空 token", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := &Auth{UID: "u1", AccessToken: c.token}
			if got := a.IsEnterpriseSession(); got != c.want {
				t.Errorf("IsEnterpriseSession()=%v want %v", got, c.want)
			}
		})
	}

	t.Run("enterpriseId 非空短路为真", func(t *testing.T) {
		a := &Auth{UID: "u1", EnterpriseID: "e1", AccessToken: mustJWT(t, `{"sub":"u1"}`)}
		if !a.IsEnterpriseSession() {
			t.Error("IsEnterprise()=true 时 IsEnterpriseSession() 应恒为 true")
		}
	})

	t.Run("个人形态不翻转 IsEnterprise（计费口径隔离）", func(t *testing.T) {
		a := &Auth{UID: "u1", AccessToken: mustJWT(t, `{"token_source":"enterprise_switch","enterprise_id":""}`)}
		if a.IsEnterprise() {
			t.Error("token_source=enterprise_switch 不应翻转 IsEnterprise（额度查询仍走个人口径）")
		}
	})
}
