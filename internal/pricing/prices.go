// Package pricing 官方 API 牌价表（人民币 / 每百万 tokens），只服务面板「已省」统计。
//
// 定位：**纯展示**。不参与 ModelRate / 积分保底 / 选号 / credit-floor 任何决策；
// 与 internal/upstream/rate_catalog.go（WorkBuddy 客户端 credits 牌价）是两套东西：
// 那边是「上游要扣多少积分」，这里是「同样这些 token 走厂商官方 API 要付多少元」。
//
// 数据来源：厂商中国区官网定价页快照（2026-11-17 核对），逐条写在 seedPrices 里
// 并带 source 出处；用户可在 data/prices.json 覆盖/补充（见 store.go），文件改动
// 即时生效，无需重启、无需保存配置。
//
// 峰谷价：DeepSeek 的牌价分高峰/空闲两档 —— 空闲时段价格是高峰时段的一半，北京时间
// 周一至周五（不含法定节假日）9:00–12:00、14:00–18:00 为高峰。Price.Peak 描述这条
// 规则；Input/Output/CacheHit 一律写**高峰价**，非高峰时段整体乘 Factor。小时桶按
// 该小时是否高峰精确计价；90 天前折叠出的日桶按当天高峰小时数加权折算（周末全天
// 空闲；法定节假日无法离线识别，按工作日处理 → 偏高，界面上写明）。
package pricing

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/modelid"
)

// seedUpdated 内置牌价快照的核对日期（写进面板「牌价表」卡片）。
const seedUpdated = "2026-11-17"

// 币种与单位：本期只支持人民币 / 每百万 tokens，其它取值一律拒绝（不猜汇率）。
const (
	CurrencyCNY = "CNY"
	UnitPer1M   = "per_1m_tokens"
)

// beijing 北京时间固定时区（1991 年后无夏令时，用固定偏移即可，不依赖 tzdata）。
var beijing = time.FixedZone("CST", 8*3600)

// Price 单个模型的官方牌价（人民币 / 每百万 tokens）。
//
// Input/Output 必填；CacheHit 为 nil 表示该模型没有单独的缓存命中价（按 Input 计），
// 显式 0 表示缓存命中免费 —— 两种语义必须分开，所以用指针。
type Price struct {
	Input    float64  `json:"input"`               // 缓存未命中的输入价
	Output   float64  `json:"output"`              // 输出价
	CacheHit *float64 `json:"cache_hit,omitempty"` // 缓存命中价；nil = 无单独档位
	Peak     *Peak    `json:"peak,omitempty"`      // 峰谷价规则；nil = 全天同价
	Source   string   `json:"source,omitempty"`    // 出处（官网页面 / 核对日期）
	Note     string   `json:"note,omitempty"`      // 备注（下线、别名映射等）
}

// Peak 峰谷价规则：高峰时段按牌价，其余时段乘 Factor。
//
// Hours 是高峰时段（北京时间，左闭右开），例如 DeepSeek 的 [[9,12],[14,18]]。
// WeekdaysOnly=true 时周末全天按空闲价。Holidays 里的日期（2006-01-02）整天按
// 空闲价 —— 官网「不含法定节假日」的规则需要用户自行填日期，否则节假日会被
// 当成工作日（偏高）。
type Peak struct {
	Factor       float64  `json:"factor"`
	Hours        [][2]int `json:"hours"`
	WeekdaysOnly bool     `json:"weekdays_only"`
	Holidays     []string `json:"holidays,omitempty"`
}

// Meta 牌价表自身的元信息（面板展示 + 排障）。
type Meta struct {
	Enabled    bool     `json:"enabled"`
	Currency   string   `json:"currency"`
	Unit       string   `json:"unit"`
	Models     int      `json:"models"`
	Updated    string   `json:"updated"`
	Source     string   `json:"source"` // seed / seed+file
	Error      string   `json:"error,omitempty"`
	PeakModels []string `json:"peak_models,omitempty"`
}

// Table 只读牌价表快照：内置种子 ∪ 数据文件覆盖。
//
// 只读是刻意的：热更新走 Store 整体替换指针，读侧（聚合循环）永远看到一致视图。
type Table struct {
	models map[string]Price
	meta   Meta
}

