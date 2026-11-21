// degrade.go 面板「反代劣化」看板的数据接口（GET /panel/api/channel_health）。
//
// 口径（v2，1.22.0 起）——三条数据源各司其职，缺一不可：
//
//	usage       请求数 / 失败数（90 天小时桶）→ 失败率趋势，历史立刻可用
//	channelstat 拦截事件计数（从升级起累计）→ 11-128 / 搬移 / 脱敏 / 限流…
//	reqlog      请求终态归档（7 天）→ 分域对比与终态细分
//
// 为什么不用归档当主源：归档只记**终态**。11-128 被 system→user 搬移救回后终态
// 是 200/success，归档里看不出上游拦过我们——而「拦了、救回来了」恰恰是反代
// 通道劣化的核心信号。归档的 err_kind/realm 字段也只有 1.21.0 之后的事件才有。
package panel

import (
	"net/http"
	"sort"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// channelKindLabels 事件类型的中文标签（面板事件表与图例共用）。
var channelKindLabels = map[string]string{
	"channel_refused":   "通道拒绝（11-128）",
	"sys_as_user":       "system→user 搬移重试",
	"role_normalized":   "developer→system 归一",
	"sanitize":          "指纹清洗改写",
	"content_blocked":   "内容拦截",
	"degrade_trigger":   "提示词降级触发",
	"model_rate_limit":  "模型级限流（6004/6000）",
	"model_busy":        "模型忙（14003）",
	"upstream_busy":     "渠道满载（6021）",
	"waf_block":         "WAF 拦截（403）",
	"model_blocked":     "模型不可用（11102）",
	"soft_rate":         "软限流（429）",
	"hard_credit":       "余额不足（402）",
	"stream_error":      "流中断",
	"stream_read_error": "上游读失败",
	"empty_stream":      "空流",
	"timeout":           "上游超时",
	"cache_miss":        "缓存未命中",
	"catalog_fail":      "目录探测失败",
}

// channelKindOrder 事件表的展示顺序：先「上游识别我们」的证据，再容量/限流，
// 最后是本地观测（缓存、目录）。未列出的类型排在末尾（按次数降序）。
var channelKindOrder = []string{
	"channel_refused", "sys_as_user", "role_normalized", "sanitize",
	"content_blocked", "degrade_trigger",
	"model_rate_limit", "model_busy", "upstream_busy", "waf_block", "model_blocked",
	"soft_rate", "hard_credit",
	"stream_error", "stream_read_error", "empty_stream", "timeout",
	"cache_miss", "catalog_fail",
}

// channelMaxWindow 看板允许的最大窗口（与 usage 小时桶保留时长对齐）。
const channelMaxWindow = 90 * 24 * time.Hour

// channelHourBucketMax 超过该跨度改用日桶（小时桶在这个跨度上已经挤成一条线）。
const channelHourBucketMax = 72 * time.Hour

// channelEventPoint 一个时间片的事件计数（channelstat）。
type channelEventPoint struct {
	T     string           `json:"t"`
	Scope string           `json:"scope"`
	Kinds map[string]int64 `json:"kinds"`
	Total int64            `json:"total"`
}

// channelReqPoint 一个时间片的请求/失败数（usage）。
type channelReqPoint struct {
	T     string `json:"t"`
	Scope string `json:"scope"`
	Req   int64  `json:"req"`
	Err   int64  `json:"err"`
}

// channelKindRow 事件表的一行。
type channelKindRow struct {
	Kind   string  `json:"kind"`
	Label  string  `json:"label"`
	Count  int64   `json:"count"`
	Per1k  float64 `json:"per_1k"`
	LastAt string  `json:"last_at,omitempty"`
}

// channelRealmRow 分域对比的一行（来自 7 天归档，只有 1.21.0+ 的事件带 realm）。
type channelRealmRow struct {
	Requests    int64   `json:"requests"`
	Success     int64   `json:"success"`
	SuccessRate float64 `json:"success_rate"`
	Fallbacks   int64   `json:"fallbacks"`
}

// channelSource 数据源的覆盖区间（前端据此标注「数据起点」与空态原因）。
type channelSource struct {
	Available bool   `json:"available"`
	From      string `json:"from,omitempty"`
	To        string `json:"to,omitempty"`
	Since     string `json:"since,omitempty"`
}

// channelHealthResp GET /panel/api/channel_health 的响应。
type channelHealthResp struct {
	WindowFrom string `json:"window_from"`
	WindowTo   string `json:"window_to"`
	Bucket     string `json:"bucket"`
	Sources    struct {
		Usage   channelSource `json:"usage"`
		Events  channelSource `json:"events"`
		Archive channelSource `json:"archive"`
	} `json:"sources"`
	Requests []channelReqPoint   `json:"requests"`
	Events   []channelEventPoint `json:"events"`
	Totals   struct {
		Requests    int64            `json:"requests"`
		Errors      int64            `json:"errors"`
		FailureRate float64          `json:"failure_rate"`
		Events      int64            `json:"events"`
		Kinds       map[string]int64 `json:"kinds"`
	} `json:"totals"`
	Kinds  []channelKindRow           `json:"kinds"`
	State  DegradeState               `json:"state"`
	Realms map[string]channelRealmRow `json:"realms"`
	// RealmLabeled / RealmTotal 分域覆盖率：1.21.0 之前的事件没有 realm 字段，
	// 前端据此显示「数据积累中」而不是把历史全算进「未标注」。
	RealmLabeled int64 `json:"realm_labeled"`
	RealmTotal   int64 `json:"realm_total"`
}

// channelHealth 反代劣化看板。
func (p *Panel) channelHealth(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	now := time.Now()
	to := parseTimeParam(q.Get("to"))
	if to.IsZero() {
		to = now
	}
	from := parseTimeParam(q.Get("from"))
	if from.IsZero() {
		// 默认近 7 天：与前端默认预设一致，也覆盖归档的保留期。
		from = to.Add(-7 * 24 * time.Hour)
	}
	if from.After(to) {
		from, to = to, from
	}
	if to.Sub(from) > channelMaxWindow {
		from = to.Add(-channelMaxWindow)
	}
	daily := to.Sub(from) > channelHourBucketMax
	bucket := "hour"
	if daily {
		bucket = "day"
	}

	out := channelHealthResp{
		WindowFrom: from.Format(time.RFC3339),
		WindowTo:   to.Format(time.RFC3339),
		Bucket:     bucket,
		Requests:   []channelReqPoint{},
		Events:     []channelEventPoint{},
	}
	out.Totals.Kinds = map[string]int64{}
	out.Realms = map[string]channelRealmRow{}
	out.Kinds = []channelKindRow{}

	// ── 1) 请求 / 失败率：usage 账本（90 天小时桶，历史立刻可用）──────────
	if p.cfg.Usage != nil {
		snap := p.cfg.Usage.SnapshotWindow(usage.Window{From: from, To: to}, nil, nil)
		for _, pt := range snap.Series {
			if pt.Agg.Requests == 0 && pt.Agg.Errors == 0 {
				continue
			}
			out.Requests = append(out.Requests, channelReqPoint{
				T:     pt.T,
				Scope: pt.Scope,
				Req:   pt.Agg.Requests,
				Err:   pt.Agg.Errors,
			})
		}
		out.Totals.Requests = snap.Totals.Requests
		out.Totals.Errors = snap.Totals.Errors
		out.Sources.Usage = channelSource{
			Available: len(out.Requests) > 0,
			From:      snap.WindowFrom,
			To:        snap.WindowTo,
		}
		if snap.Since != "" {
			out.Sources.Usage.Since = snap.Since
		}
	}
	if out.Totals.Requests > 0 {
		out.Totals.FailureRate = round1pct(float64(out.Totals.Errors) / float64(out.Totals.Requests) * 100)
	}

	// ── 2) 拦截事件：channelstat（从升级起累计）──────────────────────────
	if p.cfg.ChannelStats != nil {
		points, totals := p.cfg.ChannelStats.Series(from, to, daily)
		for _, pt := range points {
			out.Events = append(out.Events, channelEventPoint{
				T:     pt.T,
				Scope: pt.Scope,
				Kinds: pt.Kinds,
				Total: pt.Total,
			})
		}
		out.Totals.Events = totals.Total
		out.Totals.Kinds = totals.Kinds
		since, _, _ := p.cfg.ChannelStats.Snapshot()
		src := channelSource{Available: totals.Total > 0}
		if !since.IsZero() {
			src.Since = since.Format(time.RFC3339)
			src.From = since.Format(time.RFC3339)
		}
		if totals.Total > 0 {
			src.To = to.Format(time.RFC3339)
		}
		out.Sources.Events = src

		// 事件表：按固定顺序排，未列出的类型按次数降序垫后。
		rows := make([]channelKindRow, 0, len(totals.Kinds))
		for kind, n := range totals.Kinds {
			if n <= 0 {
				continue
			}
			row := channelKindRow{Kind: kind, Label: channelKindLabel(kind), Count: n}
			if out.Totals.Requests > 0 {
				row.Per1k = round2(float64(n) / float64(out.Totals.Requests) * 1000)
			}
			row.LastAt = totals.LastAt[kind]
			rows = append(rows, row)
		}
		out.Kinds = orderChannelKinds(rows)
	}

	// ── 3) 终态归档：分域对比（7 天，且只有 1.21.0+ 的事件带 realm）──────
	if p.cfg.RequestLog != nil {
		agg, err := p.cfg.RequestLog.Aggregate(from, to, bucketDuration(daily))
		if err == nil {
			out.Sources.Archive = channelSource{
				Available: agg.Totals.Requests > 0,
				From:      agg.From.Format(time.RFC3339),
				To:        agg.To.Format(time.RFC3339),
			}
			out.RealmTotal = agg.Totals.Requests
			for realm, st := range agg.Realms {
				out.Realms[realm] = channelRealmRow{
					Requests:    st.Requests,
					Success:     st.Success,
					SuccessRate: round1pct(st.SuccessRate),
					Fallbacks:   st.Fallbacks,
				}
				if realm == "cn" || realm == "global" {
					out.RealmLabeled += st.Requests
				}
			}
		}
	}

	// ── 4) 当前状态（进程内 + 跨重启两类字段分开给出）──────────────────
	if st, ok := p.currentDegradeState(); ok {
		out.State = st
	}

	writeJSON(w, http.StatusOK, out)
}

// channelKindLabel 事件类型的中文标签；未知类型原样返回（不吞数据）。
func channelKindLabel(kind string) string {
	if s, ok := channelKindLabels[kind]; ok {
		return s
	}
	return kind
}

// orderChannelKinds 事件表排序：固定顺序优先，未列出的按次数降序垫后。
func orderChannelKinds(rows []channelKindRow) []channelKindRow {
	rank := make(map[string]int, len(channelKindOrder))
	for i, k := range channelKindOrder {
		rank[k] = i
	}
	sort.SliceStable(rows, func(i, j int) bool {
		ri, iok := rank[rows[i].Kind]
		rj, jok := rank[rows[j].Kind]
		switch {
		case iok && jok:
			return ri < rj
		case iok:
			return true
		case jok:
			return false
		}
		return rows[i].Count > rows[j].Count
	})
	return rows
}

// bucketDuration 归档聚合的桶粒度（与看板的 hour/day 口径一致）。
func bucketDuration(daily bool) time.Duration {
	if daily {
		return 24 * time.Hour
	}
	return time.Hour
}

// round1pct 百分比保留 1 位（前端直接显示，不在 JS 里再算）。
func round1pct(v float64) float64 {
	return float64(int64(v*10+0.5)) / 10
}

// round2 保留 2 位（每千请求次数）。
func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}
