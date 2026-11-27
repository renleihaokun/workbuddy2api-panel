package panel

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// batchMeter 记录同时在跑的请求数与峰值。
type batchMeter struct {
	cur  atomic.Int32
	peak atomic.Int32
}

func (m *batchMeter) enter() {
	n := m.cur.Add(1)
	for {
		old := m.peak.Load()
		if n <= old || m.peak.CompareAndSwap(old, n) {
			return
		}
	}
}

func (m *batchMeter) leave() { m.cur.Add(-1) }

// newBatchPanel 搭一个带 n 个 CN 个人号的面板，账号 accessToken = "at-<uid>"。
func newBatchPanel(t *testing.T, n int, handler http.HandlerFunc) (*Panel, *pool.Pool) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	p := pool.New("")
	for i := 0; i < n; i++ {
		uid := fmt.Sprintf("u%02d", i)
		p.Add(&auth.Auth{
			UID:          uid,
			AccessToken:  "at-" + uid,
			RefreshToken: "rt-" + uid,
			ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		})
	}
	return &Panel{cfg: Config{
		Pool: p,
		Upstream: &upstream.Client{
			HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
		},
	}}, p
}

// TestTaskSweepBoundedConcurrency 补领清扫按 pool.batch_concurrency 限流：
// 5 个账号、上限 2 → 峰值并发 ≤2，且每个账号都被扫到。
func TestTaskSweepBoundedConcurrency(t *testing.T) {
	var meter batchMeter
	pn, p := newBatchPanel(t, 5, func(w http.ResponseWriter, r *http.Request) {
		meter.enter()
		defer meter.leave()
		time.Sleep(10 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"code":0,"msg":"OK","data":{"tasks":[]}}`))
	})
	p.SetBatchConcurrency(2)

	st := pn.RunTaskSweepOnce()
	if got := meter.peak.Load(); got > 2 {
		t.Fatalf("补领清扫峰值并发 %d，超过 batch_concurrency=2", got)
	}
	if st.Accounts != 5 {
		t.Fatalf("Accounts=%d want 5", st.Accounts)
	}
}

// TestTaskSweepSingleFlightSkips 清扫单飞：首轮在跑时第二次直接返回零值统计，
// 不产生第二轮上游请求。
func TestTaskSweepSingleFlightSkips(t *testing.T) {
	var reqs atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once

	pn, _ := newBatchPanel(t, 1, func(w http.ResponseWriter, r *http.Request) {
		reqs.Add(1)
		once.Do(func() { close(started) })
		<-release
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"code":0,"msg":"OK","data":{"tasks":[]}}`))
	})

	done := make(chan TaskSweepStats, 1)
	go func() { done <- pn.RunTaskSweepOnce() }()
	<-started // 首轮已持锁并在等上游

	second := pn.RunTaskSweepOnce()
	if second.Accounts != 0 {
		t.Fatalf("重叠触发应跳过（Accounts=0），得到 %+v", second)
	}
	close(release)
	<-done

	// 单号清扫只应发 2 个请求（默认口径 + mp 口径列表），不能因第二次触发翻倍。
	if got := reqs.Load(); got != 2 {
		t.Fatalf("上游请求数 %d，want 2（重叠触发不得重跑）", got)
	}
}

// TestPackagesBoundedConcurrency 积分构成刷新按 pool.batch_concurrency 限流且跳过禁用账号；
// GET /panel/api/packages 默认只读缓存，零上游请求。
func TestPackagesBoundedConcurrency(t *testing.T) {
	var meter batchMeter
	var calls atomic.Int32
	pn, p := newBatchPanel(t, 5, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		meter.enter()
		defer meter.leave()
		time.Sleep(10 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
	})
	p.DisableWithKind("u04", "disabled-test", pool.DisableKindManual)
	p.SetBatchConcurrency(2)

	// GET 默认只读缓存/基线快照，不得直接打上游。
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/panel/api/packages", nil)
	pn.packages(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("GET /panel/api/packages 不得直接打上游，实际=%d", calls.Load())
	}

	out := pn.refreshPackagesSync()
	if got := meter.peak.Load(); got > 2 {
		t.Fatalf("积分构成峰值并发 %d，超过 batch_concurrency=2", got)
	}
	if len(out) != 4 || calls.Load() != 4 {
		t.Fatalf("禁用账号应被跳过: len=%d calls=%d want 4", len(out), calls.Load())
	}

	// 刷新完成后再次 GET 应命中缓存且不新增上游请求。
	rec2 := httptest.NewRecorder()
	pn.packages(rec2, httptest.NewRequest(http.MethodGet, "/panel/api/packages", nil))
	if calls.Load() != 4 {
		t.Fatalf("缓存命中后不应新增上游请求，calls=%d want 4", calls.Load())
	}
	for i := 0; i < 4; i++ {
		if uid := fmt.Sprintf(`"uid":"u%02d"`, i); !strings.Contains(rec2.Body.String(), uid) {
			t.Fatalf("响应缺账号 %s: %s", uid, rec2.Body.String())
		}
	}
}

