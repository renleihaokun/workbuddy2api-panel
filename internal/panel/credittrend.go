// credittrend.go 剩余积分走势接口（面板「积分构成」页顶部的折线卡）。
//
// 数据源是 internal/creditline 的采样账本（余额后台刷新/签到结束时落点），
// 本文件只做「按时间窗降采样 + 补左端基线 + 附实时当前值」，不触发任何上游调用。
package panel

import (
	"net/http"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/creditline"
)

// creditTrendPoint 面板输出用的采样点。t/scope 与 usage 的时序点同格式
// （hour: 2006-01-02T15，day: 2006-01-02），前端可直接复用 parsePointTime。
type creditTrendPoint struct {
	T        string `json:"t"`
	Scope    string `json:"scope"`
	Total    int64  `json:"total"`
	CN       int64  `json:"cn"`
	Global   int64  `json:"global"`
	Accounts int    `json:"accounts"`
}

// creditTrendBucket 按窗口跨度选桶粒度：>72h 用日桶，否则小时桶。
func creditTrendBucket(span time.Duration) (scope, layout string) {
	if span > 72*time.Hour {
		return "day", "2006-01-02"
	}
	return "hour", "2006-01-02T15"
}

// creditTrend 返回窗口内的积分走势点。
//
// 参数：from / to（parseTimeParam 口径，零值 = 该侧不设界）。
// 两者都不给 = 全部已采样点（账本自身有上限，不会无限长）。
//
// 降采样：每个时间桶取**桶内最后一个点**（即该桶结束时的余额），与用量时序的
// 「按桶聚合」同直觉；同时补一个窗口左端基线点（取 from 之前最近一次采样），
// 让曲线从窗口边界就有值——否则窗口内第一个点之前的空白会被误读成「那时没积分」。
func (p *Panel) creditTrend(w http.ResponseWriter, r *http.Request) {
	if p.cfg.CreditTrend == nil {
		writeErr(w, http.StatusNotImplemented, "credit trend not available")
		return
	}
	q := r.URL.Query()
	from := parseTimeParam(q.Get("from"))
	to := parseTimeParam(q.Get("to"))
	pts := p.cfg.CreditTrend.Read(from, to)

	// 桶粒度：显式区间按区间跨度；未给区间按首末采样点跨度（不足两点用小时桶）。
	span := to.Sub(from)
	if from.IsZero() || to.IsZero() {
		span = 0
		if n := len(pts); n >= 2 {
			span = pts[n-1].Time.Sub(pts[0].Time)
		}
	}
	scope, layout := creditTrendBucket(span)

	out := make([]creditTrendPoint, 0, len(pts)+1)
	keys := make([]string, 0, len(pts)+1)
	push := func(pt creditline.Point) {
		key := pt.Time.In(time.Local).Format(layout)
		if n := len(out); n > 0 && keys[n-1] == key {
			// 同桶后来的点覆盖前一个（pts 升序 → 保留桶内最新余额）。
			out[n-1] = creditTrendPoint{
				T: key, Scope: scope, Total: pt.Total, CN: pt.CN, Global: pt.Global, Accounts: pt.Accounts,
			}
			return
		}
		keys = append(keys, key)
		out = append(out, creditTrendPoint{
			T: key, Scope: scope, Total: pt.Total, CN: pt.CN, Global: pt.Global, Accounts: pt.Accounts,
		})
	}
	// 左端基线：仅在「距窗口起点不超过两个桶宽」时补——进程停机很久后，
	// 上一次采样可能是一周前，把它画进来会把整个窗口压扁。
	if !from.IsZero() {
		if prev := p.cfg.CreditTrend.Read(time.Time{}, from.Add(-time.Nanosecond)); len(prev) > 0 {
			base := prev[len(prev)-1]
			bucketWidth := time.Hour
			if scope == "day" {
				bucketWidth = 24 * time.Hour
			}
			if from.Sub(base.Time) <= 2*bucketWidth {
				push(base)
			}
		}
	}
	for _, pt := range pts {
		push(pt)
	}

	// 实时当前值：比最后一个采样点新（余额可能刚刷新过），用于卡片右上角。
	cur := creditline.Snapshot(p.cfg.Pool.List())

	// 回显实际覆盖区间：显式区间优先，否则用采样点首末时刻。
	winFrom, winTo := from, to
	if winFrom.IsZero() && len(pts) > 0 {
		winFrom = pts[0].Time
	}
	if winTo.IsZero() && len(pts) > 0 {
		winTo = pts[len(pts)-1].Time
	}
	refreshSec, refreshEnabled := 0, false
	if p.cfg.Scheduler != nil {
		if d := p.cfg.Scheduler.BalanceInterval(); d > 0 {
			refreshSec, refreshEnabled = int(d/time.Second), true
		}
	}
	lastAt := time.Time{}
	if last, ok := p.cfg.CreditTrend.Last(); ok {
		lastAt = last.Time
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"window_from": winFrom,
		"window_to":   winTo,
		"bucket":      scope,
		"points":      out,
		"current": map[string]any{
			"total":     cur.Total,
			"cn":        cur.CN,
			"global":    cur.Global,
			"accounts":  cur.Accounts,
			"unlimited": cur.Unlimited,
		},
		"samples":         p.cfg.CreditTrend.Len(),
		"last_sample_at":  lastAt,
		"refresh_sec":     refreshSec,
		"refresh_enabled": refreshEnabled,
	})
}
