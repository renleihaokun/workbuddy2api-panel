// Package creditline 剩余积分走势采样账本。
//
// 为什么需要单独一条时间线：`credithist` 记的是「每次余额变动」的流水（谁、变了多少），
// 它能回答"这个号什么时候被扣了多少"，但回答不了「整池还剩多少、在涨还是在跌」——
// 那需要把某一时刻所有账号的余额加总。而余额只在查询路径上才是权威值，所以走势只能
// 靠**定时采样**得到：每次余额后台刷新（或签到，签到也会刷余额）结束后落一个点。
//
// 口径（与面板「账号池」的积分合计一致）：
//   - Total = 池内所有账号 credits 之和；
//   - 分域 CN / Global 按账号 realm 拆分（realm 为空按 CN，与全仓既有口径一致）；
//   - **排除企业版不限量账号**（上游 limitNum=-1，credits 不代表可用额度），
//     只把它们的数量记进 Unlimited，面板据此标注「另有 N 个不限量账号未计入」。
//
// 落盘：与 state/usage/credit-history 同目录的 credit-trend.json，原子写（tmp+rename），
// 失败只记日志——采样是旁路观测，不能反过来影响余额刷新主流程。
package creditline

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// fileVersion 落盘格式版本（与 usage.json / credit-history.json 同风格，便于将来迁移）。
const fileVersion = 1

// defaultMaxPoints 采样点上限。余额后台刷新默认 5 分钟一轮 → 288 点/天，
// 12000 点约覆盖 41 天；超出丢最旧（走势图看长期趋势，不需要无限历史）。
const defaultMaxPoints = 12000

// minInterval 两次采样的最小间隔。签到与余额刷新可能在同一分钟内先后跑完，
// 重复落点只会让图更密、不增加信息，故一分钟内只保留先到的那个。
const minInterval = time.Minute

// flushEvery 落盘节流窗口：采样点先进内存，距上次落盘超过该时长才整体重写文件。
//
// 为什么不在每次 Add 时落盘：账本随历史增长（1.2 万点约 1.2 MB），5 分钟一次
// 全量重写对 SSD / SD 卡都是无谓写入（一天 288 次 ≈ 350 MB）；30 分钟一次最多
// 丢最近 6 个点，走势图完全看不出。优雅停机与面板手动刷新会调 Flush 立即落盘。
const flushEvery = 30 * time.Minute

// Point 一个采样点：全池剩余积分合计 + 分域合计。
type Point struct {
	Time   time.Time `json:"time"`
	Total  int64     `json:"total"`
	CN     int64     `json:"cn"`
	Global int64     `json:"global"`
	// Accounts 参与合计的账号数；Unlimited 被排除的企业不限量账号数。
	Accounts  int `json:"accounts"`
	Unlimited int `json:"unlimited,omitempty"`
}

// Snapshot 从池状态算出当前合计（不含企业不限量账号）。Time 由调用方在 Add 时补。
//
// 只读快照：调用方传 pool.List() 的结果即可，不做任何上游调用。
func Snapshot(sts []pool.Status) Point {
	var p Point
	for _, st := range sts {
		// credits_total == -1 是企业版「不限量」哨兵（上游 limitNum=-1）：
		// 它的 credits 不是可用额度，混进合计会把总量算成噪声。
		if st.CreditsTotal == -1 {
			p.Unlimited++
			continue
		}
		p.Total += st.Credits
		if st.Realm == "global" {
			p.Global += st.Credits
		} else {
			p.CN += st.Credits
		}
		p.Accounts++
	}
	return p
}

// diskFile 落盘结构。
type diskFile struct {
	Version int     `json:"version"`
	Saved   string  `json:"saved,omitempty"`
	Points  []Point `json:"points"`
}

// Ledger 并发安全的走势账本。path 为空 = 纯内存（不落盘）。
type Ledger struct {
	mu     sync.Mutex
	path   string
	max    int
	points []Point // 时间升序
	// lastWrite 最近一次整体落盘的时刻（零值 = 本次进程还没写过）；dirty 表示
	// 内存里有未落盘的采样点（见 flushEvery）。
	lastWrite time.Time
	dirty     bool
}

// New 创建账本；path 非空且文件存在时加载既有采样点。加载失败只记日志、从空账本
// 开始——丢历史好过让面板起不来。maxPoints <= 0 回落默认上限。
func New(path string, maxPoints int) *Ledger {
	if maxPoints <= 0 {
		maxPoints = defaultMaxPoints
	}
	l := &Ledger{path: path, max: maxPoints}
	l.load()
	return l
}

