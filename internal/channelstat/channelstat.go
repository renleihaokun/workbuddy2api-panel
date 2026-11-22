// Package channelstat 反代通道事件计数器：把「上游怎么对待我们」按小时累计下来。
//
// 为什么需要它：请求归档（internal/reqlog）只记**终态**——11-128 被 system→user
// 搬移救回后终态是 200/success，归档里看不出上游拦过我们；而这类「拦截 → 救回」
// 恰恰是反代通道劣化的核心信号。本包在网关的决策点按事件自增，与终态无关。
//
// 与其它账本的分工：
//   - internal/usage  记「谁用了多少 token / 积分」（按账号/模型分桶）
//   - internal/reqlog 记「每个请求的终态」（7 天 JSONL 归档）
//   - 本包            记「上游拦了几次、我们用了什么手段救」（全局计数）
//
// 存储：data/channel-stats.json，按小时桶（90 天后折叠为日桶），5 分钟防抖落盘。
// 量级：18 类事件 × 90 天小时桶 ≈ 0.5–0.8 MB；脏了就全量重写，一天最多约
// 288 次 ≈ 150–250 MB 写入。SSD 部署可接受，SD 卡/机械盘部署需留意写放大
// （事件稀疏时 dirty 不常置位，实际远低于上限）。
package channelstat

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// hourlyKeep 小时桶保留时长；更早的折叠为日桶（与 usage 同口径）。
	hourlyKeep = 90 * 24 * time.Hour
	// flushInterval 落盘节流窗口。事件是全局计数，5 分钟一次足够；比 usage 的
	// 30s 更稀疏，因为这里每个请求可能触发多次自增、文件也随历史增长。
	flushInterval = 5 * time.Minute
	// fileVersion 账本格式版本。
	fileVersion = 1

	hourLayout = "2006-01-02T15"
	dayLayout  = "2006-01-02"
)

// bucket 一个时间片的各事件计数。JSON 字段名取短，桶会随时间增长。
type bucket struct {
	Scope string           `json:"s"`
	Kinds map[string]int64 `json:"k"`
}

// diskFile 落盘结构。
type diskFile struct {
	Version int      `json:"version"`
	Saved   string   `json:"saved"`
	Since   string   `json:"since,omitempty"` // 最早一条数据的时刻（前端标注「数据起点」）
	Buckets []bucket `json:"buckets"`
	// LastAt 每类事件最近一次的精确时刻（比桶起点更准，用于「最近一次」列）。
	LastAt map[string]string `json:"last_at,omitempty"`
	// DegradeLastAt 提示词降级最近一次触发的时刻（跨重启保留）。
	DegradeLastAt string `json:"degrade_last_at,omitempty"`
	// LearnedTotal 累计学习「必须伪装成 user」的 system 指纹次数（跨重启保留）。
	LearnedTotal int64 `json:"learned_total,omitempty"`
}

// Point 序列里的一个点（给面板画图）。
//
// Kinds 是原始事件计数（事件表/调试用）；Groups 是折算后的四个进图组
// （refusal/throttle/stream/other），面板堆叠柱直接用它，不再在 JS 里做减法。
type Point struct {
	T      string           `json:"t"`
	Scope  string           `json:"scope"`
	Kinds  map[string]int64 `json:"kinds"`
	Groups map[string]int64 `json:"groups"`
	Total  int64            `json:"total"`
}

// Totals 窗口内的汇总。
type Totals struct {
	Kinds  map[string]int64  `json:"kinds"`
	Groups map[string]int64  `json:"groups"`
	Total  int64             `json:"total"`
	LastAt map[string]string `json:"last_at,omitempty"`
}

