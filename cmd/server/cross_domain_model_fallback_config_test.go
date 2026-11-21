// cross_domain_model_fallback_config_test.go pool.cross_domain_model_fallback 配置测试：
// 默认 false（零回归）/ 文件覆盖为 true。
package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCrossDomainModelFallbackDefault 键缺席 → 默认 false（显式前缀仍按请求域路由）。
func TestCrossDomainModelFallbackDefault(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Pool.CrossDomainModelFallback {
		t.Error("cross_domain_model_fallback 缺省必须为 false")
	}
}

// TestCrossDomainModelFallbackParsedFromFile 显式配置覆盖默认。
func TestCrossDomainModelFallbackParsedFromFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	if err := os.WriteFile(fp, []byte(`{"pool":{"cross_domain_model_fallback":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Pool.CrossDomainModelFallback {
		t.Error("cross_domain_model_fallback=true 未生效")
	}
	// 两个跨域开关互相独立：只开新开关不得把旧开关也点亮。
	if c.Pool.CrossDomainFallback {
		t.Error("cross_domain_fallback 不得被新键隐式打开")
	}
}
