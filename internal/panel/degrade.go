// degrade.go 反代劣化看板接口（面板「反代劣化」视图）。
//
// 定位：上游对反代通道的识别与拦截（11-128 通道校验、内容审核、模型下线、
// 限流、WAF、超时）是**随时间劣化**的——只看「当前成功率」回答不了「是不是
// 越来越差」。这里把请求归档按时间桶摊开，并给出失败分类、分域对比、当前提示词
// 降级状态。
//
// 数据全部来自 reqlog 归档（脱敏元数据），不触发任何上游调用；聚合在 reqlog
// 内部流式完成（内存只与桶数相关），面板侧只做标签与派生比例。
package panel

import (
	"net/http"
	"time"
)

// channelKindLabels 失败分类的展示名（单一事实源：后端给 label，前端只排版）。
//
// 键 = reqlog.Event.ErrKind：上游错误分类（upstream.ErrKind.String()）+ 本地合成值
// （timeout / transport_error / no_healthy_account / stream_read_error / empty_stream /
// upstream_parse）+ unknown（旧归档没有该字段时的兜底）。
var channelKindLabels = map[string]string{
	"channel_refused":    "通道拒绝（11-128）",
	"content_blocked":    "内容拦截",
	"model_blocked":      "模型不可用（11102 / 整池冷却）",
	"soft_rate":          "限流（6004 / 429）",
	"model_busy":         "模型拥塞（14003）",
	"upstream_busy":      "渠道满载（6021）",
	"waf_block":          "WAF 拦截（403）",
	"hard_credit":        "余额不足（402）",
	"session_dead":       "会话失效（401）",
	"account_fault":      "账号故障（11140 / 14017）",
	"server":             "上游 5xx",
	"not_found":          "上游 404",
	"bad_params":         "请求体错误（11101）",
	"prompt_too_long":    "上下文超限（11115）",
	"image_invalid":      "图片无效",
	"client":             "其他 4xx",
	"timeout":            "上游超时",
	"transport_error":    "连接上游失败",
	"stream_read_error":  "流读取失败",
	"empty_stream":       "空流（200 + 0 帧）",
	"upstream_parse":     "流解析失败",
	"no_healthy_account": "无可用账号",
	"unknown":            "未分类（旧归档）",
}

const (
	// channelMaxWindow 聚合窗口上限：归档保留期通常远小于此，纯粹防手工传
	// 一个十年区间让面板去扫全部文件。
	channelMaxWindow = 90 * 24 * time.Hour
	// channelHourBucketMax 超过该跨度改用日桶（与用量时序同阈值）。
	channelHourBucketMax = 72 * time.Hour
)

// round1pct 百分比保留一位小数（与 reqlog 的 round1 同口径，避免两处舍入漂移）。
func round1pct(v float64) float64 { return float64(int(v*10+0.5)) / 10 }

// channelHealth 返回窗口内的请求结果聚合 + 失败分类 + 分域对比 + 当前降级状态。
//
// 参数：from / to（parseTimeParam 口径）。两者都不给 = 全部归档历史（按日桶），
// 与前端「全部历史」预设一致；只给 to 时按默认 7 天回看。
func (p *Panel) channelHealth(w http.ResponseWriter, r *http.Request) {
	if p.cfg.RequestLog == nil {
		writeErr(w, http.StatusNotImplemented, "request logger not available")
		return
	}
	q := r.URL.Query()
	from := parseTimeParam(q.Get("from"))
	to := parseTimeParam(q.Get("to"))
	if to.IsZero() {
		to = time.Now()
	}
	// from 缺省 = 不设下界（全部历史）。窗口上限只防病态区间，不影响正常使用。
	if !from.IsZero() && to.Sub(from) > channelMaxWindow {
		from = to.Add(-channelMaxWindow)
	}
	bucket := time.Hour
	switch {
	case from.IsZero():
		// 无下界 = 全部历史：按日桶（小时桶在数月跨度上会碎成几千根柱子）。
		bucket = 24 * time.Hour
	case to.Sub(from) > channelHourBucketMax:
		bucket = 24 * time.Hour
	}

	agg, err := p.cfg.RequestLog.Aggregate(from, to, bucket)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	// 失败分类 → 带展示名与占请求数比例（前端表格直接渲染）。
	kinds := make([]map[string]any, 0, len(agg.Kinds))
	for _, k := range agg.Kinds {
		label := channelKindLabels[k.Kind]
		if label == "" {
			label = k.Kind
		}
		rate := 0.0
		if agg.Totals.Requests > 0 {
			rate = round1pct(float64(k.Count) / float64(agg.Totals.Requests) * 100)
		}
		kinds = append(kinds, map[string]any{
			"kind":    k.Kind,
			"label":   label,
			"count":   k.Count,
			"rate":    rate,
			"last_at": k.LastAt,
		})
	}

	// 当前降级状态（handler 的 degradeGate / systemModeGate 快照）。
	deg := map[string]any{"available": false}
	if st, ok := p.currentDegradeState(); ok {
		deg = map[string]any{
			"available": true,
			"active":    st.Active,
			"until":     st.Until,
			"learned":   st.Learned,
			"mode":      st.Mode,
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"window_from": agg.From,
		"window_to":   agg.To,
		"bucket":      agg.Bucket,
		"totals":      agg.Totals,
		"kinds":       kinds,
		"buckets":     agg.Buckets,
		"realms":      agg.Realms,
		"degrade":     deg,
		"archive":     agg.Archive,
	})
}
