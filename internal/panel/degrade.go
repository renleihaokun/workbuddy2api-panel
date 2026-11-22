// degrade.go 面板「通道健康」看板的数据接口（GET /panel/api/channel_health）。
//
// 口径（v3）——三条数据源各司其职，缺一不可：
//
//	usage       请求数 / 失败数（90 天小时桶）→ 失败率趋势，历史立刻可用
//	channelstat 拦截/改写事件计数（注册表见 internal/channelstat/kind.go）
//	reqlog      请求终态归档（7 天）→ 分域对比
//
// v3 相对 v2 的变化（重构 + 看板 UX 重做）：
//   - 事件标签/顺序/分组/Hint 全部来自 channelstat 注册表，面板不再维护两份清单；
//   - 每个时间片带 groups（refusal/throttle/stream/other），前端不再做减法；
//   - state 可空（未接线 = null），零值时间字段省略（见 DegradeState.MarshalJSON）；
//   - usage 序列按看板粒度折叠、窗口左边界按桶对齐，三源同口径；
//   - sources 统一 from/to/since，archive 增加 since（数据覆盖条）。
package panel

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/channelstat"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// channelMaxWindow 看板允许的最大窗口（与 usage 小时桶保留时长对齐）。
const channelMaxWindow = 90 * 24 * time.Hour

// channelHourBucketMax 超过该跨度改用日桶（小时桶在这个跨度上已经挤成一条线）。
const channelHourBucketMax = 72 * time.Hour

// channelHourBucketSlack 粒度判定的容差。
//
// 前端「近 3 天」发 from=now-72h、不传 to，服务端补 to=now，必然比 from+72h
// 大几毫秒；不留容差会让 72h 预设永远落进日桶（v1.22.0 的实际行为）。
const channelHourBucketSlack = time.Minute

// channelEventPoint 一个时间片的事件计数（channelstat）。
type channelEventPoint struct {
	T      string           `json:"t"`
	Scope  string           `json:"scope"`
	Kinds  map[string]int64 `json:"kinds"`
	Groups map[string]int64 `json:"groups"`
	Total  int64            `json:"total"`
}

// channelReqPoint 一个时间片的请求/失败数（usage）。
type channelReqPoint struct {
	T     string `json:"t"`
	Scope string `json:"scope"`
	Req   int64  `json:"req"`
	Err   int64  `json:"err"`
}

