// catalog_names_test.go 每域模型名快照（catalog.go）的三态语义与 TTL：
// 同名模型跨域回退只认 CatalogPresent，且过期/未探测一律 Unknown（不猜）。
package upstream

import (
	"testing"
	"time"
)

// TestCatalogModelStateTriState 三态：未探测 → Unknown；写入后 present/absent；
// realm 隔离（global 的目录不污染 cn）；空 realm 归一化到 cn；空模型名恒 Unknown。
func TestCatalogModelStateTriState(t *testing.T) {
	c := New()
	if got := c.CatalogModelState("cn", "glm-5.2"); got != CatalogUnknown {
		t.Fatalf("未探测 cn = %v, want CatalogUnknown", got)
	}
	c.storeCatalogNames("cn", []string{"glm-5.2", "kimi-k3"})
	if got := c.CatalogModelState("cn", "glm-5.2"); got != CatalogPresent {
		t.Errorf("cn 命中 = %v, want CatalogPresent", got)
	}
	if got := c.CatalogModelState("cn", "no-such-model"); got != CatalogAbsent {
		t.Errorf("cn 未命中 = %v, want CatalogAbsent", got)
	}
	// realm 隔离：cn 的目录不得让 global 判出 present/absent。
	if got := c.CatalogModelState("global", "glm-5.2"); got != CatalogUnknown {
		t.Errorf("global 未探测 = %v, want CatalogUnknown（realm 必须隔离）", got)
	}
	// 空 realm 与全仓口径一致：按 cn 处理。
	if got := c.CatalogModelState("", "glm-5.2"); got != CatalogPresent {
		t.Errorf("空 realm = %v, want CatalogPresent（归一化到 cn）", got)
	}
	// 空模型名不参与判定。
	if got := c.CatalogModelState("cn", ""); got != CatalogUnknown {
		t.Errorf("空模型名 = %v, want CatalogUnknown", got)
	}
}

// TestCatalogModelStateExpires TTL 到期后回落 Unknown（不能拿旧目录决定路由）。
func TestCatalogModelStateExpires(t *testing.T) {
	c := New()
	c.storeCatalogNames("cn", []string{"glm-5.2"})
	// 直接把快照时间推到 TTL 之外（同包测试，允许触碰内部状态）。
	c.catalog.mu.Lock()
	e := c.catalog.m["cn"]
	e.at = time.Now().Add(-2 * catalogTTL)
	c.catalog.m["cn"] = e
	c.catalog.mu.Unlock()

	if got := c.CatalogModelState("cn", "glm-5.2"); got != CatalogUnknown {
		t.Errorf("过期目录 = %v, want CatalogUnknown", got)
	}
	if names := c.CachedCatalogNames("cn"); names != nil {
		t.Errorf("过期快照 = %v, want nil", names)
	}
}

// TestStoreCatalogNamesIgnoresEmpty 空名单不写入：一次异常的空目录不能把
// 「另一域确有同名模型」判成不存在（宁可保持 Unknown）。
func TestStoreCatalogNamesIgnoresEmpty(t *testing.T) {
	c := New()
	c.storeCatalogNames("cn", nil)
	c.storeCatalogNames("cn", []string{"", "   "})
	if got := c.CatalogModelState("cn", "glm-5.2"); got != CatalogUnknown {
		t.Errorf("空名单后 = %v, want CatalogUnknown", got)
	}
	c.storeCatalogNames("cn", []string{"glm-5.2", " glm-5.2 ", ""})
	if names := c.CachedCatalogNames("cn"); len(names) != 1 || names[0] != "glm-5.2" {
		t.Errorf("去重/去空白后 = %v, want [glm-5.2]", names)
	}
}

// TestCatalogStateString 日志口径：三种状态各有稳定可读名。
func TestCatalogStateString(t *testing.T) {
	cases := map[CatalogState]string{
		CatalogUnknown: "unknown",
		CatalogPresent: "present",
		CatalogAbsent:  "absent",
	}
	for st, want := range cases {
		if got := st.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", int(st), got, want)
		}
	}
}

// TestCatalogNilClientNilSafe 零值/nil Client 不得 panic（面板/测试可能裸用）。
func TestCatalogNilClientNilSafe(t *testing.T) {
	var c *Client
	if got := c.CatalogModelState("cn", "glm-5.2"); got != CatalogUnknown {
		t.Errorf("nil client = %v, want CatalogUnknown", got)
	}
	if names := c.CachedCatalogNames("cn"); names != nil {
		t.Errorf("nil client names = %v, want nil", names)
	}
	c.storeCatalogNames("cn", []string{"glm-5.2"}) // 不得 panic
}
