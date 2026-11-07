// Package batch 控制面批量任务的并发闸门与单飞工具。
//
// 背景：余额刷新 / 任务补领清扫 / 任务中心扫描等「按账号扇出」的批量任务此前
// 每账号起一个 goroutine，账号一多就是 N～2N 个上游请求同时打出；它们又不占
// pool 的在途租约（Acquire/Release 只服务数据面 chat/search），因此可以和
// 正常对话流量叠加、也能绕过 global 域为 WAF 压低的并发档位。
//
// 本包只做两件事：
//   - Gate：进程级共享的账号发扇出上限（默认 3，运行期可改），批量任务统一从这里
//     取名额；上限与具体任务无关，因此多个任务同时跑也不会叠加成 N×3。
//   - Coalescer：同一任务并发触发时合并成一轮（等待方在上一轮结束后返回），
//     避免重复点击 / 定时重叠造成双倍上游请求。
//
// 刻意不做的事：
//   - Gate 不接管数据面（chat / search）的并发——那条链路由 pool.max_in_flight
//     按账号控制，语义与批量任务不同。
//   - 不提供 ctx 取消：控制面 RPC 已有 upstream.timeout_seconds（默认 120s）兜底，
//     Acquire 的最坏等待是「前面一批请求的全部超时」，有界、不死锁。
package batch

import "sync"

const (
	// Default 默认批量并发：与改造前几个手写 sem 的取值一致（3），零行为回归。
	Default = 3
	// Max 批量并发上限：再往上放就不如让用户显式关掉任务了，也避免一个误配把
	// WAF 风险重新拉满。
	Max = 8
)

// Normalize 把配置值收敛到 [1, Max]：<=0 视为未设置回落 Default，>Max 钳到 Max。
// 与 config.normalize 中 max_in_flight_global 的「非法值回落默认」同风格。
func Normalize(n int) int {
	if n <= 0 {
		return Default
	}
	if n > Max {
		return Max
	}
	return n
}

// Gate 控制面批量任务的共享并发闸门。
//
// 用 sync.Cond 而非 channel 信号量：上限需要支持运行期热改（面板保存配置后立即
// 生效），而带缓冲 channel 无法安全扩容。名额只覆盖「正在打上游」的那段：fn
// 执行期间不持有 Gate.mu，只占 inFlight 计数。
type Gate struct {
	mu       sync.Mutex
	cond     *sync.Cond
	limit    int
	inFlight int
}

// NewGate 构建闸门；limit 走 Normalize（<=0 回落 Default，>Max 钳制）。
func NewGate(limit int) *Gate {
	g := &Gate{limit: Normalize(limit)}
	g.cond = sync.NewCond(&g.mu)
	return g
}

// Limit 返回当前生效上限。nil 接收者 / 零值 Gate 返回 Default（便于测试/零值兜底）。
func (g *Gate) Limit() int {
	if g == nil {
		return Default
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.limit <= 0 {
		return Default
	}
	return g.limit
}

// Set 运行期改上限并唤醒等待者。调大立刻放行排队者；调小时后已占名额的请求照常
// 跑完，新请求等到 inFlight 降到新上限以下才放行。
func (g *Gate) Set(n int) {
	if g == nil {
		return
	}
	n = Normalize(n)
	g.mu.Lock()
	if g.cond == nil {
		g.cond = sync.NewCond(&g.mu)
	}
	g.limit = n
	g.cond.Broadcast()
	g.mu.Unlock()
}

// Acquire 占一个名额，返回幂等的释放函数。
func (g *Gate) Acquire() func() {
	if g == nil {
		return func() {}
	}
	g.mu.Lock()
	if g.cond == nil {
		g.cond = sync.NewCond(&g.mu)
	}
	if g.limit <= 0 {
		// 零值 Gate（未走 NewGate）也要能工作：按默认上限兜底，避免 0 上限永久阻塞。
		g.limit = Default
	}
	for g.inFlight >= g.limit {
		g.cond.Wait()
	}
	g.inFlight++
	g.mu.Unlock()

	// 幂等释放：重复调用只会归还一次名额，避免调用方手滑把计数减穿。
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.inFlight--
			g.cond.Signal()
			g.mu.Unlock()
		})
	}
}

// ForEach 以不超过上限的并发对 0..n-1 各执行一次 fn。
//
// 用固定 worker 池（worker 数 = min(当前上限, n)）而不是「n 个 goroutine 一起等
// 闸门」：账号上百时不至于堆积上百个阻塞 goroutine。worker 每取一项再 Acquire，
// 是为了让多个任务共享同一闸门时按真实上游占用计数（而不是按「各任务各留几个
// 位置」算错）。
//
// fn 内不得再调用 ForEach/Acquire（嵌套占位在 limit=1 时会自锁）。
func (g *Gate) ForEach(n int, fn func(int)) {
	if n <= 0 {
		return
	}
	workers := g.Limit()
	if workers > n {
		workers = n
	}
	if workers < 1 {
		workers = 1
	}

	jobs := make(chan int)
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for i := range jobs {
				release := g.Acquire()
				fn(i)
				release()
			}
		}()
	}
	for i := 0; i < n; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
}

// Coalescer 把并发触发合并成一轮：已有在途轮次时，后来者等它结束就返回，不再
// 重复执行。用于「后台定时器 + 面板手动按钮」共用同一个全量任务（余额刷新）。
//
// 语义是**合并**而不是**跳过**：手动刷新恰逢后台在跑时，等到后台这轮结束即可，
// 用户拿到的仍是刚查回来的数据。
type Coalescer struct {
	mu sync.Mutex
	ch chan struct{}
}

// Do 执行 fn；若已有在途轮次则等待其结束后返回（不执行 fn）。
func (c *Coalescer) Do(fn func()) {
	if c == nil {
		fn()
		return
	}
	c.mu.Lock()
	if ch := c.ch; ch != nil {
		c.mu.Unlock()
		<-ch
		return
	}
	ch := make(chan struct{})
	c.ch = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.ch = nil
		c.mu.Unlock()
		close(ch)
	}()
	fn()
}
