// degrade_test.go 反代劣化看板接口（/panel/api/channel_health）v2：
// 三源合并（usage 失败率 / channelstat 事件 / 归档分域）、事件表排序与每千请求、
// 进程内与跨重启状态字段、默认窗口、鉴权。
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

	"github.com/linguo2625469/workbuddy2api-panel/internal/channelstat"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// channelPanel 构造看板面板：归档由测试手写（确定性），usage / channelstat 可选。
func channelPanel(t *testing.T, events []reqlog.Event, us *usage.Recorder, cs *channelstat.Ledger) *Panel {
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
	return New(Config{
		Version: "test", APIKey: "test-key", Pool: pool.New(""),
		RequestLog: rl, Usage: us, ChannelStats: cs,
	})
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
	WindowFrom string `json:"window_from"`
	WindowTo   string `json:"window_to"`
	Bucket     string `json:"bucket"`
	Sources    struct {
		Usage   channelSource `json:"usage"`
		Events  channelSource `json:"events"`
		Archive channelSource `json:"archive"`
	} `json:"sources"`
	Requests []struct {
		T     string `json:"t"`
		Scope string `json:"scope"`
		Req   int64  `json:"req"`
		Err   int64  `json:"err"`
	} `json:"requests"`
	Events []struct {
		T     string           `json:"t"`
		Scope string           `json:"scope"`
		Kinds map[string]int64 `json:"kinds"`
		Total int64            `json:"total"`
	} `json:"events"`
	Totals struct {
		Requests    int64            `json:"requests"`
		Errors      int64            `json:"errors"`
		FailureRate float64          `json:"failure_rate"`
		Events      int64            `json:"events"`
		Kinds       map[string]int64 `json:"kinds"`
	} `json:"totals"`
	Kinds []struct {
		Kind   string  `json:"kind"`
		Label  string  `json:"label"`
		Count  int64   `json:"count"`
		Per1k  float64 `json:"per_1k"`
		LastAt string  `json:"last_at"`
	} `json:"kinds"`
	State  DegradeState               `json:"state"`
	Realms map[string]channelRealmRow `json:"realms"`
	Label  int64                      `json:"realm_labeled"`
	Total  int64                      `json:"realm_total"`
}

// 三源齐全时的形状：失败率来自 usage、事件来自 channelstat、分域来自归档。
func TestChannelHealthV2Shape(t *testing.T) {
	base := time.Date(2026, 11, 21, 10, 5, 0, 0, time.Local)

	// usage：4 次请求 1 次失败（失败率 25%）。
	us := usage.New("")
	for i := 0; i < 3; i++ {
		us.Add(base.Add(time.Duration(i)*time.Minute), "cn", "u1", "glm-5.3", usage.Delta{}, true)
	}
	us.Add(base.Add(4*time.Minute), "cn", "u1", "glm-5.3", usage.Delta{}, false)

	// channelstat：11-128 被拦 3 次、全部搬移救回；脱敏 10 条、清洗 2 条、缓存未命中 1 次。
	cs := channelstat.New("")
	cs.AddAt(base, "channel_refused", 3)
	cs.AddAt(base, "sys_as_user", 3)
	cs.AddAt(base, "role_normalized", 10)
	cs.AddAt(base, "sanitize", 2)
	cs.AddAt(base.Add(10*time.Minute), "cache_miss", 1)

	events := []reqlog.Event{
		{Time: base, Outcome: reqlog.OutcomeSuccess, Realm: "cn", DurationMs: 100},
		{Time: base.Add(time.Minute), Outcome: reqlog.OutcomeHTTPError, ErrKind: "channel_refused", Realm: "cn"},
		{Time: base.Add(2 * time.Minute), Outcome: reqlog.OutcomeSuccess, Realm: "global", Fallback: true},
		{Time: base.Add(3 * time.Minute), Outcome: reqlog.OutcomeSuccess, Realm: "global"},
	}
	p := channelPanel(t, events, us, cs)

	from := base.Add(-time.Hour).Unix()
	to := base.Add(3 * time.Hour).Unix()
	code, body := channelGet(t, p, "?from="+strconv.FormatInt(from, 10)+"&to="+strconv.FormatInt(to, 10))
	if code != 200 {
		t.Fatalf("GET channel_health -> %d %s", code, body)
	}
	var out channelResp
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, body)
	}
	if out.Bucket != "hour" {
		t.Errorf("bucket=%q want hour（<72h 窗口）", out.Bucket)
	}
	if out.Totals.Requests != 4 || out.Totals.Errors != 1 || out.Totals.FailureRate != 25 {
		t.Errorf("totals=%+v want 4 请求 / 1 失败 / 25%%", out.Totals)
	}
	if out.Totals.Events != 19 {
		t.Errorf("totals.events=%d want 19（3+3+10+2+1）", out.Totals.Events)
	}
	if len(out.Requests) == 0 || out.Requests[0].Req != 4 || out.Requests[0].Err != 1 {
		t.Errorf("requests=%+v want 一个 4/1 的桶", out.Requests)
	}
	// 事件序列按窗口补齐空桶（前端按点画柱），数据都落在 10:00 那个桶。
	if len(out.Events) != 5 {
		t.Fatalf("events=%d want 5（窗口 09:00–13:00 逐小时补齐）", len(out.Events))
	}
	if out.Events[1].Kinds["channel_refused"] != 3 || out.Events[1].Total != 19 {
		t.Errorf("events[1]=%+v want channel_refused=3 total=19", out.Events[1])
	}
	if out.Events[0].Total != 0 {
		t.Errorf("events[0]=%+v want 空桶", out.Events[0])
	}

	// 事件表：固定顺序（通道拒绝在最前），标签中文，每千请求换算正确。
	if len(out.Kinds) != 5 {
		t.Fatalf("kinds=%d want 5", len(out.Kinds))
	}
	if out.Kinds[0].Kind != "channel_refused" || out.Kinds[0].Label != "通道拒绝（11-128）" {
		t.Errorf("kinds[0]=%+v want 通道拒绝在最前且带中文标签", out.Kinds[0])
	}
	if out.Kinds[0].Per1k != 750 {
		t.Errorf("per_1k=%v want 750（3 次 / 4 请求）", out.Kinds[0].Per1k)
	}
	if out.Kinds[0].LastAt == "" {
		t.Error("last_at 不得为空")
	}
	if out.Kinds[1].Kind != "sys_as_user" || out.Kinds[2].Kind != "role_normalized" {
		t.Errorf("顺序 want channel_refused → sys_as_user → role_normalized，得到 %+v", out.Kinds)
	}

	if !out.Sources.Usage.Available || !out.Sources.Events.Available || !out.Sources.Archive.Available {
		t.Errorf("sources 三源都应 available: %+v", out.Sources)
	}
	if out.Sources.Events.Since == "" {
		t.Error("events.since 应回显数据起点")
	}
	if out.Realms["cn"].Requests != 2 || out.Realms["global"].Fallbacks != 1 {
		t.Errorf("realms=%+v want cn 2 请求 / global fallback 1", out.Realms)
	}
	if out.Label != 4 || out.Total != 4 {
		t.Errorf("realm_labeled=%d realm_total=%d want 4/4", out.Label, out.Total)
	}
}