// TestTasksScanAllBoundedConcurrency 任务中心扫描按同一闸门限流，GET 只读缓存零上游，
// POST /tasks/scan_all 异步单飞触发后可通过 /tasks/scan 读到完整结果。
func TestTasksScanAllBoundedConcurrency(t *testing.T) {
	var meter batchMeter
	var calls atomic.Int32
	pn, p := newBatchPanel(t, 5, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		meter.enter()
		defer meter.leave()
		time.Sleep(10 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"code":0,"msg":"OK","data":{"tasks":[]}}`))
	})
	p.SetBatchConcurrency(2)

	// 只读扫描缓存端点零上游调用。
	recCached := httptest.NewRecorder()
	pn.tasksScanCached(recCached, httptest.NewRequest(http.MethodGet, "/panel/api/tasks/scan", nil))
	if recCached.Code != http.StatusOK || calls.Load() != 0 {
		t.Fatalf("GET /panel/api/tasks/scan 异常 code=%d calls=%d", recCached.Code, calls.Load())
	}

	items, pending := pn.runTasksScanSync()
	if got := meter.peak.Load(); got > 2 {
		t.Fatalf("扫描峰值并发 %d，超过 batch_concurrency=2", got)
	}
	if pending != 0 || len(items) != 5 {
		t.Fatalf("pending=%d len=%d want 0/5", pending, len(items))
	}

	// 异步 HTTP 入口返回 started=true 并可并发单飞。
	rec := httptest.NewRecorder()
	pn.tasksScanAll(rec, httptest.NewRequest(http.MethodPost, "/panel/api/tasks/scan_all", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("POST scan_all status=%d body=%s", rec.Code, rec.Body.String())
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !pn.tasksScanMeta()["scanning"].(bool) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	recAfter := httptest.NewRecorder()
	pn.tasksScanCached(recAfter, httptest.NewRequest(http.MethodGet, "/panel/api/tasks/scan", nil))
	if got := strings.Count(recAfter.Body.String(), `"uid":"u0`); got != 5 {
		t.Fatalf("响应账号行数 %d want 5: %s", got, recAfter.Body.String())
	}
}

// TestPanelReadEndpointsZeroUpstream 验证打开页面/切页触发的所有 GET 控制面接口零上游调用，
// 且 balance_all 异步化并解耦浏览器 UA 昵称全池扫描。
func TestPanelReadEndpointsZeroUpstream(t *testing.T) {
	var upstreamCalls, profileCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if strings.Contains(r.URL.Path, "/console/account") {
			profileCalls.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":1000,"CycleCapacityRemain":500,"CycleCapacityUsed":0}]}}}}`))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u01", Nickname: "n1", AccessToken: "at-1", RefreshToken: "rt-1", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up})
	pn := New(Config{Pool: p, Upstream: up, Scheduler: sch})

	for _, path := range []string{
		"/panel/api/overview",
		"/panel/api/compensation",
		"/panel/api/packages",
		"/panel/api/models",
		"/panel/api/school/vouchers",
		"/panel/api/tasks/scan",
	} {
		rec := httptest.NewRecorder()
		pn.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s -> %d %s", path, rec.Code, rec.Body.String())
		}
	}
	if upstreamCalls.Load() != 0 {
		t.Fatalf("只读 GET 面板端点不应打上游，实际=%d", upstreamCalls.Load())
	}

	// 手动触发全量余额刷新不连带触发 /console/account 浏览器 UA 扫描。
	recBal := httptest.NewRecorder()
	pn.ServeHTTP(recBal, httptest.NewRequest(http.MethodPost, "/panel/api/balance_all", nil))
	if recBal.Code != http.StatusOK {
		t.Fatalf("POST /panel/api/balance_all -> %d %s", recBal.Code, recBal.Body.String())
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !sch.TasksStatus().Balance.Running {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if profileCalls.Load() != 0 {
		t.Fatalf("balance_all 不应连带抓 /console/account，实际=%d", profileCalls.Load())
	}
}
