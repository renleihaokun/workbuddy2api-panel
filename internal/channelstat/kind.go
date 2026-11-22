// kind.go 通道事件词表：类型、中文标签、图表分组、计量单位与一句话说明。
//
// 定位：反代劣化子系统的**单一事实来源**。账本（channelstat.go）、面板
// （internal/panel/degrade.go）、前端看板（app.js）与埋点（internal/server、
// internal/upstream）都从这里取事件类型与展示文案：
//   - 新增一个事件 = 这里加一条注册表 + 对应决策点加一次 Inc；
//   - 面板的中文标签、事件表顺序、堆叠柱分组、每千请求是否换算全部自动跟上；
//   - Kind 的底层类型必须是 string：它是 data/channel-stats.json 的 JSON 键，
//     也是面板 API 的 kind 字段。历史值一经发布即冻结，只能新增不能改名。
package channelstat

// Kind 通道事件类型（稳定字符串 ID）。
type Kind string

// 事件类型常量。字符串值 = 落盘键 = API 字段，冻结不可改。
const (
	// KindChannelRefused 上游通道校验拒绝（11-128）：核心劣化信号。
	KindChannelRefused Kind = "channel_refused"
	// KindSysAsUser system→user 搬移重试实际发出（救回动作）。
	KindSysAsUser Kind = "sys_as_user"
	// KindRoleNormalized developer→system 角色归一（按消息条数计）。
	KindRoleNormalized Kind = "role_normalized"
	// KindSanitize 指纹清洗改写（按消息条数计）。
	KindSanitize Kind = "sanitize"
	// KindContentBlocked 内容策略拦截。
	KindContentBlocked Kind = "content_blocked"
	// KindDegradeTrigger 提示词降级窗口开始。
	KindDegradeTrigger Kind = "degrade_trigger"
	// KindModelBusy 模型级拥塞（14003）。
	KindModelBusy Kind = "model_busy"
	// KindUpstreamBusy 渠道容量满载（6021）。
	KindUpstreamBusy Kind = "upstream_busy"
	// KindWafBlock WAF 拦截（403）。
	KindWafBlock Kind = "waf_block"
	// KindModelBlocked 该账号无此模型（11102）。
	KindModelBlocked Kind = "model_blocked"
	// KindSoftRate 软限流（429，含 6004/6000 模型级限流）。
	KindSoftRate Kind = "soft_rate"
	// KindHardCredit 余额不足（402）。
	KindHardCredit Kind = "hard_credit"
	// KindStreamError 流内 error 帧。
	KindStreamError Kind = "stream_error"
	// KindStreamReadError 读上游流失败。
	KindStreamReadError Kind = "stream_read_error"
	// KindEmptyStream 上游 200 但空流。
	KindEmptyStream Kind = "empty_stream"
	// KindTimeout 上游超时/停滞。
	KindTimeout Kind = "timeout"
	// KindCacheMiss 大前缀整段未命中告警。
	KindCacheMiss Kind = "cache_miss"
	// KindCatalogFail 目录探测失败。
	KindCatalogFail Kind = "catalog_fail"
)

// Group 事件在堆叠柱里的分组。只有四个进图组 + 一个「计数但不进图」。
type Group string

const (
	// GroupRefusal 通道拒绝（上游认为请求不是官方客户端）。
	GroupRefusal Group = "refusal"
	// GroupThrottle 限流、容量与风控。
	GroupThrottle Group = "throttle"
	// GroupStream 流中断与超时。
	GroupStream Group = "stream"
	// GroupOther 内容拦截、降级与本地观测；未登记的 kind 也落这里。
	GroupOther Group = "other"
	// GroupOffchart 计数但不进堆叠柱：按消息计的工作量，或与拒绝一一对应的动作。
	GroupOffchart Group = "offchart"
)

// Unit 计量单位：决定面板「每千请求」是否有意义。
type Unit string

const (
	// UnitRequest 按请求/尝试计：per_1k 可换算。
	UnitRequest Unit = "request"
	// UnitWork 按消息条数/探测次数计：per_1k 无意义（面板显示「—」）。
	UnitWork Unit = "work"
)

// KindSpec 一条事件类型的完整定义。
//
// Hint 是给运维看的一句话「这是什么 / 该怎么办」，面板事件表直接展示；
// 新增事件时必填（有测试兜底）。
type KindSpec struct {
	Kind  Kind   `json:"kind"`
	Label string `json:"label"`
	Group Group  `json:"group"`
	Unit  Unit   `json:"unit"`
	Hint  string `json:"hint"`
}

