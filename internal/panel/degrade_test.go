// degrade_test.go 面板「通道健康」看板接口（/panel/api/channel_health）v3：
// 三源合并（usage 失败率 / channelstat 事件 / 归档分域）、事件表注册表元数据、
// 每点 groups、可空 state、usage 按粒度折叠、窗口左边界对齐。
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
	TZ         string `json:"tz"`
	Groups     []struct {
		Key   string `json:"key"`
		Label string `json:"label"`
	} `json:"groups"`
	Sources struct {
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
		T      string           `json:"t"`
		Scope  string           `json:"scope"`
		Kinds  map[string]int64 `json:"kinds"`
		Groups map[string]int64 `json:"groups"`
		Total  int64            `json:"total"`
	} `json:"events"`
	Totals struct {
		Requests    int64            `json:"requests"`
		Errors      int64            `json:"errors"`
		FailureRate float64          `json:"failure_rate"`
		Events      int64            `json:"events"`
		Kinds       map[string]int64 `json:"kinds"`
		Groups      map[string]int64 `json:"groups"`
	} `json:"totals"`
	Kinds []struct {
		Kind   string  `json:"kind"`
		Label  string  `json:"label"`
		Group  string  `json:"group"`
		Unit   string  `json:"unit"`
		Hint   string  `json:"hint"`
		Count  int64   `json:"count"`
		Per1k  float64 `json:"per_1k"`
		LastAt string  `json:"last_at"`
	} `json:"kinds"`
	State  *DegradeState              `json:"state"`
	Realms map[string]channelRealmRow `json:"realms"`
	Label  int64                      `json:"realm_labeled"`
	Total  int64                      `json:"realm_total"`
}

