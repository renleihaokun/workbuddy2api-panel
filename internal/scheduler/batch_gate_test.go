package scheduler

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

// concurrencyMeter 记录同时在跑的请求数与峰值。
type concurrencyMeter struct {
	cur  atomic.Int32
	peak atomic.Int32
}

func (m *concurrencyMeter) enter() {
	n := m.cur.Add(1)
	for {
		old := m.peak.Load()
		if n <= old || m.peak.CompareAndSwap(old, n) {
			return
		}
	}
}

func (m *concurrencyMeter) leave() { m.cur.Add(-1) }

// newBatchTestScheduler 搭一个带 n 个 CN 个人号的调度器，账号 accessToken = "at-<uid>"。
func newBatchTestScheduler(t *testing.T, n int, handler http.HandlerFunc) (*Scheduler, *pool.Pool) {
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
	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
		WebBaseCN:     srv.URL,
	}
	return New(Config{Pool: p, Upstream: up}), p
}

// TestRunBalanceRefreshBoundedConcurrency 余额刷新按 pool.batch_concurrency 限流：
// 6 个账号、上限 2 → 峰值并发 ≤2，且每号恰好查一次。
func TestRunBalanceRefreshBoundedConcurrency(t *testing.T) {
	const accounts = 6
	var meter concurrencyMeter
	var mu sync.Mutex
	calls := map[string]int{}

	s, p := newBatchTestScheduler(t, accounts, func(w http.ResponseWriter, r *http.Request) {
		meter.enter()
		defer meter.leave()
		time.Sleep(10 * time.Millisecond) // 拉长重叠窗口，越界必被观测到
		uid := strings.TrimPrefix(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "at-")
		mu.Lock()
		calls[uid]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":1000,"CycleCapacityRemain":500,"CycleCapacityUsed":0}]}}}}`)
	})
	p.SetBatchConcurrency(2)
	s.RunBalanceRefreshNow()

	if got := meter.peak.Load(); got > 2 {
		t.Fatalf("余额刷新峰值并发 %d，超过 batch_concurrency=2", got)
	}
	mu.Lock()
	defer mu.Unlock()
	for i := 0; i < accounts; i++ {
		uid := fmt.Sprintf("u%02d", i)
		if calls[uid] != 1 {
			t.Errorf("%s 被查 %d 次，want 1", uid, calls[uid])
		}
		if st, ok := p.Status(uid); !ok || st.Credits != 500 {
			t.Errorf("%s credits=%d ok=%v，want 500", uid, st.Credits, ok)
		}
	}
}

// TestRunCheckinSingleFlightSkipsOverlapping 签到单飞：首轮在跑时，第二次触发
// （面板 StartCheckin 走同一把锁）必须返回 false，且不产生第二轮上游请求。
func TestRunCheckinSingleFlightSkipsOverlapping(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once

	s, _ := newBatchTestScheduler(t, 1, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/daily-checkin") {
			once.Do(func() { close(started) })
			<-release
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
	})

	done := make(chan bool, 1)
	go func() { done <- s.RunCheckinNow() }()
	<-started // 首轮已持锁并在等上游
	if s.StartCheckin() {
		t.Fatal("首轮未结束时 StartCheckin 应返回 false（单飞）")
	}
	close(release)
	if first := <-done; !first {
		t.Fatal("首轮 RunCheckinNow 应返回 true")
	}
}

// TestStartActivityRunsBody 面板触发路径必须真正执行任务体——不能出现
// 「startAsync 先拿锁、任务体又自己 TryLock 同一把锁」导致永远跳过。
func TestStartActivityRunsBody(t *testing.T) {
	fastActivity(t)
	stub := &reportStub{}
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	if !s.StartActivity() {
		t.Fatal("首次 StartActivity 应返回 true")
	}
	deadline := time.Now().Add(2 * time.Second)
	for stub.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := stub.calls.Load(); got != 1 {
		t.Fatalf("活跃上报调用 %d 次，want 1（任务体被单飞锁自己挡住？）", got)
	}
}

// TestStartCheckinRunsBody 面板「全部签到」同样要真正跑起来。
func TestStartCheckinRunsBody(t *testing.T) {
	var checkins atomic.Int32
	s, _ := newBatchTestScheduler(t, 1, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/daily-checkin") {
			checkins.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
	})

	if !s.StartCheckin() {
		t.Fatal("首次 StartCheckin 应返回 true")
	}
	deadline := time.Now().Add(2 * time.Second)
	for checkins.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := checkins.Load(); got != 1 {
		t.Fatalf("签到调用 %d 次，want 1", got)
	}
}

// TestCompensationStatusAllBoundedConcurrency 补偿状态查询共用同一闸门。
func TestCompensationStatusAllBoundedConcurrency(t *testing.T) {
	const accounts = 5
	var meter concurrencyMeter

	s, _ := newBatchTestScheduler(t, accounts, func(w http.ResponseWriter, r *http.Request) {
		meter.enter()
		defer meter.leave()
		time.Sleep(10 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"code":0,"msg":"OK","data":{"active":false,"claimed":false,"credit_num":0}}`))
	})
	s.cfg.Pool.SetBatchConcurrency(2)

	rows := s.CompensationStatusAll()
	if got := meter.peak.Load(); got > 2 {
		t.Fatalf("补偿状态查询峰值并发 %d，超过 batch_concurrency=2", got)
	}
	if len(rows) != accounts {
		t.Fatalf("rows=%d want %d", len(rows), accounts)
	}
	for i := 0; i < accounts; i++ {
		if want := fmt.Sprintf("u%02d", i); rows[i].UID != want {
			t.Fatalf("行序漂移：rows[%d]=%s want %s", i, rows[i].UID, want)
		}
	}
}