// seedPrices 内置牌价（高峰价口径，人民币 / 百万 tokens）。
//
// 只收录**已核对过官网**的条目；查不到的模型宁可不写（面板会显示未定价），
// 绝不填估值。
var seedPrices = map[string]Price{
	"glm-5.3": {
		Input: 8, Output: 28, CacheHit: ptr(2),
		Source: "docs.bigmodel.cn/cn/guide/start/pricing（2026-11-17）",
	},
	"glm-5.3-flash": {
		Input: 0.8, Output: 2.8, CacheHit: ptr(0.23),
		Source: "docs.bigmodel.cn/cn/guide/start/pricing（2026-11-17）",
	},
	"glm-5.2": {
		Input: 8, Output: 28, CacheHit: ptr(2),
		Source: "docs.bigmodel.cn/cn/guide/start/pricing（2026-11-17）",
	},
	"deepseek-v4.1-flash": {
		Input: 2, Output: 8, CacheHit: ptr(0.04), Peak: deepSeekPeak(),
		Source: "api-docs.deepseek.com/zh-cn/quick_start/pricing（高峰价，2026-11-17）",
	},
	"deepseek-v4-flash": {
		Input: 2, Output: 8, CacheHit: ptr(0.04), Peak: deepSeekPeak(),
		Source: "api-docs.deepseek.com/zh-cn/quick_start/pricing（高峰价，2026-11-17）",
		Note:   "官方已下线，请求由 DeepSeek-V4.1-Flash 承接并按 Flash 价计费",
	},
	"deepseek-v4-pro": {
		Input: 9, Output: 27, CacheHit: ptr(0.3), Peak: deepSeekPeak(),
		Source: "api-docs.deepseek.com/zh-cn/quick_start/pricing（高峰价，2026-11-17）",
	},
	"minimax-m3.1-flash": {
		Input: 1, Output: 4, CacheHit: ptr(0.1),
		Source: "platform.minimaxi.com/docs/pricing/overview（2026-11-17）",
	},
	"space-bunny": {
		Input: 1, Output: 4, CacheHit: ptr(0.1),
		Source: "platform.minimaxi.com/docs/pricing/overview（2026-11-17）",
		Note:   "MiniMax M3.1 Flash 的早期匿名版本（MiniMax 官方博客），按 M3.1 Flash 牌价折",
	},
	"step-5-preview": {
		Input: 7, Output: 20, CacheHit: ptr(0.7),
		Source: "platform.stepfun.com/docs/zh/guides/pricing/details（2026-11-17）",
		Note:   "缓存命中价 2026-10-14 起由 0.35 调到 0.70（腾讯 TokenHub 价目表仍显示 0.35，可能未同步）",
	},
	// Kimi（Moonshot）：K3 系列的缓存写入按 TTL 另计（5min 档与输入同价、1h 档双倍），
	// 本统计只算「命中 / 未命中 / 输出」三段，不单列缓存写入 —— 近似但不会虚高。
	//
	// WorkBuddy 的 kimi-k3-1 官方价目表里没有这个 id（腾讯侧加的 -1 后缀），按 Kimi K3
	// 牌价折；若实际跑的是别的档位，在 data/prices.json 里覆盖一行即可。
	"kimi-k3.1-pro": {
		Input: 13, Output: 68, CacheHit: ptr(1.3),
		Source: "platform.kimi.com/docs/pricing/chat（2026-11-17）",
		Note:   "K3.1 Pro；缓存写入 5min 13 / 1h 26 元/百万另行计费，这里只算命中与未命中",
	},
	"kimi-k3": {
		Input: 20, Output: 100, CacheHit: ptr(2),
		Source: "platform.kimi.com/docs/pricing/chat（2026-11-17）",
		Note:   "缓存写入 5min 20 / 1h 40 元/百万另行计费",
	},
	"kimi-k3-1": {
		Input: 20, Output: 100, CacheHit: ptr(2),
		Source: "platform.kimi.com/docs/pricing/chat（按 Kimi K3 折，2026-11-17）",
		Note:   "WorkBuddy 侧加的 -1 后缀，官方价目表无此 id；按 Kimi K3 牌价折算，如实际档位不同可覆盖",
	},
	// K2.8 是 K3 之后发的新一代预览模型，月之暗面自家定价页还没列，但腾讯 TokenHub
	// （WorkBuddy 走的渠道）已按量计费，价目与 K3 系列同口径。
	"kimi-k2.8-preview": {
		Input: 6.5, Output: 27, CacheHit: ptr(1.7),
		Source: "cloud.tencent.com/document/product/1823/130055（TokenHub 计费页，2026-11-17）",
	},
	// 小米 MiMo
	"mimo-v2.6-pro": {
		Input: 3, Output: 6, CacheHit: ptr(0.025),
		Source: "mimo.mi.com/models/zh-CN/mimo-v2.6-pro（2026-11-17，人民币价）",
	},
	// 腾讯混元（TokenHub 按量计费）。WorkBuddy 的 hy4free/hy4std/hy3-x 是自家档位名，
	// 官方模型分别是 Hy4 / Hy3，按对应官方牌价折。
	"hy3": {
		Input: 1, Output: 4, CacheHit: ptr(0.25),
		Source: "cloud.tencent.com/document/product/1823/130055（Hy3 按量计费，2026-11-17）",
	},
	"hy3-x": {
		Input: 1, Output: 4, CacheHit: ptr(0.25),
		Source: "cloud.tencent.com/document/product/1823/130055（Hy3 按量计费，2026-11-17）",
		Note:   "WorkBuddy 的付费档别名，按 Hy3 官方牌价折",
	},
	"hy4std": {
		Input: 6, Output: 18, CacheHit: ptr(0.3),
		Source: "cloud.tencent.com/document/product/1823/130055（Hy4 按量计费，2026-11-17）",
	},
	"hy4free": {
		Input: 6, Output: 18, CacheHit: ptr(0.3),
		Source: "cloud.tencent.com/document/product/1823/130055（Hy4 按量计费，2026-11-17）",
		Note:   "WorkBuddy 内为免费档，官方按 Hy4 牌价折",
	},
	"hy4-preview-x": {
		Input: 6, Output: 18, CacheHit: ptr(0.3),
		Source: "cloud.tencent.com/document/product/1823/130055（Hy4 preview 按量计费，2026-11-17）",
	},
}

