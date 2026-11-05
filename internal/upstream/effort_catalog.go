// effort_catalog.go 推理档位（reasoning effort）产品级静态兜底表。
//
// 数据来源三级（吸收参考仓库 reconcileWithFallback/buddy-adapter.ts:499-523 语义）：
//   - 远端 FetchModels / global 探测已解析的 supportedEfforts/defaultEffort 桶（权威，优先）；
//   - 本文件按 realm 分开的产品级静态兜底表（远端缺失时补齐）；
//   - 两者皆无 → 不在 /v1/models 输出 effort 字段（omitted，不是空数组）。
//
// 数据来源（2026-11-05 重校）：
//   - CN/CodeBuddy 面：官方 WorkBuddy 客户端 product-config 快照（savedAt
//     2026-11-05T14:45:53Z，61 models / 19 个 CLI 对话模型）。仅收录 CLI 名单内模型，
//     supportedEfforts / defaultEffort 原样照抄；无 supportedEfforts 的模型按产品语义
//     记为单档（defaultEffort）。已退役模型（glm-5.1 / kimi-k2.6 等）保留旧条目，
//     只影响兜底，不主动删除。
//   - global/WorkBuddy 国际版面：2026-10 实测。deepseek-v4.1-flash 实测接受
//     low/high/max（reasoning_tokens 随档位递增），旧表写死 ['high'] 已过时。
//
// 两个 realm 对同一模型可能给出不同档位，故**按 realm 分表**，绝不混用。
//
// 只列**可枚举**档位的模型；仅有固定默认档（glm-5.1/kimi-* 的 medium）同样入表，
// 但其档位是「可枚举单档」而非「无选择器」，照抄产品目录如实暴露。
package upstream

// effortCap 一个模型的档位能力（对齐产品兜底表条目 reasoningEfforts + defaultReasoningEffort）。
type effortCap struct {
	efforts       []string
	defaultEffort string
}

// cnEffortFallback CN / CodeBuddy 面静态兜底表。
// 2026-11-05 官方客户端快照：19 个 CLI 模型中带 supportedEfforts 的逐条照抄；
// 固定档模型（fast/balanced/deep-model、mimo-v2.6-pro、glm-5.1 等）记单档。
var cnEffortFallback = map[string]effortCap{
	// ---- DeepSeek ----
	"deepseek-v4-flash":   {efforts: []string{"high", "xhigh"}, defaultEffort: "high"},
	"deepseek-v4.1-flash": {efforts: []string{"low", "high", "max"}, defaultEffort: "high"},
	"deepseek-v4-pro":     {efforts: []string{"high", "xhigh"}, defaultEffort: "high"},

	// ---- 混元 ----
	"hy3":           {efforts: []string{"low", "high"}, defaultEffort: "high"},
	"hy3-x":         {efforts: []string{"low", "high"}, defaultEffort: "high"},
	"hy4free":       {efforts: []string{"high"}, defaultEffort: "high"}, // Hy4 正式版限免通道
	"hy4std":        {efforts: []string{"high"}, defaultEffort: "high"}, // Hy4 正式版标准通道
	"hy4-preview":   {efforts: []string{"high"}, defaultEffort: "high"}, // 已退役，保留兜底
	"hy4-preview-x": {efforts: []string{"high"}, defaultEffort: "high"},

	// ---- GLM ----
	"glm-5.3":       {efforts: []string{"low", "high", "max"}, defaultEffort: "high"},
	"glm-5.3-flash": {efforts: []string{"low", "high", "max"}, defaultEffort: "high"},
	"glm-5.2":       {efforts: []string{"high", "xhigh"}, defaultEffort: "high"},
	"glm-5.1":       {efforts: []string{"medium"}},
	"glm-5v-turbo":  {efforts: []string{"medium"}},

	// ---- Kimi ----
	"kimi-k3.1-pro":     {efforts: []string{"low", "high", "max"}, defaultEffort: "high"},
	"kimi-k3-1":         {efforts: []string{"low", "high", "xhigh"}, defaultEffort: "high"},
	"kimi-k2.8-preview": {efforts: []string{"low", "high", "max"}, defaultEffort: "high"},
	"kimi-k2.7":         {efforts: []string{"medium"}},
	"kimi-k2.6":         {efforts: []string{"medium"}},

	// ---- MiniMax / Step / Space / MiMo ----
	"minimax-m3.1-flash": {efforts: []string{"low", "medium", "high", "xhigh", "max"}, defaultEffort: "max"},
	"minimax-m3":         {efforts: []string{"medium"}},
	"step-5-preview":     {efforts: []string{"low", "medium", "high"}, defaultEffort: "high"},
	"space-bunny":        {efforts: []string{"low", "medium", "high", "xhigh", "max"}, defaultEffort: "max"},
	"mimo-v2.6-pro":      {efforts: []string{"high"}, defaultEffort: "high"},

	// ---- 路由别名（固定档）----
	"fast-model":     {efforts: []string{"medium"}, defaultEffort: "medium"},
	"balanced-model": {efforts: []string{"medium"}, defaultEffort: "medium"},
	"deep-model":     {efforts: []string{"medium"}, defaultEffort: "medium"},
}

