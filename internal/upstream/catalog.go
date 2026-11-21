// catalog.go 每域「模型名快照」：给同名模型跨域回退提供「另一域到底有没有这个
// 模型」的只读判据（pool.cross_domain_model_fallback）。
//
// 为什么单独存一份名字集合，而不是复用既有的倍率/effort 缓存：
//   - storeModelRates 只收「有可解析倍率」的模型，天然缺条目，不能当目录用；
//   - effort 桶同样只收「有档位数据」的模型；
//   - 跨域回退要回答的是「这个名字在该域目录里存不存在」，必须按探测到的完整
//     ID 名单存，缺一条就会把可回退的模型判成不存在。
//
// 三态语义（宁可少回退，不猜）：
//   - CatalogPresent：目录新鲜且确有该模型；
//   - CatalogAbsent ：目录新鲜且不含该模型（handler 可据此直接换域，不浪费 11102）；
//   - CatalogUnknown：从未探测 / 已过期 / 探测失败（handler 不据此跨域）。
//
// 写入点只有两个（都已在启动预热路径上，无新增上游请求）：
//   - FetchModels 成功（CN 或任一非 global 路由账号）；
//   - global 目录探测成功（global_models.go 的 fetchGlobalModelsOnce）。
package upstream

import (
	"strings"
	"sync"
	"time"
)

// CatalogState 模型目录对某个 (域, 模型) 的判定结果。
type CatalogState int

const (
	// CatalogUnknown 目录未探测/已过期/探测失败：调用方不得据此跨域（不猜）。
	CatalogUnknown CatalogState = iota
	// CatalogPresent 目录新鲜且确有该模型。
	CatalogPresent
	// CatalogAbsent 目录新鲜且不含该模型。
	CatalogAbsent
)

// String 日志/面板可读名（"unknown" / "present" / "absent"）。
func (s CatalogState) String() string {
	switch s {
	case CatalogPresent:
		return "present"
	case CatalogAbsent:
		return "absent"
	default:
		return "unknown"
	}
}

// catalogTTL 名字快照有效期。取 1h（与 global 目录探测缓存同量级）：
// 目录增删模型是小时级事件，比这更旧的名字集合不该再决定请求路由。
const catalogTTL = time.Hour

// catalogEntry 单域名字快照。
type catalogEntry struct {
	names map[string]struct{}
	at    time.Time
}

// realmCatalog 每域名字快照（按 Client 实例持有，测试新建 Client 即隔离）。
// 命名避开 model_catalog.go 的包级单例 catalogState（那是 model.json 缓存）。
type realmCatalog struct {
	mu sync.RWMutex
	m  map[string]catalogEntry // realmKey → 快照
}

// storeCatalogNames 记录某域最近一次成功探测到的模型名集合（空列表 = 不写入，
// 避免一次异常的空目录把可回退的模型判成不存在）。
func (c *Client) storeCatalogNames(realm string, names []string) {
	if c == nil || len(names) == 0 {
		return
	}
	set := make(map[string]struct{}, len(names))
	for _, id := range names {
		if id = strings.TrimSpace(id); id != "" {
			set[id] = struct{}{}
		}
	}
	if len(set) == 0 {
		return
	}
	c.catalog.mu.Lock()
	defer c.catalog.mu.Unlock()
	if c.catalog.m == nil {
		c.catalog.m = make(map[string]catalogEntry, 2)
	}
	c.catalog.m[realmKey(realm)] = catalogEntry{names: set, at: time.Now()}
}

// CatalogModelState 返回该域目录对该模型的判定（只读、不触发任何上游探测）。
//
// 供同名模型跨域回退判定：只有 CatalogPresent 才算「另一域确有同名模型」，
// 只有 CatalogAbsent 才允许「本域目录明确没有」的快速换域。
func (c *Client) CatalogModelState(realm, model string) CatalogState {
	if c == nil || model == "" {
		return CatalogUnknown
	}
	c.catalog.mu.RLock()
	defer c.catalog.mu.RUnlock()
	e, ok := c.catalog.m[realmKey(realm)]
	if !ok || e.at.IsZero() || time.Since(e.at) > catalogTTL {
		return CatalogUnknown
	}
	if _, hit := e.names[model]; hit {
		return CatalogPresent
	}
	return CatalogAbsent
}

// CachedCatalogNames 返回该域名字快照的副本（过期/未探测 → nil）。
//
// 只读观测面：面板/排障用它确认「目录到底拉到了什么」，不参与路由决策。
func (c *Client) CachedCatalogNames(realm string) []string {
	if c == nil {
		return nil
	}
	c.catalog.mu.RLock()
	defer c.catalog.mu.RUnlock()
	e, ok := c.catalog.m[realmKey(realm)]
	if !ok || e.at.IsZero() || time.Since(e.at) > catalogTTL {
		return nil
	}
	out := make([]string, 0, len(e.names))
	for id := range e.names {
		out = append(out, id)
	}
	return out
}
