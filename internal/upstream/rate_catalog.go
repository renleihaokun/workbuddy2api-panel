// rate_catalog.go 模型牌价（credits）静态兜底表。
//
// 定位：**只用于展示兜底**——上游目录没下发 credits 时，面板「模型与档位」
// 与 /v1/models 用这里的 2026-11-05 官方客户端快照价补位，避免显示「—」。
// 不参与 ModelRate / credit-floor / 选号：那些路径仍以远端目录为准，静态价
// 过期不会影响路由或计费判断。
//
// 数据来源：WorkBuddy 客户端 product-config 快照（savedAt 2026-11-05T14:45:53Z，
// 19 个 CLI 对话模型）。价格随时会变（官方调价/促销），远端永远优先。
package upstream

import "strings"

// cnRateFallback CN 面牌价（credits 原文，含 x 前缀）。
var cnRateFallback = map[string]string{
	"fast-model":          "x0.15",
	"balanced-model":      "x0.50",
	"deep-model":          "x1.00",
	"hy4free":             "x0.00",
	"hy4std":              "x0.29",
	"hy3":                 "x0.00",
	"hy3-x":               "x0.05",
	"deepseek-v4.1-flash": "x0.11",
	"glm-5.3":             "x0.69",
	"glm-5.3-flash":       "x0.08",
	"glm-5.2":             "x0.79",
	"space-bunny":         "x0.08",
	"minimax-m3.1-flash":  "x0.08",
	"kimi-k3.1-pro":       "x0.97",
	"kimi-k3-1":           "x1.62",
	"kimi-k2.8-preview":   "x0.77",
	"deepseek-v4-pro":     "x0.51",
	"step-5-preview":      "x0.33",
	"mimo-v2.6-pro":       "x0.13",
	// 非 CLI 名单但仍可能出现的参考条目（模型数组内保留）。
	"deepseek-v4-flash": "x0.17",
	"hy4-preview-x":     "x0.29",
}

// globalRateFallback global 面牌价兜底：本机没有国际版客户端快照，只收录
// 有线上/社区实证的条目（2026-11 deepseek-v4.1-flash 国际版 0.11x）。
var globalRateFallback = map[string]string{
	"deepseek-v4.1-flash": "x0.11",
}

// CreditsListing 返回模型应展示的牌价：远端原文优先；远端缺失才落静态快照；
// 两者皆无返回空串（不编造）。
func CreditsListing(realm, model, remote string) string {
	if s := strings.TrimSpace(remote); s != "" {
		return s
	}
	table := cnRateFallback
	if realmKey(realm) == "global" {
		table = globalRateFallback
	}
	return table[model]
}