// channelKindRow 事件表的一行。Label/Group/Unit/Hint 都来自注册表，
// 未登记的 kind 回落到原字符串（不吞数据），Group 落 other、Unit 为空。
type channelKindRow struct {
	Kind   string  `json:"kind"`
	Label  string  `json:"label"`
	Group  string  `json:"group"`
	Unit   string  `json:"unit"`
	Hint   string  `json:"hint"`
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
	// TZ 服务端分桶时区偏移（如 "+08:00"）。前端据此提示「桶时间按服务端时区」，
	// 避免 UTC 宿主 + UTC+8 浏览器时把图表时间读错 8 小时。
	TZ      string                  `json:"tz"`
	Groups  []channelstat.GroupSpec `json:"groups"`
	Sources struct {
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
		Groups      map[string]int64 `json:"groups"`
	} `json:"totals"`
	Kinds []channelKindRow `json:"kinds"`
	// State 未接线时为 null（前端显示「状态不可用」而不是编造「从未触发」）。
	State  *DegradeState              `json:"state"`
	Realms map[string]channelRealmRow `json:"realms"`
	// RealmLabeled / RealmTotal 分域覆盖率：1.21.0 之前的事件没有 realm 字段，
	// 前端据此显示覆盖率而不是把历史全算进「未标注」。
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
	daily := to.Sub(from) > channelHourBucketMax+channelHourBucketSlack
	bucket := "hour"
	if daily {
		bucket = "day"
	}
	// 左边界按桶对齐：usage 按桶起点过滤、channelstat 内部会向下取整，
	// 不先对齐会让三源在窗口左边缘差最多一个桶（per-1k 的分子分母不同源）。
	from = alignBucket(from, daily)

	out := channelHealthResp{
		WindowFrom: from.Format(time.RFC3339),
		WindowTo:   to.Format(time.RFC3339),
		Bucket:     bucket,
		TZ:         time.Now().Format("-07:00"),
		Groups:     channelstat.ChartGroups(),
		Requests:   []channelReqPoint{},
		Events:     []channelEventPoint{},
	}
	out.Totals.Kinds = map[string]int64{}
	out.Totals.Groups = map[string]int64{}
	out.Realms = map[string]channelRealmRow{}
	out.Kinds = []channelKindRow{}

	// ── 1) 请求 / 失败率：usage 账本（90 天小时桶，历史立刻可用）──────────
	if p.cfg.Usage != nil {
		snap := p.cfg.Usage.SnapshotWindow(usage.Window{From: from, To: to}, nil, nil)
		out.Requests = foldUsagePoints(snap.Series, daily)
		out.Totals.Requests = snap.Totals.Requests
		out.Totals.Errors = snap.Totals.Errors
		out.Sources.Usage = channelSource{
			Available: len(out.Requests) > 0,
			From:      from.Format(time.RFC3339),
			To:        to.Format(time.RFC3339),
		}
		if snap.Since != "" {
			out.Sources.Usage.Since = bucketKeyToRFC3339(snap.Since)
		}
	}
	if out.Totals.Requests > 0 {
		out.Totals.FailureRate = round1pct(float64(out.Totals.Errors) / float64(out.Totals.Requests) * 100)
	}

	// ── 2) 拦截/改写事件：channelstat（从升级起累计）────────────────────
	if p.cfg.ChannelStats != nil {
		points, totals := p.cfg.ChannelStats.Series(from, to, daily)
		for _, pt := range points {
			out.Events = append(out.Events, channelEventPoint{
				T:      pt.T,
				Scope:  pt.Scope,
				Kinds:  pt.Kinds,
				Groups: pt.Groups,
				Total:  pt.Total,
			})
		}
		out.Totals.Events = totals.Total
		out.Totals.Kinds = totals.Kinds
		out.Totals.Groups = totals.Groups
		since, _, _ := p.cfg.ChannelStats.Snapshot()
		src := channelSource{
			Available: totals.Total > 0,
			From:      from.Format(time.RFC3339),
			To:        to.Format(time.RFC3339),
		}
		if !since.IsZero() {
			src.Since = since.Format(time.RFC3339)
		}
		out.Sources.Events = src

		// 事件表：注册表顺序优先，未登记的按次数降序垫后。
		rows := make([]channelKindRow, 0, len(totals.Kinds))
		for kind, n := range totals.Kinds {
			if n <= 0 {
				continue
			}
			spec, ok := channelstat.Lookup(channelstat.Kind(kind))
			row := channelKindRow{
				Kind:  kind,
				Label: channelstat.Label(channelstat.Kind(kind)),
				Group: string(channelstat.GroupOf(channelstat.Kind(kind))),
				Count: n,
			}
			if ok {
				row.Unit = string(spec.Unit)
				row.Hint = spec.Hint
			}
			// 每千请求只对「按请求计」的事件有意义；按消息条数计的改写
			// （role_normalized/sanitize）与探测失败（catalog_fail）显示「—」。
			if ok && spec.Unit == channelstat.UnitRequest && out.Totals.Requests > 0 {
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
				From:      from.Format(time.RFC3339),
				To:        to.Format(time.RFC3339),
			}
			if !agg.Since.IsZero() {
				out.Sources.Archive.Since = agg.Since.Format(time.RFC3339)
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

	// ── 4) 当前状态（进程内 + 跨重启两类字段分开给出；未接线 = null）──────
	if st, ok := p.currentDegradeState(); ok {
		out.State = &st
	}

	writeJSON(w, http.StatusOK, out)
}

// orderChannelKinds 事件表排序：注册表顺序优先，未列出的按次数降序垫后。
func orderChannelKinds(rows []channelKindRow) []channelKindRow {
	sort.SliceStable(rows, func(i, j int) bool {
		ri := channelstat.Order(channelstat.Kind(rows[i].Kind))
		rj := channelstat.Order(channelstat.Kind(rows[j].Kind))
		switch {
		case ri >= 0 && rj >= 0:
			return ri < rj
		case ri >= 0:
			return true
		case rj >= 0:
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

// alignBucket 把时刻对齐到桶边界（小时桶对齐到整点，日桶对齐到当天零点）。
func alignBucket(t time.Time, daily bool) time.Time {
	t = t.In(time.Local)
	y, m, d := t.Date()
	if daily {
		return time.Date(y, m, d, 0, 0, 0, 0, time.Local)
	}
	return time.Date(y, m, d, t.Hour(), 0, 0, 0, time.Local)
}

// foldUsagePoints 把 usage 时序折叠到看板粒度。
//
// usage 账本保留 90 天小时桶，与看板的 hour/day 口径无关；不折叠会出现
// 「事件按日、失败率按小时」的同屏两种粒度（v1.22.0 的遗留问题），7 天默认
// 窗口还会因为点数 >80 连圆点都不画。
func foldUsagePoints(pts []usage.Point, daily bool) []channelReqPoint {
	out := make([]channelReqPoint, 0, len(pts))
	if !daily {
		for _, pt := range pts {
			if pt.Agg.Requests == 0 && pt.Agg.Errors == 0 {
				continue
			}
			out = append(out, channelReqPoint{T: pt.T, Scope: pt.Scope, Req: pt.Agg.Requests, Err: pt.Agg.Errors})
		}
		return out
	}
	acc := map[string]*channelReqPoint{}
	for _, pt := range pts {
		if pt.Agg.Requests == 0 && pt.Agg.Errors == 0 {
			continue
		}
		key, scope := pt.T, pt.Scope
		if pt.Scope != "day" {
			if t, err := time.ParseInLocation("2006-01-02T15", pt.T, time.Local); err == nil {
				key, scope = t.Format("2006-01-02"), "day"
			}
		}
		dst := acc[key]
		if dst == nil {
			dst = &channelReqPoint{T: key, Scope: scope}
			acc[key] = dst
		}
		dst.Req += pt.Agg.Requests
		dst.Err += pt.Agg.Errors
	}
	keys := make([]string, 0, len(acc))
	for k := range acc {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, *acc[k])
	}
	return out
}

// bucketKeyToRFC3339 把 usage 的桶键（"2026-10-14T12" / "2026-10-14"）转成
// RFC3339（本地时区）。前端统一按绝对时间解析，不再猜桶键格式。
func bucketKeyToRFC3339(key string) string {
	key = strings.TrimPrefix(strings.TrimPrefix(key, "h:"), "d:")
	for _, layout := range []string{"2006-01-02T15", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, key, time.Local); err == nil {
			return t.Format(time.RFC3339)
		}
	}
	return ""
}

// round1pct 百分比保留 1 位（前端直接显示，不在 JS 里再算）。
func round1pct(v float64) float64 {
	return float64(int64(v*10+0.5)) / 10
}

// round2 保留 2 位（每千请求次数）。
func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}
