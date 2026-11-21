// credit_trend_test.go 剩余积分走势接口（/panel/api/credit_trend）：
// 降采样（同桶取最后一个点）、左端基线、实时当前值、鉴权与未装配语义。
package panel

import (
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/creditline"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// trendPanel 构造带走势账本与一个 500 分账号的面板。
func trendPanel(t *testing.T, ledger *creditline.Ledger) *Panel {
	t.Helper()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", Nickname: "小明"})
	p.SetCredits("u1", 500, 1000)
	return New(Config{Version: "test", APIKey: "test-key", Pool: p, CreditTrend: ledger})
}

func trendGet(t *testing.T, p *Panel, query string) (int, string) {
	t.Helper()
	req := httptest.NewRequest("GET", "/panel/api/credit_trend"+query, nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

type trendResp struct {
	WindowFrom time.Time `json:"window_from"`
	WindowTo   time.Time `json:"window_to"`
	Bucket     string    `json:"bucket"`
	Points     []struct {
		T        string `json:"t"`
		Scope    string `json:"scope"`
		Total    int64  `json:"total"`
		CN       int64  `json:"cn"`
		Global   int64  `json:"global"`
		Accounts int    `json:"accounts"`
	} `json:"points"`
	Current struct {
		Total     int64 `json:"total"`
		CN        int64 `json:"cn"`
		Global    int64 `json:"global"`
		Accounts  int   `json:"accounts"`
		Unlimited int   `json:"unlimited"`
	} `json:"current"`
	Samples      int  `json:"samples"`
	RefreshSec   int  `json:"refresh_sec"`
	RefreshOn    bool `json:"refresh_enabled"`
	LastSampleAt time.Time `json:"last_sample_at"`
}

// seedTrend 落四个采样点：10:00 / 10:10（同一小时桶）/ 11:05 / 12:20。
func seedTrend(t *testing.T) (*creditline.Ledger, time.Time) {
	t.Helper()
	l := creditline.New("", 100)
	base := time.Date(2026, 11, 21, 10, 0, 0, 0, time.Local)
	l.Add(creditline.Point{Time: base, Total: 900, CN: 900, Accounts: 2})
	l.Add(creditline.Point{Time: base.Add(10 * time.Minute), Total: 850, CN: 850, Accounts: 2})
	l.Add(creditline.Point{Time: base.Add(65 * time.Minute), Total: 800, CN: 700, Global: 100, Accounts: 2})
	l.Add(creditline.Point{Time: base.Add(140 * time.Minute), Total: 780, CN: 680, Global: 100, Accounts: 2})
	return l, base
}

// 无窗口：按首末点跨度选小时桶，同桶只保留最后一个点。
func TestCreditTrendShapeAndDownsample(t *testing.T) {
	l, base := seedTrend(t)
	code, body := trendGet(t, trendPanel(t, l), "")
	if code != 200 {
		t.Fatalf("GET credit_trend -> %d %s", code, body)
	}
	var out trendResp
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, body)
	}
	if out.Bucket != "hour" {
		t.Errorf("bucket=%q want hour", out.Bucket)
	}
	if len(out.Points) != 3 {
		t.Fatalf("points=%+v want 3（10/11/12 三个小时桶）", out.Points)
	}
	if out.Points[0].T != "2026-11-21T10" || out.Points[0].Total != 850 {
		t.Errorf("points[0]=%+v want 10 点桶 total 850（桶内最后一个点）", out.Points[0])
	}
	if out.Points[1].T != "2026-11-21T11" || out.Points[1].Global != 100 {
		t.Errorf("points[1]=%+v want 11 点桶 global 100", out.Points[1])
	}
	if out.Points[2].T != "2026-11-21T12" || out.Points[2].Total != 780 {
		t.Errorf("points[2]=%+v want 12 点桶 total 780", out.Points[2])
	}
	if out.Points[0].Scope != "hour" {
		t.Errorf("scope=%q want hour", out.Points[0].Scope)
	}
	// 无显式窗口：回显采样首末时刻。
	if !out.WindowFrom.Equal(base) || !out.WindowTo.Equal(base.Add(140*time.Minute)) {
		t.Errorf("window=%v→%v want 采样首末", out.WindowFrom, out.WindowTo)
	}
	// 实时当前值来自池快照（比最后一个采样点新）。
	if out.Current.Total != 500 || out.Current.Accounts != 1 {
		t.Errorf("current=%+v want total 500 accounts 1", out.Current)
	}
	if out.Samples != 4 {
		t.Errorf("samples=%d want 4", out.Samples)
	}
	if out.LastSampleAt.IsZero() {
		t.Error("last_sample_at 不得为零值")
	}
}

// 显式窗口：左端补基线（取窗口前最近一点），曲线从边界开始就有值。
func TestCreditTrendBaselineAndWindow(t *testing.T) {
	l, base := seedTrend(t)
	from := base.Add(30 * time.Minute).Unix() // 10:30：窗口内只剩 11:05 / 12:20
	to := base.Add(150 * time.Minute).Unix()  // 12:30
	code, body := trendGet(t, trendPanel(t, l),
		"?from="+strconv.FormatInt(from, 10)+"&to="+strconv.FormatInt(to, 10))
	if code != 200 {
		t.Fatalf("-> %d %s", code, body)
	}
	var out trendResp
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Points) != 3 {
		t.Fatalf("points=%+v want 3（基线 10 点桶 + 11:05 + 12:20）", out.Points)
	}
	if out.Points[0].T != "2026-11-21T10" || out.Points[0].Total != 850 {
		t.Errorf("基线点=%+v want 10 点桶 total 850（窗口前最近一点）", out.Points[0])
	}
	if !out.WindowFrom.Equal(time.Unix(from, 0)) || !out.WindowTo.Equal(time.Unix(to, 0)) {
		t.Errorf("window 应回显显式区间，得到 %v→%v", out.WindowFrom, out.WindowTo)
	}
}