// Ledger 并发安全的事件计数器。path 为空 = 纯内存（测试用，不落盘）。
type Ledger struct {
	mu            sync.Mutex
	path          string
	buckets       map[string]map[string]int64 // scope -> kind -> n
	since         time.Time                   // 最早一条数据
	last          map[string]time.Time        // kind -> 最近一次
	degradeLastAt time.Time
	learnedTotal  int64
	dirty         bool
	stop          chan struct{}
	done          chan struct{}
	stopOnce      sync.Once
	startOnce     sync.Once
	started       atomic.Bool // Start 是否真的起过后台循环（Stop 判它避免永久阻塞）
}

// New 创建账本并尝试从 path 恢复；path 为空时禁用落盘。
func New(path string) *Ledger {
	l := &Ledger{
		path:    path,
		buckets: make(map[string]map[string]int64),
		last:    make(map[string]time.Time),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	if path != "" {
		if err := l.load(); err != nil {
			log.Printf("[channelstat] 读取 %s 失败（从零开始）: %v", path, err)
		}
	}
	return l
}

// Start 启动后台落盘循环（5 分钟一次，含 90 天折叠）。Stop 前一直运行。
//
// 幂等：重复调用只起一个 goroutine（否则两个 goroutine 会各自 close(done)，
// Stop 时 panic: close of closed channel）。
func (l *Ledger) Start() {
	if l == nil {
		return
	}
	l.startOnce.Do(func() {
		l.started.Store(true)
		go func() {
			defer close(l.done)
			t := time.NewTicker(flushInterval)
			defer t.Stop()
			for {
				select {
				case <-l.stop:
					l.mu.Lock()
					l.rollupLocked(time.Now())
					l.flushLocked()
					l.mu.Unlock()
					return
				case <-t.C:
					l.mu.Lock()
					l.rollupLocked(time.Now())
					l.flushLocked()
					l.mu.Unlock()
				}
			}
		}()
	})
}

// Stop 停止后台循环并做最后一次落盘（含折叠）。
//
// 未 Start 过的账本直接返回：此时 done 永远不会被 close，等下去就是永久阻塞
// （测试/纯内存账本经常只 New 不 Start）。Stop 可重复调用。
func (l *Ledger) Stop() {
	if l == nil || !l.started.Load() {
		return
	}
	l.stopOnce.Do(func() { close(l.stop) })
	<-l.done
}

// Inc 记一次事件（now）。
func (l *Ledger) Inc(kind Kind) { l.IncN(kind, 1) }

// IncN 记 n 次事件（now）。
func (l *Ledger) IncN(kind Kind, n int) {
	if l == nil || kind == "" || n <= 0 {
		return
	}
	l.mu.Lock()
	l.addLocked(time.Now(), kind, n)
	l.mu.Unlock()
}

// AddAt 把 n 次事件记到指定时刻（回填 / 测试用；不影响「最近一次」以外的语义）。
func (l *Ledger) AddAt(t time.Time, kind Kind, n int) {
	if l == nil || kind == "" || n <= 0 {
		return
	}
	l.mu.Lock()
	l.addLocked(t, kind, n)
	l.mu.Unlock()
}

func (l *Ledger) addLocked(t time.Time, kind Kind, n int) {
	key := string(kind)
	scope := "h:" + t.Format(hourLayout)
	b := l.buckets[scope]
	if b == nil {
		b = make(map[string]int64, 4)
		l.buckets[scope] = b
	}
	b[key] += int64(n)
	if l.since.IsZero() || t.Before(l.since) {
		l.since = t
	}
	if prev, ok := l.last[key]; !ok || t.After(prev) {
		l.last[key] = t
	}
	l.dirty = true
}

// MarkDegrade 记录一次提示词降级触发（跨重启保留「上次触发」）。
func (l *Ledger) MarkDegrade(at time.Time) {
	if l == nil {
		return
	}
	if at.IsZero() {
		at = time.Now()
	}
	l.mu.Lock()
	if at.After(l.degradeLastAt) {
		l.degradeLastAt = at
	}
	l.dirty = true
	l.mu.Unlock()
}

// AddLearned 累加「已学习指纹」次数（跨重启保留累计值）。
func (l *Ledger) AddLearned(n int) {
	if l == nil || n <= 0 {
		return
	}
	l.mu.Lock()
	l.learnedTotal += int64(n)
	l.dirty = true
	l.mu.Unlock()
}

// Snapshot 返回跨重启的元信息：数据起点、降级上次触发、累计学习次数。
func (l *Ledger) Snapshot() (since, degradeLastAt time.Time, learnedTotal int64) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.since, l.degradeLastAt, l.learnedTotal
}

