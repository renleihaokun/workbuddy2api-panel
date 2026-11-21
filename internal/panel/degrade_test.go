// degrade_test.go 反代劣化看板接口（/panel/api/channel_health）：
// 窗口聚合形状、失败分类标签、分域对比、降级状态透出、鉴权与未装配语义。
package panel

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
)

// channelPanel 构造带归档事件的看板面板（归档目录由测试手写，确定性）。
func channelPanel(t *testing.T, events []reqlog.Event) *Panel {
	t.Helper()
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "requests-2026-11-21.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	for _, e := range events {
		if err := enc.Encode(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	rl := reqlog.New(reqlog.Config{Dir: dir, Enabled: true})
	t.Cleanup(rl.Close)
	return New(Config{Version: "test", APIKey: "test-key", Pool: pool.New(""), RequestLog: rl})
}

func channelGet(t *testing.T, p *Panel, query string) (int, string) {
	t.Helper()
	req := httptest.NewRequest("GET", "/panel/api/channel_health"+query, nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

type channelResp struct {
	WindowFrom time.Time        `json:"window_from"`
	WindowTo   time.Time        `json:"window_to"`
	Bucket     string           `json:"bucket"`
	Totals     reqlog.AggTotals `json:"totals"`
	Kinds   []struct {
		Kind   string    `json:"kind"`
		Label  string    `json:"label"`
		Count  int64     `json:"count"`
		Rate   float64   `json:"rate"`
		LastAt time.Time `json:"last_at"`
	} `json:"kinds"`
	Buckets []struct {
		T        string           `json:"t"`
		Scope    string           `json:"scope"`
		Requests int64            `json:"requests"`
		Kinds    map[string]int64 `json:"kinds"`
	} `json:"buckets"`
	Realms  map[string]reqlog.AggTotals `json:"realms"`
	Degrade struct {
		Available bool      `json:"available"`
		Active    bool      `json:"active"`
		Until     time.Time `json:"until"`
		Learned   int       `json:"learned"`
		Mode      string    `json:"mode"`
	} `json:"degrade"`
	Archive struct {
		Enabled bool `json:"enabled"`
		Files   int  `json:"files"`
	} `json:"archive"`
}

// 接口形状：总量/分类/分桶/分域齐全，失败分类带中文标签与占比。
// 显式给一个 < 72h 的窗口 → 小时桶（「全部历史」不给参数时才按日桶）。
func TestChannelHealthEndpointShape(t *testing.T) {
	base := time.Date(2026, 11, 21, 10, 5, 0, 0, time.Local)
	p := channelPanel(t, []reqlog.Event{
		{Time: base, Outcome: reqlog.OutcomeSuccess, Realm: "cn", DurationMs: 100},
		{Time: base.Add(3 * time.Minute), Outcome: reqlog.OutcomeHTTPError, ErrKind: "channel_refused", Realm: "cn", DurationMs: 200},
		{Time: base.Add(70 * time.Minute), Outcome: reqlog.OutcomeHTTPError, ErrKind: "soft_rate", Realm: "global",
			Fallback: true, Degraded: true},
		{Time: base.Add(75 * time.Minute), Outcome: reqlog.OutcomeSuccess, Realm: "global", Fallback: true},
	})
	from := base.Add(-time.Hour).Unix()
	to := base.Add(3 * time.Hour).Unix()
	code, body := channelGet(t, p,
		"?from="+strconv.FormatInt(from, 10)+"&to="+strconv.FormatInt(to, 10))
	if code != 200 {
		t.Fatalf("GET channel_health -> %d %s", code, body)
	}
	var out channelResp
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, body)
	}
	if out.Bucket != "hour" {
		t.Errorf("bucket=%q want hour（默认窗口按小时桶）", out.Bucket)
	}
	if out.Totals.Requests != 4 || out.Totals.Success != 2 || out.Totals.HTTPError != 2 {
		t.Errorf("totals=%+v want 4/2/2", out.Totals)
	}
	if out.Totals.Fallbacks != 2 || out.Totals.Degraded != 1 {
		t.Errorf("fallbacks=%d degraded=%d want 2/1", out.Totals.Fallbacks, out.Totals.Degraded)
	}
	if len(out.Kinds) != 2 {
		t.Fatalf("kinds=%+v want 2", out.Kinds)
	}
	byKind := map[string]string{}
	for _, k := range out.Kinds {
		byKind[k.Kind] = k.Label
		if k.Rate != 25 {
			t.Errorf("kind %s rate=%v want 25（1/4 请求）", k.Kind, k.Rate)
		}
		if k.LastAt.IsZero() {
			t.Errorf("kind %s last_at 不得为零值", k.Kind)
		}
	}
	if byKind["channel_refused"] != "通道拒绝（11-128）" {
		t.Errorf("channel_refused label=%q want 通道拒绝（11-128）", byKind["channel_refused"])
	}
	if byKind["soft_rate"] == "" || byKind["soft_rate"] == "soft_rate" {
		t.Errorf("soft_rate 应有中文标签，得到 %q", byKind["soft_rate"])
	}
	if len(out.Buckets) != 2 {
		t.Fatalf("buckets=%d want 2（10 点 / 11 点）", len(out.Buckets))
	}
	if out.Buckets[0].T != "2026-11-21T10" || out.Buckets[0].Kinds["channel_refused"] != 1 {
		t.Errorf("bucket[0]=%+v want 10 点桶 + channel_refused=1", out.Buckets[0])
	}
	if out.Realms["cn"].Requests != 2 || out.Realms["global"].Fallbacks != 2 {
		t.Errorf("realms=%+v want cn 2 请求 / global fallbacks 2", out.Realms)
	}
	if !out.Archive.Enabled || out.Archive.Files != 1 {
		t.Errorf("archive=%+v want enabled + 1 file", out.Archive)
	}
	// 未接线降级状态：available=false（前端显示「状态不可用」，不编造）。
	if out.Degrade.Available {
		t.Error("未接线 degradeState 时 available 应为 false")
	}
}

// 降级状态接线后原样透出（active/until/learned/mode）。
func TestChannelHealthDegradeState(t *testing.T) {
	p := channelPanel(t, nil)
	until := time.Date(2026, 11, 22, 0, 0, 0, 0, time.Local)
	p.SetDegradeState(func() DegradeState {
		return DegradeState{Active: true, Until: until, Learned: 3, Mode: "passthrough"}
	})
	code, body := channelGet(t, p, "")
	if code != 200 {
		t.Fatalf("-> %d %s", code, body)
	}
	var out channelResp
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Degrade.Available || !out.Degrade.Active || out.Degrade.Learned != 3 || out.Degrade.Mode != "passthrough" {
		t.Fatalf("degrade=%+v want available/active/3/passthrough", out.Degrade)
	}
	if !out.Degrade.Until.Equal(until) {
		t.Errorf("degrade.until=%v want %v", out.Degrade.Until, until)
	}
}

// 时间窗过滤：from/to 只统计区间内的事件。
func TestChannelHealthWindowFilter(t *testing.T) {
	base := time.Date(2026, 11, 21, 10, 0, 0, 0, time.Local)
	p := channelPanel(t, []reqlog.Event{
		{Time: base, Outcome: reqlog.OutcomeSuccess, Realm: "cn"},
		{Time: base.Add(2 * time.Hour), Outcome: reqlog.OutcomeHTTPError, ErrKind: "server", Realm: "cn"},
	})
	from := base.Add(time.Hour).Unix()
	to := base.Add(3 * time.Hour).Unix()
	code, body := channelGet(t, p, "?from="+strconv.FormatInt(from, 10)+"&to="+strconv.FormatInt(to, 10))
	if code != 200 {
		t.Fatalf("-> %d %s", code, body)
	}
	var out channelResp
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Totals.Requests != 1 || out.Totals.HTTPError != 1 {
		t.Fatalf("totals=%+v want 只统计 12:00 那条", out.Totals)
	}
}

// 不给参数 = 全部归档历史：按日桶，且回显真实数据区间（不是 0001-01-01）。
func TestChannelHealthAllHistoryDayBucket(t *testing.T) {
	base := time.Date(2026, 11, 20, 22, 0, 0, 0, time.Local)
	p := channelPanel(t, []reqlog.Event{
		{Time: base, Outcome: reqlog.OutcomeSuccess, Realm: "cn"},
		{Time: base.Add(3 * time.Hour), Outcome: reqlog.OutcomeSuccess, Realm: "cn"}, // 次日 01:00
	})
	code, body := channelGet(t, p, "")
	if code != 200 {
		t.Fatalf("-> %d %s", code, body)
	}
	var out channelResp
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Bucket != "day" {
		t.Errorf("bucket=%q want day（全部历史按日桶）", out.Bucket)
	}
	if len(out.Buckets) != 2 || out.Buckets[0].T != "2026-11-20" {
		t.Errorf("buckets=%+v want 两个自然日桶", out.Buckets)
	}
	if out.WindowFrom.Year() < 2000 || out.WindowTo.Year() < 2000 {
		t.Errorf("window=%v→%v 应回显真实数据区间", out.WindowFrom, out.WindowTo)
	}
}

// 归档关闭：200 + 空总量 + archive.enabled=false（前端显示「归档已关闭」而不是一片 0）。
func TestChannelHealthArchiveDisabled(t *testing.T) {
	rl := reqlog.New(reqlog.Config{Dir: t.TempDir(), Enabled: false})
	t.Cleanup(rl.Close)
	p := New(Config{Version: "test", APIKey: "test-key", Pool: pool.New(""), RequestLog: rl})
	code, body := channelGet(t, p, "")
	if code != 200 {
		t.Fatalf("-> %d %s", code, body)
	}
	var out channelResp
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Archive.Enabled || out.Totals.Requests != 0 {
		t.Fatalf("archive=%+v totals=%+v want 关闭 + 空", out.Archive, out.Totals)
	}
	if !strings.Contains(body, `"kinds":[]`) || !strings.Contains(body, `"buckets":[]`) {
		t.Fatalf("空结果应为 []，得到 %s", body)
	}
}

// 未装配 RequestLog = 501；不带 key = 401。
func TestChannelHealthNotConfiguredAndAuth(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "test-key", Pool: pool.New("")})
	code, body := channelGet(t, p, "")
	if code != 501 || !strings.Contains(body, "request logger not available") {
		t.Fatalf("未装配应回 501，得到 %d %s", code, body)
	}

	req := httptest.NewRequest("GET", "/panel/api/channel_health", nil)
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if rr.Code != 401 {
		t.Fatalf("无 key 应回 401，得到 %d", rr.Code)
	}
}
