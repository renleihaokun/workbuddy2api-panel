package batch

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNormalize(t *testing.T) {
	cases := []struct{ in, want int }{
		{-5, Default},
		{0, Default},
		{1, 1},
		{3, 3},
		{Max, Max},
		{Max + 1, Max},
	}
	for _, c := range cases {
		if got := Normalize(c.in); got != c.want {
			t.Errorf("Normalize(%d)=%d want %d", c.in, got, c.want)
		}
		if got := NewGate(c.in).Limit(); got != c.want {
			t.Errorf("NewGate(%d).Limit()=%d want %d", c.in, got, c.want)
		}
	}
}

// TestGateForEachBounded 上限必须真的生效：8 项、上限 2 → 同时在跑的 fn ≤2，
// 且每个下标恰好执行一次。
func TestGateForEachBounded(t *testing.T) {
	g := NewGate(2)
	var cur, peak atomic.Int32
	var seen [8]atomic.Int32

	g.ForEach(len(seen), func(i int) {
		n := cur.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		seen[i].Add(1)
		time.Sleep(5 * time.Millisecond) // 拉长重叠窗口，让越界必然被观测到
		cur.Add(-1)
	})

	if got := peak.Load(); got > 2 {
		t.Fatalf("同时在跑 %d 个，超过上限 2", got)
	}
	for i := range seen {
		if got := seen[i].Load(); got != 1 {
			t.Errorf("下标 %d 执行 %d 次，want 1", i, got)
		}
	}
}

// TestGateForEachLimitOne 上限 1 时严格串行。
func TestGateForEachLimitOne(t *testing.T) {
	g := NewGate(1)
	var cur, peak atomic.Int32
	g.ForEach(4, func(int) {
		n := cur.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		cur.Add(-1)
	})
	if got := peak.Load(); got != 1 {
		t.Fatalf("peak=%d want 1（严格串行）", got)
	}
}

// TestGateSetRuntimeResize 运行期改上限：Limit 立刻返回新值，排队者被唤醒后按新上限跑。
func TestGateSetRuntimeResize(t *testing.T) {
	g := NewGate(1)
	if got := g.Limit(); got != 1 {
		t.Fatalf("limit=%d want 1", got)
	}
	g.Set(4)
	if got := g.Limit(); got != 4 {
		t.Fatalf("limit=%d want 4", got)
	}
	g.Set(0) // <=0 回落默认
	if got := g.Limit(); got != Default {
		t.Fatalf("limit=%d want %d", got, Default)
	}
	g.Set(99) // >Max 钳制
	if got := g.Limit(); got != Max {
		t.Fatalf("limit=%d want %d", got, Max)
	}
}

// TestGateReleaseIdempotent 重复调用释放函数只能归还一个名额，不能把计数减穿。
func TestGateReleaseIdempotent(t *testing.T) {
	g := NewGate(1)
	release := g.Acquire()
	release()
	release()
	g.mu.Lock()
	n := g.inFlight
	g.mu.Unlock()
	if n != 0 {
		t.Fatalf("inFlight=%d want 0（重复释放必须被 Once 吃掉）", n)
	}
}

// TestCoalescerMergesConcurrentRound 第一轮被阻塞在途时，第二个 Do 只等待不重跑。
func TestCoalescerMergesConcurrentRound(t *testing.T) {
	var c Coalescer
	var calls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})

	go c.Do(func() {
		calls.Add(1)
		close(entered)
		<-release
	})
	<-entered // 此刻 c.ch 已置位、第一轮被 release 卡住

	// 50ms 后才结束第一轮；期间主 goroutine 直接发起第二次 Do，必然命中在途轮次。
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(release)
	}()
	c.Do(func() { calls.Add(1) })

	if got := calls.Load(); got != 1 {
		t.Fatalf("fn 执行 %d 次，want 1（并发触发应合并）", got)
	}
}

// TestCoalescerSequentialRunsEach 串行触发是两轮，不能被永久合并。
func TestCoalescerSequentialRunsEach(t *testing.T) {
	var c Coalescer
	var calls atomic.Int32
	c.Do(func() { calls.Add(1) })
	c.Do(func() { calls.Add(1) })
	if got := calls.Load(); got != 2 {
		t.Fatalf("fn 执行 %d 次，want 2", got)
	}
}

// TestZeroValueGateUsable 零值 Gate（未走 NewGate）按默认上限工作，不能因 0 上限永久阻塞。
func TestZeroValueGateUsable(t *testing.T) {
	var g Gate
	if got := g.Limit(); got != Default {
		t.Fatalf("零值 Limit=%d want %d", got, Default)
	}
	done := make(chan struct{})
	go func() {
		release := g.Acquire()
		release()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("零值 Gate Acquire 阻塞（cond/limit 未初始化）")
	}
	g.Set(2)
	if got := g.Limit(); got != 2 {
		t.Fatalf("零值 Set 后 Limit=%d want 2", got)
	}
}

// TestNilGateSafe 零值/未接线场景（测试或装配缺失）按默认上限跑，不 panic。
func TestNilGateSafe(t *testing.T) {
	var g *Gate
	if got := g.Limit(); got != Default {
		t.Fatalf("nil Limit=%d want %d", got, Default)
	}
	g.Set(5) // no-op
	release := g.Acquire()
	release()
	var seen sync.Map
	g.ForEach(3, func(i int) { seen.Store(i, true) })
	for i := 0; i < 3; i++ {
		if _, ok := seen.Load(i); !ok {
			t.Fatalf("nil gate ForEach 漏掉下标 %d", i)
		}
	}
}