// Len 当前桶数量（测试用）。
func (l *Ledger) Len() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// Series 返回 [from, to] 的连续时间序列与窗口汇总。
//
// daily=false 用小时桶，true 用日桶；缺失的时间片补零（前端按点画柱，缺片会让
// 柱子挤在一起）。from 零值 = 数据起点；to 零值 = 现在。窗口跨度超过 90 天且
// 仍要求小时桶时自动降级为日桶（90 天 × 24 点已超出可读范围）。
func (l *Ledger) Series(from, to time.Time, daily bool) ([]Point, Totals) {
	totals := Totals{Kinds: map[string]int64{}, Groups: map[string]int64{}, LastAt: map[string]string{}}
	if l == nil {
		return []Point{}, totals
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	if to.IsZero() || to.After(now) {
		to = now
	}
	if from.IsZero() {
		from = l.since
		if from.IsZero() {
			from = to.Add(-24 * time.Hour)
		}
	}
	// 未来/倒置起点收敛到窗口末尾：此前是交换 from/to，传一个未来 from 会
	// 交换出一个未来窗口，返回几十个未来空点（面板自定义区间可触发）。
	if from.After(to) {
		from = to
	}
	if !daily && to.Sub(from) > 90*24*time.Hour {
		daily = true
	}
	step := time.Hour
	layout := hourLayout
	if daily {
		step = 24 * time.Hour
		layout = dayLayout
	}
	// 对齐到桶边界：起点取整点/零点，终点向下取整，保证首尾桶完整。
	from = truncate(from, daily)
	to = truncate(to, daily)

	agg := make(map[string]map[string]int64, 64)
	for scope, kinds := range l.buckets {
		t, ok := parseScope(scope)
		if !ok {
			continue
		}
		// 按目标粒度对齐后再比较：日桶下 21:00 的小时桶属于「今天」，
		// 直接拿 21:00 和「今天 00:00」比会把当天数据全部丢掉。
		bStart := truncate(t, daily)
		if bStart.Before(from) || bStart.After(to) {
			continue
		}
		key := "h:" + t.Format(hourLayout)
		if daily {
			key = "d:" + t.Format(dayLayout)
		}
		dst := agg[key]
		if dst == nil {
			dst = make(map[string]int64, len(kinds))
			agg[key] = dst
		}
		for k, n := range kinds {
			dst[k] += n
			totals.Kinds[k] += n
			totals.Total += n
		}
	}
	totals.Groups = groupsOf(totals.Kinds, totals.Total)

	// 「最近一次」：l.last 可能是本地 time.Now()（进程内事件），也可能是
	// time.Parse 出来的 UTC（从磁盘恢复）。必须统一到本地时区再按桶对齐，
	// 否则 UTC 的 00:00 截断结果与本地 from/to 比较会整体错位——生产上
	// 表现为事件明明有计数、「最近一次」却全是「—」。
	for k, at := range l.last {
		local := at.In(time.Local)
		if truncate(local, daily).Before(from) || truncate(local, daily).After(to) {
			continue
		}
		totals.LastAt[k] = local.Format(time.RFC3339)
	}

	points := make([]Point, 0, int(to.Sub(from)/step)+1)
	scopeLabel, layout := "hour", "2006-01-02T15"
	if daily {
		scopeLabel, layout = "day", dayLayout
	}
	for t := from; !t.After(to); t = t.Add(step) {
		key := "h:" + t.Format(hourLayout)
		if daily {
			key = "d:" + t.Format(dayLayout)
		}
		kinds := agg[key]
		if kinds == nil {
			kinds = map[string]int64{}
		}
		var sum int64
		for _, n := range kinds {
			sum += n
		}
		points = append(points, Point{
			T:      t.Format(layout),
			Scope:  scopeLabel,
			Kinds:  kinds,
			Groups: groupsOf(kinds, sum),
			Total:  sum,
		})
	}
	return points, totals
}

// truncate 把时刻对齐到桶边界（小时桶对齐到整点，日桶对齐到当天零点）。
func truncate(t time.Time, daily bool) time.Time {
	y, m, d := t.Date()
	if daily {
		return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
	}
	return time.Date(y, m, d, t.Hour(), 0, 0, 0, t.Location())
}

// parseScope 解析 "h:2006-01-02T15" / "d:2006-01-02" 里的时刻。
func parseScope(scope string) (time.Time, bool) {
	switch {
	case strings.HasPrefix(scope, "h:"):
		t, err := time.ParseInLocation(hourLayout, scope[2:], time.Local)
		return t, err == nil
	case strings.HasPrefix(scope, "d:"):
		t, err := time.ParseInLocation(dayLayout, scope[2:], time.Local)
		return t, err == nil
	}
	return time.Time{}, false
}

// rollupLocked 把超过 hourlyKeep 的小时桶折叠为日桶（调用方持锁）。
func (l *Ledger) rollupLocked(now time.Time) {
	cutoff := now.Add(-hourlyKeep)
	moved := false
	for scope, kinds := range l.buckets {
		t, ok := parseScope(scope)
		if !ok || !strings.HasPrefix(scope, "h:") || !t.Before(cutoff) {
			continue
		}
		key := "d:" + t.Format(dayLayout)
		dst := l.buckets[key]
		if dst == nil {
			dst = make(map[string]int64, len(kinds))
			l.buckets[key] = dst
		}
		for k, n := range kinds {
			dst[k] += n
		}
		delete(l.buckets, scope)
		moved = true
	}
	if moved {
		l.dirty = true
	}
}

// flushLocked 序列化并落盘（调用方持锁）。
func (l *Ledger) flushLocked() {
	if l.path == "" || !l.dirty {
		return
	}
	scopes := make([]string, 0, len(l.buckets))
	for scope := range l.buckets {
		scopes = append(scopes, scope)
	}
	sort.Strings(scopes)
	buckets := make([]bucket, 0, len(scopes))
	for _, scope := range scopes {
		buckets = append(buckets, bucket{Scope: scope, Kinds: l.buckets[scope]})
	}
	last := make(map[string]string, len(l.last))
	for k, t := range l.last {
		last[k] = t.Format(time.RFC3339)
	}
	f := diskFile{
		Version:      fileVersion,
		Saved:        time.Now().Format(time.RFC3339),
		Buckets:      buckets,
		LastAt:       last,
		LearnedTotal: l.learnedTotal,
	}
	if !l.since.IsZero() {
		f.Since = l.since.Format(time.RFC3339)
	}
	if !l.degradeLastAt.IsZero() {
		f.DegradeLastAt = l.degradeLastAt.Format(time.RFC3339)
	}
	raw, err := json.Marshal(f)
	if err != nil {
		log.Printf("[channelstat] 序列化失败: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		log.Printf("[channelstat] 建目录失败: %v", err)
		return
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("[channelstat] 写临时文件失败: %v", err)
		return
	}
	if err := os.Rename(tmp, l.path); err != nil {
		log.Printf("[channelstat] 原子替换失败: %v", err)
		return
	}
	l.dirty = false
}

// Flush 立即落盘（停机 / 面板手动刷新用）。
func (l *Ledger) Flush() {
	if l == nil || l.path == "" {
		return
	}
	l.mu.Lock()
	l.flushLocked()
	l.mu.Unlock()
}

// load 读取账本文件；随后尝试合并一次性回填种子（见 seedPath）。
//
// 主文件读取失败（非 NotExist）时**不再**合并种子：种子导入末尾会 flush 覆盖
// 主账本，一次瞬时读失败就能把主账本替换成只剩种子数据。
func (l *Ledger) load() error {
	if err := l.loadFile(l.path, true); err != nil {
		return err
	}
	l.loadSeed()
	return nil
}

// loadFile 把文件里的桶累加进账本，并按 replaceMeta 决定元信息语义：
//   - replaceMeta=true（主账本）：since/last/degrade_last_at/learned_total 以文件为准；
//   - replaceMeta=false（回填种子）：since 取最早、last 每 kind 取最晚、
//     degrade_last_at 取最晚、learned_total 累加。
//
// 桶一律累加。主文件是权威快照，种子是一次性历史补充，两者不能共用「覆盖」
// 语义——此前种子缺 learned_total 就会把主账本累计清零，更旧的种子还能把
// 「上次触发」回滚。
func (l *Ledger) loadFile(path string, replaceMeta bool) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var f diskFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return err
	}
	for _, b := range f.Buckets {
		if b.Scope == "" || len(b.Kinds) == 0 {
			continue
		}
		dst := l.buckets[b.Scope]
		if dst == nil {
			dst = make(map[string]int64, len(b.Kinds))
			l.buckets[b.Scope] = dst
		}
		for k, n := range b.Kinds {
			dst[k] += n
		}
	}
	if replaceMeta {
		if t, err := time.Parse(time.RFC3339, f.Since); err == nil {
			l.since = t
		}
		for k, v := range f.LastAt {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				l.last[k] = t
			}
		}
		if t, err := time.Parse(time.RFC3339, f.DegradeLastAt); err == nil {
			l.degradeLastAt = t
		}
		l.learnedTotal = f.LearnedTotal
	} else {
		if t, err := time.Parse(time.RFC3339, f.Since); err == nil {
			if l.since.IsZero() || t.Before(l.since) {
				l.since = t
			}
		}
		for k, v := range f.LastAt {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				continue
			}
			if prev, ok := l.last[k]; !ok || t.After(prev) {
				l.last[k] = t
			}
		}
		if t, err := time.Parse(time.RFC3339, f.DegradeLastAt); err == nil {
			if t.After(l.degradeLastAt) {
				l.degradeLastAt = t
			}
		}
		l.learnedTotal += f.LearnedTotal
	}
	// 恢复后不置 dirty：没有新事件就不该重写文件。
	return nil
}