// globalEffortFallback global / WorkBuddy 国际版面静态兜底表。
// 2026-10 实测：deepseek-v4.1-flash 在 global 端点同样接受 low/high/max
// （reasoning_tokens 随档位递增：low < high < max）；旧表照抄产品兜底表写死
// 单档 ['high']，会把客户端显式选择的 max/low 静默降级成 high。其余条目沿用
// 2026-09 产品表，未拿到线上实证前不改。
var globalEffortFallback = map[string]effortCap{
	"fast-model":          {efforts: []string{"medium"}},
	"balanced-model":      {efforts: []string{"medium"}},
	"primary-model":       {efforts: []string{"high"}},
	"hy4-preview-f":       {efforts: []string{"high"}, defaultEffort: "high"},
	"hy3":                 {efforts: []string{"low", "high"}, defaultEffort: "high"},
	"deepseek-v4.1-flash": {efforts: []string{"low", "high", "max"}, defaultEffort: "high"},
	"gpt-6-astra":         {efforts: []string{"low", "medium", "high", "xhigh", "max"}, defaultEffort: "high"},
	"gpt-5.6-sol":         {efforts: []string{"low", "medium", "high", "xhigh", "max"}, defaultEffort: "high"},
	"gpt-5.6-terra":       {efforts: []string{"low", "medium", "high", "xhigh", "max"}, defaultEffort: "high"},
	"gpt-5.6-luna":        {efforts: []string{"low", "medium", "high", "xhigh", "max"}, defaultEffort: "high"},
	"gpt-5.5":             {efforts: []string{"low", "medium", "high", "xhigh"}, defaultEffort: "high"},
	"gpt-5.4":             {efforts: []string{"low", "medium", "high", "xhigh"}, defaultEffort: "high"},
	"gpt-5.3-codex":       {efforts: []string{"medium"}},
	"gemini-3.5-flash":    {efforts: []string{"medium"}},
	"glm-5.3":             {efforts: []string{"low", "high", "max"}, defaultEffort: "high"},
	"glm-5.2":             {efforts: []string{"high", "xhigh"}, defaultEffort: "high"},
	"kimi-k3":             {efforts: []string{"medium"}},
	"kimi-k2.6":           {efforts: []string{"medium"}},
}

// staticEffortCap 按 realm 取静态兜底条目；未命中返回 zero effortCap（efforts=nil）。
// realm 经 realmKey 归一化（空 → "cn"），与 efforts 缓存桶同口径。
func staticEffortCap(realm, model string) effortCap {
	table := cnEffortFallback
	if realmKey(realm) == "global" {
		table = globalEffortFallback
	}
	return table[model]
}

// EffortListing 计算模型在 /v1/models 应暴露的 effort 能力（三级查找 + 默认档防御）。
//
// remoteEfforts/remoteDefault 为远端（FetchModels / global 探测）已解析值；
// remoteEfforts 非空时以其为权威（不回落到静态表），否则落到产品级静态兜底表；
// 两者皆无 → efforts 返回 nil（调用方省略字段，不输出空数组）。
//
// defaultEffort 仅在「efforts 非空且 default 命中 efforts」时才返回
// （对齐参考仓库 resolveModel 的 `defaultEffort ∈ efforts` 防御：不宣称不支持的默认档）。
// remoteDefault 空串不回落到静态默认档——默认档随档位表同源：remote 有档位就用 remote 默认档，
// 静态兜底档位就用静态默认档，避免跨源拼接出「档位是静态、默认档是 remote」的矛盾组合。
func EffortListing(realm, model string, remoteEfforts []string, remoteDefault string) (efforts []string, defaultEffort string) {
	var src effortCap
	switch {
	case len(remoteEfforts) > 0:
		src = effortCap{efforts: remoteEfforts, defaultEffort: remoteDefault}
	default:
		src = staticEffortCap(realm, model)
	}
	if len(src.efforts) == 0 {
		return nil, ""
	}
	efforts = append([]string(nil), src.efforts...)
	if src.defaultEffort != "" && containsEffort(efforts, src.defaultEffort) {
		defaultEffort = src.defaultEffort
	}
	return efforts, defaultEffort
}

// containsEffort 档位成员判定（精确匹配，对齐参考仓库 `efforts.includes(defaultEffort)`）。
func containsEffort(efforts []string, want string) bool {
	for _, e := range efforts {
		if e == want {
			return true
		}
	}
	return false
}

// globalEffortMap global 域降级用的 effort 能力表：静态兜底表为基，远端桶覆盖（权威优先）。
//
// prepareBody 对 global 请求调此函数（而非直接用远端桶），因为 global 上游可能不下发
// supportedEfforts——此时也必须按产品静态表降级。历史 issue #84 曾按旧产品表把
// deepseek-v4.1-flash 国际版限定为 high；2026-10 实测该模型接受 low/high/max，
// 静态表已按实测更新（见 globalEffortFallback）。其余模型仍按表降级。
// 语义对齐参考仓库 effortsFor（remoteMeta → productFallback → 静态表），只取前两级：
// 远端桶（探测已解析）→ 本产品静态表（本文件），缺档位即无（不再到通用静态表）。
func globalEffortMap(remoteEfforts map[string][]string, remoteDefaults map[string]string) (map[string][]string, map[string]string) {
	efforts := make(map[string][]string, len(globalEffortFallback)+len(remoteEfforts))
	defs := make(map[string]string, len(globalEffortFallback)+len(remoteDefaults))
	// 静态兜底为基。
	for id, cap := range globalEffortFallback {
		efforts[id] = append([]string(nil), cap.efforts...)
		if cap.defaultEffort != "" {
			defs[id] = cap.defaultEffort
		}
	}
	// 远端权威覆盖（仅当远端确实下发了该模型档位）。
	for id, v := range remoteEfforts {
		if len(v) > 0 {
			efforts[id] = v
		}
	}
	for id, v := range remoteDefaults {
		if v != "" {
			defs[id] = v
		}
	}
	return efforts, defs
}