// load 读回采样点（无文件 = 空账本，不是错误）。
func (l *Ledger) load() {
	if l.path == "" {
		return
	}
	raw, err := os.ReadFile(l.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[creditline] 读取 %s 失败（从空账本开始）: %v", l.path, err)
		}
		return
	}
	var f diskFile
	if err := json.Unmarshal(raw, &f); err != nil {
		log.Printf("[creditline] 解析 %s 失败（从空账本开始）: %v", l.path, err)
		return
	}
	pts := make([]Point, 0, len(f.Points))
	for _, p := range f.Points {
		if p.Time.IsZero() {
			continue
		}
		pts = append(pts, p)
	}
	if len(pts) > l.max {
		pts = append([]Point(nil), pts[len(pts)-l.max:]...)
	}
	l.points = pts
	log.Printf("[creditline] 已恢复 %d 个积分走势采样点（%s）", len(l.points), l.path)
}

// Add 追加一个采样点（Time 零值 = now）。
//
// 去抖：距上一个点不足 minInterval 时直接丢弃本次——同一分钟内签到与余额刷新
// 先后完成是常态，重复落点不增加信息。
//
// 落盘节流：点先进内存，距上次落盘超过 flushEvery 才整体重写文件（见 flushEvery
// 的取舍说明）；需要立即持久化时调 Flush。
func (l *Ledger) Add(p Point) {
	if l == nil {
		return
	}
	if p.Time.IsZero() {
		p.Time = time.Now()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if n := len(l.points); n > 0 && p.Time.Sub(l.points[n-1].Time) < minInterval {
		return
	}
	l.points = append(l.points, p)
	if len(l.points) > l.max {
		// 丢最旧：copy 到新底层数组，避免 append 复用旧数组导致内存不释放。
		l.points = append([]Point(nil), l.points[len(l.points)-l.max:]...)
	}
	l.dirty = true
	// 节流按墙钟算（不是按采样时间）：表达的是「文件重写频率」，与采样点自身
	// 的时间戳无关（补采/时钟回拨都不该把写盘频率放大）。
	if l.lastWrite.IsZero() || time.Since(l.lastWrite) >= flushEvery {
		l.flushLocked()
	}
}

// Flush 立即把内存里的采样点落盘（优雅停机 / 面板手动刷新时调用；无未落盘点时零操作）。
func (l *Ledger) Flush() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.dirty {
		l.flushLocked()
	}
}

// flushLocked 整体重写账本文件（调用方必须持锁）。
func (l *Ledger) flushLocked() {
	raw, err := json.Marshal(diskFile{
		Version: fileVersion,
		Saved:   time.Now().Format(time.RFC3339),
		Points:  l.points,
	})
	if err != nil {
		log.Printf("[creditline] 序列化失败: %v", err)
		return
	}
	// 落盘仍在锁内：并发采样（余额刷新与签到撞车）时，「快照+顺序」必须一致，
	// 否则慢的那次会把新内容覆盖成旧的。
	l.write(raw)
	l.dirty = false
	l.lastWrite = time.Now()
}

// Read 返回 [from, to] 内的采样点副本（升序；零值侧不设界）。
func (l *Ledger) Read(from, to time.Time) []Point {
	if l == nil {
		return []Point{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Point, 0, len(l.points))
	for _, p := range l.points {
		if !from.IsZero() && p.Time.Before(from) {
			continue
		}
		if !to.IsZero() && p.Time.After(to) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// Last 返回最新采样点（空账本 → false）。
func (l *Ledger) Last() (Point, bool) {
	if l == nil {
		return Point{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.points) == 0 {
		return Point{}, false
	}
	return l.points[len(l.points)-1], true
}

// Len 当前采样点条数。
func (l *Ledger) Len() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.points)
}

// write 原子写盘（临时文件 + rename，与 usage/credithist 同口径）。失败只记日志。
func (l *Ledger) write(raw []byte) {
	if l.path == "" {
		return
	}
	if dir := filepath.Dir(l.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Printf("[creditline] 建目录失败: %v", err)
			return
		}
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("[creditline] 写临时文件失败: %v", err)
		return
	}
	if err := os.Rename(tmp, l.path); err != nil {
		log.Printf("[creditline] 原子替换失败: %v", err)
	}
}