// deepSeekPeak DeepSeek 峰谷规则：工作日 9–12、14–18 点高峰，空闲时段半价。
func deepSeekPeak() *Peak {
	return &Peak{Factor: 0.5, Hours: [][2]int{{9, 12}, {14, 18}}, WeekdaysOnly: true}
}

func ptr(v float64) *float64 { return &v }

// Seed 返回内置种子牌价表（文件缺失/损坏时的兜底）。
func Seed() *Table { return NewTable(nil, Meta{Source: "seed", Updated: seedUpdated}) }

// NewTable 以种子为底、overrides 覆盖后构建不可变表。overrides 里非法条目已被
// ParseFile 剔除；这里再兜一层（防止调用方手工构造脏数据）。
func NewTable(overrides map[string]Price, meta Meta) *Table {
	models := make(map[string]Price, len(seedPrices)+len(overrides))
	for k, v := range seedPrices {
		models[k] = v
	}
	for k, v := range overrides {
		key, ok := normalizeKey(k)
		if !ok || validatePrice(v) != nil {
			continue
		}
		models[key] = v
	}
	meta.Enabled = true
	if meta.Currency == "" {
		meta.Currency = CurrencyCNY
	}
	if meta.Unit == "" {
		meta.Unit = UnitPer1M
	}
	if meta.Updated == "" {
		meta.Updated = seedUpdated
	}
	meta.Models = len(models)
	meta.PeakModels = peakModels(models)
	return &Table{models: models, meta: meta}
}