// 只有事件计数器、没有 usage / 归档：仍然 200，按源报告可用性（部分数据也有用）。
func TestChannelHealthEventsOnly(t *testing.T) {
	cs := channelstat.New("")
	cs.Inc("channel_refused")
	cs.Inc("sys_as_user")
	p := channelPanel(t, nil, nil, cs)

	code, body := channelGet(t, p, "")
	if code != 200 {
		t.Fatalf("-> %d %s", code, body)
	}
	var out channelResp
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Totals.Events != 2 || len(out.Events) == 0 {
		t.Errorf("events=%+v want 2", out.Totals)
	}
	if out.Sources.Usage.Available || out.Sources.Archive.Available {
		t.Errorf("未装配的源应 available=false: %+v", out.Sources)
	}
	if !out.Sources.Events.Available {
		t.Error("events 源应 available")
	}
	if len(out.Requests) != 0 || out.Totals.Requests != 0 {
		t.Errorf("没有 usage 时请求序列应为空，得到 %+v", out.Requests)
	}
	if out.Totals.FailureRate != 0 {
		t.Errorf("failure_rate=%v want 0（无请求）", out.Totals.FailureRate)
	}
}

// 状态字段：进程内（learned_now / active）与跨重启（learned_total / degrade_last_at）分开透出。
func TestChannelHealthState(t *testing.T) {
	p := channelPanel(t, nil, nil, nil)
	until := time.Date(2026, 11, 22, 0, 0, 0, 0, time.Local)
	last := time.Date(2026, 11, 21, 3, 0, 0, 0, time.Local)
	p.SetDegradeState(func() DegradeState {
		return DegradeState{
			Active: true, Until: until, LearnedNow: 3, LearnedTotal: 12,
			DegradeLastAt: last, Mode: "passthrough",
		}
	})
	code, body := channelGet(t, p, "")
	if code != 200 {
		t.Fatalf("-> %d %s", code, body)
	}
	var out channelResp
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if !out.State.Active || out.State.LearnedNow != 3 || out.State.LearnedTotal != 12 || out.State.Mode != "passthrough" {
		t.Fatalf("state=%+v want active/3/12/passthrough", out.State)
	}
	if !out.State.Until.Equal(until) || !out.State.DegradeLastAt.Equal(last) {
		t.Errorf("state 时间字段=%+v want until=%v last=%v", out.State, until, last)
	}
}

// 不给参数 = 近 7 天（>72h 自动按日桶），窗口回显为真实区间。
func TestChannelHealthDefaultWindowDayBucket(t *testing.T) {
	p := channelPanel(t, nil, nil, nil)
	code, body := channelGet(t, p, "")
	if code != 200 {
		t.Fatalf("-> %d %s", code, body)
	}
	var out channelResp
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Bucket != "day" {
		t.Errorf("bucket=%q want day（默认 7 天窗口）", out.Bucket)
	}
	from, err1 := time.Parse(time.RFC3339, out.WindowFrom)
	to, err2 := time.Parse(time.RFC3339, out.WindowTo)
	if err1 != nil || err2 != nil {
		t.Fatalf("window 不是 RFC3339: %q → %q", out.WindowFrom, out.WindowTo)
	}
	if span := to.Sub(from); span < 6*24*time.Hour || span > 8*24*time.Hour {
		t.Errorf("window span=%v want ≈7 天", span)
	}
}

// 未装配任何源也返回 200（看板显示空态），但鉴权照旧。
func TestChannelHealthAuthAndEmpty(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "test-key", Pool: pool.New("")})
	req := httptest.NewRequest("GET", "/panel/api/channel_health", nil)
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if rr.Code != 401 {
		t.Fatalf("无 key 应回 401，得到 %d", rr.Code)
	}
	code, body := channelGet(t, p, "")
	if code != 200 {
		t.Fatalf("无数据源应回 200，得到 %d %s", code, body)
	}
	if !strings.Contains(body, `"events":[]`) || !strings.Contains(body, `"kinds":[]`) {
		t.Fatalf("空序列应为 []，得到 %s", body)
	}
}
