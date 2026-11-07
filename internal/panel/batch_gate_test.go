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

// TestPackagesBoundedConcurrency 积分构成（面板读接口）与其他批量任务共用闸门。
func TestPackagesBoundedConcurrency(t *testing.T) {
	var meter batchMeter
	pn, p := newBatchPanel(t, 5, func(w http.ResponseWriter, r *http.Request) {
		meter.enter()
		defer meter.leave()
		time.Sleep(10 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
	})
	p.SetBatchConcurrency(2)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/panel/api/packages", nil)
	pn.packages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := meter.peak.Load(); got > 2 {
		t.Fatalf("积分构成峰值并发 %d，超过 batch_concurrency=2", got)
	}
	for i := 0; i < 5; i++ {
		if uid := fmt.Sprintf(`"uid":"u%02d"`, i); !strings.Contains(rec.Body.String(), uid) {
			t.Fatalf("响应缺账号 %s: %s", uid, rec.Body.String())
		}
	}
}

// TestTasksScanAllBoundedConcurrency 任务中心扫描按同一闸门限流，响应结构不变。
func TestTasksScanAllBoundedConcurrency(t *testing.T) {
	var meter batchMeter
	pn, p := newBatchPanel(t, 5, func(w http.ResponseWriter, r *http.Request) {
		meter.enter()
		defer meter.leave()
		time.Sleep(10 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"code":0,"msg":"OK","data":{"tasks":[]}}`))
	})
	p.SetBatchConcurrency(2)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/panel/api/tasks/scan_all", nil)
	pn.tasksScanAll(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := meter.peak.Load(); got > 2 {
		t.Fatalf("扫描峰值并发 %d，超过 batch_concurrency=2", got)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"pending_count":0`) {
		t.Fatalf("响应缺 pending_count: %s", body)
	}
	if got := strings.Count(body, `"uid":"u0`); got != 5 {
		t.Fatalf("响应账号行数 %d want 5", got)
	}
}