// peakModels 有峰谷价规则的模型名（升序，面板提示用）。
func peakModels(models map[string]Price) []string {
	out := make([]string, 0, 4)
	for k, v := range models {
		if v.Peak != nil {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Meta 返回牌价表元信息。
func (t *Table) Meta() Meta {
	if t == nil {
		return Meta{}
	}
	return t.meta
}

// Lookup 查价：先精确命中 "realm:model"，再回落到裸模型名。
func (t *Table) Lookup(realm, model string) (Price, bool) {
	if t == nil {
		return Price{}, false
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return Price{}, false
	}
	if realm != "" {
		if p, ok := t.models[modelid.Key(realm, model)]; ok {
			return p, true
		}
	}
	p, ok := t.models[model]
	return p, ok
}

// Cost 计算一次桶（一小时或一天）的官方等价支出（元）。ok=false 表示该模型没有牌价。
func (t *Table) Cost(realm, model string, at time.Time, hourly bool, pt, ct, hit, miss int64) (float64, bool) {
	p, ok := t.Lookup(realm, model)
	if !ok {
		return 0, false
	}
	return p.Cost(at, hourly, pt, ct, hit, miss), true
}

// Cost 计算单条桶的等价支出（元）。
//
// 缓存口径：用法统计里的 prompt_tokens 含缓存命中部分，所以必须拆成
// 「命中×缓存价 + 未命中×输入价」；上游没回缓存维度时（CH=CM=0）整段按未命中价，
// 这是**偏高**的保守口径（界面文案已写明）。
func (p Price) Cost(at time.Time, hourly bool, pt, ct, hit, miss int64) float64 {
	return p.costWithFactor(p.FactorAt(at, hourly), pt, ct, hit, miss)
}

// costWithFactor 按给定峰谷系数算钱（系数 = 1 即高峰价）。抽出来是为了让
// Calculator 复用同一套缓存/钳制逻辑，又不必每桶复制 Price 结构体。
func (p Price) costWithFactor(f float64, pt, ct, hit, miss int64) float64 {
	if pt < 0 {
		pt = 0
	}
	if ct < 0 {
		ct = 0
	}
	if hit < 0 {
		hit = 0
	}
	if miss < 0 {
		miss = 0
	}
	// 上游口径打架时的钳制：命中+未命中不得超过 prompt，缺的部分一律按未命中补齐。
	if hit > pt {
		hit = pt
	}
	if miss > pt-hit {
		miss = pt - hit
	}
	if hit+miss < pt {
		miss = pt - hit
	}
	cache := p.Input
	if p.CacheHit != nil {
		cache = *p.CacheHit
	}
	return (float64(hit)*cache + float64(miss)*p.Input + float64(ct)*p.Output) / 1_000_000 * f
}

// FactorAt 返回某时刻的峰谷系数（1 = 高峰价，Factor = 空闲价）。
//
// at 是桶的本地时间；hourly=true 表示小时桶（精确到小时），false 表示日桶
// （按当天高峰小时占比加权）。无峰谷规则的模型恒为 1。
func (p Price) FactorAt(at time.Time, hourly bool) float64 {
	s := p.Peak
	if s == nil || s.Factor <= 0 || s.Factor > 1 || len(s.Hours) == 0 {
		return 1
	}
	bt := at.In(beijing)
	if len(s.Holidays) > 0 {
		day := bt.Format("2006-01-02")
		for _, h := range s.Holidays {
			if h == day {
				return s.Factor
			}
		}
	}
	if s.WeekdaysOnly && (bt.Weekday() == time.Saturday || bt.Weekday() == time.Sunday) {
		return s.Factor
	}
	if hourly {
		if inPeak(s.Hours, bt.Hour()) {
			return 1
		}
		return s.Factor
	}
	peakHours := 0
	for h := 0; h < 24; h++ {
		if inPeak(s.Hours, h) {
			peakHours++
		}
	}
	return (float64(peakHours) + float64(24-peakHours)*s.Factor) / 24
}

// inPeak 判断整点 h 是否落在任一段高峰时段内（左闭右开）。
func inPeak(hours [][2]int, h int) bool {
	for _, w := range hours {
		if h >= w[0] && h < w[1] {
			return true
		}
	}
	return false
}

// Calculator 一次聚合内的查价/系数缓存（单 goroutine 使用）。
//
// 为什么需要：聚合循环最多遍历 40 万个桶，而桶的时间片只有「小时数 × 天数」种、
// 模型数也有限。缓存 (时间片, 模型) → 峰谷系数 与 (realm, 模型) → 牌价，
// 把时间换算和 map 查找压到桶数级别之外。
type Calculator struct {
	table   *Table
	prices  map[string]Price
	missing map[string]struct{}
	factors map[string]float64
}

// NewCalculator 基于当前牌价表创建一次聚合用的计算器。
func (t *Table) NewCalculator() *Calculator {
	return &Calculator{
		table:   t,
		prices:  make(map[string]Price, 32),
		missing: make(map[string]struct{}),
		factors: make(map[string]float64, 256),
	}
}

// Cost 同 Table.Cost，带进程内缓存。
func (c *Calculator) Cost(realm, model string, at time.Time, hourly bool, pt, ct, hit, miss int64) (float64, bool) {
	if c == nil || c.table == nil {
		return 0, false
	}
	key := realm + "\x00" + model
	p, ok := c.prices[key]
	if !ok {
		if _, bad := c.missing[key]; bad {
			return 0, false
		}
		p, ok = c.table.Lookup(realm, model)
		if !ok {
			c.missing[key] = struct{}{}
			return 0, false
		}
		c.prices[key] = p
	}
	// 峰谷系数只依赖 (realm, 模型, 时间片)：realm 覆盖行可能给同一模型配不同的
	// 峰谷规则，所以键里必须带 realm。小时桶与日桶的系数算法不同，前缀区分。
	fkey := realm + "\x00" + model + "\x00"
	if hourly {
		fkey += "h" + at.Format(time.RFC3339Nano)
	} else {
		fkey += "d" + at.Format(time.RFC3339Nano)
	}
	f, seen := c.factors[fkey]
	if !seen {
		f = p.FactorAt(at, hourly)
		if len(c.factors) < 1_000_000 { // 防御性上限：异常输入也不让缓存无界增长
			c.factors[fkey] = f
		}
	}
	return p.costWithFactor(f, pt, ct, hit, miss), true
}

// ---------------------------------------------------------------- 文件格式 ----

// fileFormat data/prices.json 的磁盘结构。
type fileFormat struct {
	Version  int              `json:"version"`
	Updated  string           `json:"updated"`
	Currency string           `json:"currency"`
	Unit     string           `json:"unit"`
	Models   map[string]Price `json:"models"`
}

// ParseFile 解析并校验价格文件，返回可直接交给 NewTable 的覆盖表与快照日期。
//
// 整份文件坏（JSON 解析失败 / 币种不是人民币 / 单位不是每百万 tokens）→ 返回
// error，由 Store 保留上一版并透出错误；单行非法（负价、NaN、峰谷时段越界）→
// 剔除该行并记为警告，不影响其余条目（一条手误不该让整页数字清零）。
func ParseFile(raw []byte) (map[string]Price, string, []string, error) {
	var f fileFormat
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, "", nil, err
	}
	if cur := strings.TrimSpace(f.Currency); cur != "" && cur != CurrencyCNY {
		return nil, "", nil, &FormatError{Msg: "currency 只支持 CNY（人民币），当前配置为 " + cur}
	}
	if u := strings.TrimSpace(f.Unit); u != "" && u != UnitPer1M {
		return nil, "", nil, &FormatError{Msg: "unit 只支持 per_1m_tokens，当前配置为 " + u}
	}
	out := make(map[string]Price, len(f.Models))
	var warns []string
	for k, v := range f.Models {
		key, ok := normalizeKey(k)
		if !ok {
			warns = append(warns, "忽略非法模型键 "+strconvQuote(k))
			continue
		}
		if err := validatePrice(v); err != nil {
			warns = append(warns, "忽略非法条目 "+key+"："+err.Error())
			continue
		}
		out[key] = v
	}
	sort.Strings(warns)
	return out, strings.TrimSpace(f.Updated), warns, nil
}

// FormatError 文件级格式错误（币种/单位不匹配），调用方据此拒绝整份文件。
type FormatError struct{ Msg string }

func (e *FormatError) Error() string { return e.Msg }

// normalizeKey 把牌价键归一化：显式 realm 前缀 → "realm:bare"（斜杠写法等价），
// 裸名保持原样（对两域都生效）。
func normalizeKey(k string) (string, bool) {
	k = strings.TrimSpace(k)
	if k == "" {
		return "", false
	}
	if realm, bare, explicit := modelid.Split(k); explicit {
		if strings.TrimSpace(bare) == "" {
			return "", false
		}
		return modelid.Key(realm, bare), true
	}
	return k, true
}

// validatePrice 单条价格校验：非负、有限；峰谷规则的系数/时段/节假日格式合法。
func validatePrice(p Price) error {
	if !finite(p.Input) || p.Input < 0 {
		return &FormatError{Msg: "input 必须是非负有限数"}
	}
	if !finite(p.Output) || p.Output < 0 {
		return &FormatError{Msg: "output 必须是非负有限数"}
	}
	if p.CacheHit != nil && (!finite(*p.CacheHit) || *p.CacheHit < 0) {
		return &FormatError{Msg: "cache_hit 必须是非负有限数"}
	}
	if s := p.Peak; s != nil {
		if !finite(s.Factor) || s.Factor <= 0 || s.Factor > 1 {
			return &FormatError{Msg: "peak.factor 必须在 (0,1] 区间"}
		}
		if len(s.Hours) == 0 {
			return &FormatError{Msg: "peak.hours 不能为空"}
		}
		for _, w := range s.Hours {
			if w[0] < 0 || w[1] > 24 || w[0] >= w[1] {
				return &FormatError{Msg: "peak.hours 时段必须满足 0<=起<止<=24"}
			}
		}
		for _, h := range s.Holidays {
			if _, err := time.Parse("2006-01-02", h); err != nil {
				return &FormatError{Msg: "peak.holidays 日期格式应为 2006-01-02：" + h}
			}
		}
	}
	return nil
}

// finite 判断浮点是否可用（拒绝 NaN / ±Inf）。
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// strconvQuote 轻量引号包装（避免为一条告警引入 strconv）。
func strconvQuote(s string) string { return "\"" + s + "\"" }