// 跨天窗口用日桶（span > 72h）。
func TestCreditTrendDayBucket(t *testing.T) {
	l := creditline.New("", 100)
	base := time.Date(2026, 11, 10, 9, 0, 0, 0, time.Local)
	l.Add(creditline.Point{Time: base, Total: 10, CN: 10, Accounts: 1})
	l.Add(creditline.Point{Time: base.AddDate(0, 0, 10), Total: 20, CN: 20, Accounts: 1})
	code, body := trendGet(t, trendPanel(t, l), "")
	if code != 200 {
		t.Fatalf("-> %d %s", code, body)
	}
	var out trendResp
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Bucket != "day" {
		t.Errorf("bucket=%q want day", out.Bucket)
	}
	if len(out.Points) != 2 || out.Points[0].T != "2026-11-10" || out.Points[0].Scope != "day" {
		t.Errorf("points=%+v want 两个日桶", out.Points)
	}
}

// 空账本必须回 []（而不是 null）：前端把 null 与「接口不可用」混在一起会显示成加载失败。
func TestCreditTrendEmptyReturnsArray(t *testing.T) {
	code, body := trendGet(t, trendPanel(t, creditline.New("", 10)), "")
	if code != 200 {
		t.Fatalf("-> %d %s", code, body)
	}
	if !strings.Contains(body, `"points":[]`) {
		t.Fatalf("空结果应为 []，得到 %s", body)
	}
}

// 未装配账本 = 501；不带 key = 401。
func TestCreditTrendNotConfiguredAndAuth(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "test-key", Pool: pool.New("")})
	code, body := trendGet(t, p, "")
	if code != 501 || !strings.Contains(body, "credit trend not available") {
		t.Fatalf("未装配应回 501，得到 %d %s", code, body)
	}
	req := httptest.NewRequest("GET", "/panel/api/credit_trend", nil)
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if rr.Code != 401 {
		t.Fatalf("无 key 应回 401，得到 %d", rr.Code)
	}
}