// 三源齐全时的形状：失败率来自 usage、事件来自 channelstat、分域来自归档；
// 事件表带注册表元数据，每点带 groups。
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
	cs.AddAt(base, channelstat.KindChannelRefused, 3)
	cs.AddAt(base, channelstat.KindSysAsUser, 3)
	cs.AddAt(base, channelstat.KindRoleNormalized, 10)
	cs.AddAt(base, channelstat.KindSanitize, 2)
	cs.AddAt(base.Add(10*time.Minute), channelstat.KindCacheMiss, 1)

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
	if out.TZ == "" {
		t.Error("tz 应回显服务端分桶时区偏移")
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
	// 每点 groups：refusal 3；offchart（搬移 3 + 归一 10 + 清洗 2）=15 被减掉；
	// other = 19-3-15 = 1（缓存未命中）。
	if got := out.Events[1].Groups; got["refusal"] != 3 || got["throttle"] != 0 ||
		got["stream"] != 0 || got["other"] != 1 {
		t.Errorf("events[1].groups=%v want refusal=3/throttle=0/stream=0/other=1", got)
	}
	if out.Totals.Groups["refusal"] != 3 || out.Totals.Groups["other"] != 1 {
		t.Errorf("totals.groups=%v want refusal=3/other=1", out.Totals.Groups)
	}
	if len(out.Events[0].Groups) != 4 {
		t.Errorf("空桶 groups=%v want 四组零值", out.Events[0].Groups)
	}

	// 堆叠柱元数据：固定四组，顺序与注册表一致。
	if len(out.Groups) != 4 || out.Groups[0].Key != "refusal" || out.Groups[1].Key != "throttle" ||
		out.Groups[2].Key != "stream" || out.Groups[3].Key != "other" {
		t.Errorf("groups metadata=%+v want refusal/throttle/stream/other", out.Groups)
	}
	for _, g := range out.Groups {
		if g.Label == "" {
			t.Errorf("group %q 缺中文标签", g.Key)
		}
	}

	// 事件表：注册表顺序、中文标签、分组/单位/Hint、每千请求换算。
	if len(out.Kinds) != 5 {
		t.Fatalf("kinds=%d want 5", len(out.Kinds))
	}
	if out.Kinds[0].Kind != "channel_refused" || out.Kinds[0].Label != "通道拒绝（11-128）" {
		t.Errorf("kinds[0]=%+v want 通道拒绝在最前且带中文标签", out.Kinds[0])
	}
	if out.Kinds[0].Group != "refusal" || out.Kinds[0].Unit != "request" || out.Kinds[0].Hint == "" {
		t.Errorf("kinds[0] 注册表元数据缺失: %+v", out.Kinds[0])
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
	// 按消息计的事件不换算每千请求（显示「—」）。
	if out.Kinds[2].Unit != "work" || out.Kinds[2].Per1k != 0 {
		t.Errorf("role_normalized unit/per_1k=%q/%v want work/0", out.Kinds[2].Unit, out.Kinds[2].Per1k)
	}

	if !out.Sources.Usage.Available || !out.Sources.Events.Available || !out.Sources.Archive.Available {
		t.Errorf("sources 三源都应 available: %+v", out.Sources)
	}
	if out.Sources.Events.Since == "" {
		t.Error("events.since 应回显数据起点")
	}
	if out.Sources.Archive.Since == "" {
		t.Error("archive.since 应回显窗口内最早记录")
	}
	if out.Realms["cn"].Requests != 2 || out.Realms["global"].Fallbacks != 1 {
		t.Errorf("realms=%+v want cn 2 请求 / global fallback 1", out.Realms)
	}
	if out.Label != 4 || out.Total != 4 {
		t.Errorf("realm_labeled=%d realm_total=%d want 4/4", out.Label, out.Total)
	}
	// 未接线：state 必须是 null，而不是零值编造「本进程未触发」。
	if out.State != nil {
		t.Errorf("未接线 state=%+v want null", out.State)
	}
}

// 只有事件计数器、没有 usage / 归档：仍然 200，按源报告可用性（部分数据也有用）。
func TestChannelHealthEventsOnly(t *testing.T) {
	cs := channelstat.New("")
	cs.Inc(channelstat.KindChannelRefused)
	cs.Inc(channelstat.KindSysAsUser)
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
	if out.State == nil {
		t.Fatal("接线后 state 不能为 null")
	}
	if !out.State.Active || out.State.LearnedNow != 3 || out.State.LearnedTotal != 12 || out.State.Mode != "passthrough" {
		t.Fatalf("state=%+v want active/3/12/passthrough", out.State)
	}
	if !out.State.Until.Equal(until) || !out.State.DegradeLastAt.Equal(last) {
		t.Errorf("state 时间字段=%+v want until=%v last=%v", out.State, until, last)
	}
}

// 从未降级：零值时间字段必须省略，前端才能显示「从未」而不是 0001-01-01。
func TestDegradeStateJSONOmitsZeroTimes(t *testing.T) {
	raw, err := json.Marshal(DegradeState{Mode: "passthrough"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if strings.Contains(s, "0001-") || strings.Contains(s, "until") || strings.Contains(s, "degrade_last_at") {
		t.Fatalf("零值时间不应下发: %s", s)
	}
	if !strings.Contains(s, `"mode":"passthrough"`) {
		t.Fatalf("普通字段应保留: %s", s)
	}

	set := DegradeState{
		Active: true, Until: time.Date(2026, 11, 23, 0, 0, 0, 0, time.Local),
		DegradeLastAt: time.Date(2026, 11, 21, 3, 0, 0, 0, time.Local),
	}
	raw, err = json.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	s = string(raw)
	if !strings.Contains(s, `"until":"`) || !strings.Contains(s, `"degrade_last_at":"`) {
		t.Fatalf("非零时间字段应下发: %s", s)
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
	if !strings.Contains(body, `"state":null`) {
		t.Fatalf("未接线 state 应为 null，得到 %s", body)
	}
}

// 7 天窗口：usage 的小时点必须折叠成日点，和事件序列同粒度。
func TestChannelHealthUsageFoldedToDaily(t *testing.T) {
	base := time.Date(2026, 11, 21, 10, 5, 0, 0, time.Local)
	us := usage.New("")
	us.Add(base, "cn", "u1", "glm-5.3", usage.Delta{}, true)
	us.Add(base.Add(time.Hour), "cn", "u1", "glm-5.3", usage.Delta{}, false)
	us.Add(base.Add(24*time.Hour), "cn", "u1", "glm-5.3", usage.Delta{}, true)

	p := channelPanel(t, nil, us, nil)
	from := base.Add(-24 * time.Hour).Unix()
	to := base.Add(72 * time.Hour).Unix()
	code, body := channelGet(t, p, "?from="+strconv.FormatInt(from, 10)+"&to="+strconv.FormatInt(to, 10))
	if code != 200 {
		t.Fatalf("-> %d %s", code, body)
	}
	var out channelResp
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Bucket != "day" {
		t.Fatalf("bucket=%q want day（96h 窗口）", out.Bucket)
	}
	if len(out.Requests) != 2 {
		t.Fatalf("requests=%+v want 2 个日点（3 个小时桶折叠成 2 天）", out.Requests)
	}
	if out.Requests[0].Scope != "day" || out.Requests[1].Scope != "day" {
		t.Errorf("scope=%q/%q want day/day", out.Requests[0].Scope, out.Requests[1].Scope)
	}
	if out.Requests[0].Req != 2 || out.Requests[0].Err != 1 {
		t.Errorf("day1=%+v want req=2 err=1", out.Requests[0])
	}
	if out.Totals.Requests != 3 || out.Totals.Errors != 1 {
		t.Errorf("totals=%+v want 3/1（折叠不改变总量）", out.Totals)
	}
}

// 窗口左边界按桶对齐：from=10:30 时 10:00 的桶也要算进来（三源同口径）。
func TestChannelHealthWindowAlignedFrom(t *testing.T) {
	base := time.Date(2026, 11, 21, 10, 0, 0, 0, time.Local)
	us := usage.New("")
	us.Add(base, "cn", "u1", "glm-5.3", usage.Delta{}, true)
	us.Add(base.Add(time.Hour), "cn", "u1", "glm-5.3", usage.Delta{}, true)

	p := channelPanel(t, nil, us, nil)
	from := base.Add(30 * time.Minute).Unix()
	to := base.Add(3 * time.Hour).Unix()
	code, body := channelGet(t, p, "?from="+strconv.FormatInt(from, 10)+"&to="+strconv.FormatInt(to, 10))
	if code != 200 {
		t.Fatalf("-> %d %s", code, body)
	}
	var out channelResp
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	wantFrom := base.Format(time.RFC3339)
	if out.WindowFrom != wantFrom {
		t.Errorf("window_from=%q want %q（按桶对齐）", out.WindowFrom, wantFrom)
	}
	if out.Totals.Requests != 2 {
		t.Errorf("requests=%d want 2（10:00 桶不能因为 from=10:30 被丢掉）", out.Totals.Requests)
	}
}

// 72h 预设不能因为几毫秒误差永远落日桶。
func TestChannelHealthThreeDayStaysHourly(t *testing.T) {
	p := channelPanel(t, nil, nil, nil)
	now := time.Now()
	from := now.Add(-channelHourBucketMax - 500*time.Millisecond).Unix()
	to := now.Unix()
	code, body := channelGet(t, p, "?from="+strconv.FormatInt(from, 10)+"&to="+strconv.FormatInt(to, 10))
	if code != 200 {
		t.Fatalf("-> %d %s", code, body)
	}
	var out channelResp
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Bucket != "hour" {
		t.Errorf("bucket=%q want hour（72h 预设 + 1 分钟容差）", out.Bucket)
	}
}

// 未登记 kind 不丢：进 other 组，事件表用原字符串 + other 分组。
func TestChannelHealthUnknownKindGoesToOther(t *testing.T) {
	base := time.Date(2026, 11, 21, 10, 0, 0, 0, time.Local)
	cs := channelstat.New("")
	cs.AddAt(base, "weird_kind", 2)

	p := channelPanel(t, nil, nil, cs)
	from := base.Add(-time.Hour).Unix()
	to := base.Add(time.Hour).Unix()
	code, body := channelGet(t, p, "?from="+strconv.FormatInt(from, 10)+"&to="+strconv.FormatInt(to, 10))
	if code != 200 {
		t.Fatalf("-> %d %s", code, body)
	}
	var out channelResp
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Totals.Groups["other"] != 2 {
		t.Errorf("groups.other=%d want 2（未登记 kind 不能丢）", out.Totals.Groups["other"])
	}
	found := false
	for _, k := range out.Kinds {
		if k.Kind == "weird_kind" {
			found = true
			if k.Label != "weird_kind" || k.Group != "other" || k.Unit != "" || k.Hint != "" {
				t.Errorf("unknown row=%+v want raw label / other / 空 unit/hint", k)
			}
		}
	}
	if !found {
		t.Errorf("事件表缺 weird_kind: %+v", out.Kinds)
	}
}