// registry 注册表。**顺序即事件表顺序**（先「上游识别我们」的证据，再容量/限流，
// 最后是本地观测）。新增事件只能追加或按语义插入，不得重命名已有 Kind。
var registry = []KindSpec{
	{KindChannelRefused, "通道拒绝（11-128）", GroupRefusal, UnitRequest,
		"上游认为请求不是官方客户端；检查提示词模式 / 指纹脱敏 / system→user 兜底"},
	{KindSysAsUser, "system→user 搬移重试", GroupOffchart, UnitRequest,
		"把 system 指令搬进 user 角色重发；与通道拒绝成对出现"},
	{KindRoleNormalized, "developer→system 归一", GroupOffchart, UnitWork,
		"developer 角色归一为 system（协议兼容）；按 developer 消息条数计"},
	{KindSanitize, "指纹清洗改写", GroupOffchart, UnitWork,
		"按指纹名单清洗 system/assistant 文本；按消息条数计"},
	{KindContentBlocked, "内容拦截", GroupOther, UnitRequest,
		"内容审核命中；passthrough 下首次会触发提示词降级"},
	{KindDegradeTrigger, "提示词降级触发", GroupOther, UnitRequest,
		"提示词降级窗口开始；到次日 00:00 CST 自动恢复"},
	{KindModelBusy, "模型忙（14003）", GroupThrottle, UnitRequest,
		"模型级拥塞；换模型或按 Retry-After 等待"},
	{KindUpstreamBusy, "渠道满载（6021）", GroupThrottle, UnitRequest,
		"上游推理队列满载；等窗口过去，换号无效"},
	{KindWafBlock, "WAF 拦截（403）", GroupThrottle, UnitRequest,
		"出口 IP 被 WAF 拦；检查出口 IP 或换 IP"},
	{KindModelBlocked, "模型不可用（11102）", GroupThrottle, UnitRequest,
		"该账号没有这个模型；换模型或换号"},
	{KindSoftRate, "软限流（429）", GroupThrottle, UnitRequest,
		"账号级限流（含 6004/6000 模型级）；看账号池冷却，考虑加号/降并发"},
	{KindHardCredit, "余额不足（402）", GroupThrottle, UnitRequest,
		"账号余额不足；次日 04:00 签到后恢复"},
	{KindStreamError, "流中断", GroupStream, UnitRequest,
		"流内 error 帧；看是否集中在某账号/模型"},
	{KindStreamReadError, "上游读失败", GroupStream, UnitRequest,
		"读上游流失败；网络或上游抖动"},
	{KindEmptyStream, "空流", GroupStream, UnitRequest,
		"上游 200 但空流；上游缺陷，已按 502 观测"},
	{KindTimeout, "上游超时", GroupStream, UnitRequest,
		"上游超时/停滞；轮转已止损，不罚号"},
	{KindCacheMiss, "缓存未命中", GroupOther, UnitRequest,
		"大前缀整段未命中；成本信号"},
	{KindCatalogFail, "目录探测失败", GroupOther, UnitWork,
		"模型目录探测失败；影响模型列表与跨域回退"},
}

// specOf / groupOf / orderOf 注册表的只读索引（包初始化时构建一次）。
var (
	specOf  = make(map[Kind]KindSpec, len(registry))
	groupOf = make(map[Kind]Group, len(registry))
	orderOf = make(map[Kind]int, len(registry))
)

func init() {
	for i, s := range registry {
		specOf[s.Kind] = s
		groupOf[s.Kind] = s.Group
		orderOf[s.Kind] = i
	}
}

// Kinds 返回注册表副本（顺序 = 事件表顺序）。调用方不得依赖内部切片。
func Kinds() []KindSpec {
	out := make([]KindSpec, len(registry))
	copy(out, registry)
	return out
}

// Lookup 查一条事件定义；未登记返回 false。
func Lookup(k Kind) (KindSpec, bool) {
	s, ok := specOf[k]
	return s, ok
}

// Label 返回中文标签；未登记的 kind 原样返回（不吞数据）。
func Label(k Kind) string {
	if s, ok := specOf[k]; ok {
		return s.Label
	}
	return string(k)
}

// Order 返回事件表排序位；未登记返回 -1（面板把未登记项按次数降序垫后）。
func Order(k Kind) int {
	if i, ok := orderOf[k]; ok {
		return i
	}
	return -1
}

// GroupOf 返回事件所属分组；未登记一律落 GroupOther（不丢数据）。
func GroupOf(k Kind) Group {
	if g, ok := groupOf[k]; ok {
		return g
	}
	return GroupOther
}

// GroupSpec 堆叠柱一个系列的定义（前端据此画图例与颜色）。
type GroupSpec struct {
	Key   Group  `json:"key"`
	Label string `json:"label"`
}

// chartGroups 进图组的固定顺序与中文标签（与历史 CH_EVENT_SERIES 一致）。
var chartGroups = []GroupSpec{
	{GroupRefusal, "通道拒绝"},
	{GroupThrottle, "限流·容量"},
	{GroupStream, "流中断·超时"},
	{GroupOther, "其他"},
}

// ChartGroups 返回进图组的元数据副本（顺序固定）。
func ChartGroups() []GroupSpec {
	out := make([]GroupSpec, len(chartGroups))
	copy(out, chartGroups)
	return out
}

// groupsOf 把一个桶的 kind 计数折算成四个进图组。
//
// other 用「总数 − 三组 − offchart」倒推：未登记 kind 因此自动落进 other，
// 不会因为忘记登记而消失；offchart 必须显式减掉，否则会被算进「其他」。
// 空桶返回四组零值（非 nil），与 Kinds 恒非 nil 的口径对齐。
func groupsOf(kinds map[string]int64, total int64) map[string]int64 {
	out := map[string]int64{
		string(GroupRefusal):  0,
		string(GroupThrottle): 0,
		string(GroupStream):   0,
		string(GroupOther):    0,
	}
	var charted, off int64
	for k, n := range kinds {
		switch g := GroupOf(Kind(k)); g {
		case GroupRefusal, GroupThrottle, GroupStream:
			out[string(g)] += n
			charted += n
		case GroupOffchart:
			off += n
		}
	}
	if other := total - charted - off; other > 0 {
		out[string(GroupOther)] = other
	}
	return out
}
