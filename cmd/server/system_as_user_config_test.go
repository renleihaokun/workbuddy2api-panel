package main

import "testing"

// TestSystemAsUserFallbackConfig 钉住 features.system_as_user_fallback 的默认值与
// 显式覆盖：默认开启（不换客户端就能绕过 system 指纹拦截），显式 false 生效。
func TestSystemAsUserFallbackConfig(t *testing.T) {
	t.Run("default true", func(t *testing.T) {
		c, err := ParseConfig([]byte(`{"listen":":0"}`))
		if err != nil {
			t.Fatal(err)
		}
		if !c.Features.SystemAsUserFallback {
			t.Error("features.system_as_user_fallback should default to true")
		}
	})
	t.Run("explicit false", func(t *testing.T) {
		c, err := ParseConfig([]byte(`{"listen":":0","features":{"system_as_user_fallback":false}}`))
		if err != nil {
			t.Fatal(err)
		}
		if c.Features.SystemAsUserFallback {
			t.Error("explicit false should disable system_as_user_fallback")
		}
	})
	t.Run("sanitize toggle independent", func(t *testing.T) {
		c, err := ParseConfig([]byte(`{"listen":":0","features":{"sanitize_blacklist_fingerprints":false}}`))
		if err != nil {
			t.Fatal(err)
		}
		if c.Features.SanitizeBlacklistFingerprints {
			t.Error("sanitize toggle should be false")
		}
		if !c.Features.SystemAsUserFallback {
			t.Error("system_as_user_fallback must stay true (orthogonal switch)")
		}
	})
}