// seedPath 一次性回填种子：<name>.json → <name>.seed.json。
func seedPath(path string) string {
	return strings.TrimSuffix(path, ".json") + ".seed.json"
}

// loadSeed 合并回填种子（存在才做），合并后改名 .imported，避免重复累加。
//
// 先落盘再改名：改名之后就再也读不到种子了，必须保证合并结果已经进了主账本，
// 否则「改名成功 + 落盘失败」= 回填数据永久丢失。
func (l *Ledger) loadSeed() {
	if l.path == "" {
		return
	}
	seed := seedPath(l.path)
	if _, err := os.Stat(seed); err != nil {
		return
	}
	if err := l.loadFile(seed, false); err != nil {
		log.Printf("[channelstat] 回填种子 %s 解析失败: %v", seed, err)
		return
	}
	l.mu.Lock()
	l.dirty = true
	l.flushLocked()
	flushFailed := l.dirty // flushLocked 成功才会清 dirty
	l.mu.Unlock()
	if flushFailed {
		log.Printf("[channelstat] 回填种子已读入但落盘失败，保留 %s 下次再试", seed)
		return
	}
	if err := os.Rename(seed, seed+".imported"); err != nil {
		log.Printf("[channelstat] 回填种子改名失败（下次启动会重复合并）: %v", err)
		return
	}
	log.Printf("[channelstat] 已合并回填种子 %s", seed)
}
