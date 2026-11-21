// Package reqlog 记录脱敏的请求级指标与可选 JSONL 归档。
//
// 内存指标有界保存最近 100 条并维护进程级计数；磁盘归档只写请求元数据，
// 不写提示词、响应正文、Authorization 或其它凭证。可选的调用来源（客户端 IP /
// User-Agent，见 Event.ClientIP/UserAgent）由 server 按配置开关决定是否填充。
// 归档队列满时丢弃并计数，不允许日志写盘阻塞模型请求。
package reqlog

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	recentLimit     = 100
	defaultFileMax  = int64(16 << 20)
	defaultQueue    = 1024
	defaultReadMax  = 1000
	archiveFileGlob = "requests-*.jsonl"
)

// NewRequestID 生成不含用户信息的本地请求 ID。
func NewRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return fmt.Sprintf("req-%x", b[:])
	}
	return fmt.Sprintf("req-%d", time.Now().UnixNano())
}

const (
	OutcomeSuccess     = "success"
	OutcomeHTTPError   = "http_error"
	OutcomeStreamError = "stream_error"
	OutcomeInterrupted = "interrupted"
)

// Config 归档参数。Enabled=false 时仍保留内存指标。
type Config struct {
	Dir           string
	Enabled       bool
	RetentionDays int
	MaxBytes      int64
	FileMaxBytes  int64
	QueueSize     int
}

// Event 是一条脱敏请求记录。Account 只保存“昵称(uid8)”标签，不保存完整 UID。
//
// ClientIP / UserAgent 是**调用来源**：面板「运行日志」用它回答"这条请求是谁打进来的"。
// 二者由 server 侧按 logging.request_client_info 开关决定是否填充（关掉即保持空串，
// 归档里不会出现来源字段）——来源信息比 token 计数敏感，运营可自行决定是否落盘。
// 仍然不写提示词、响应正文、Authorization 或其它凭证。
type Event struct {
	Time             time.Time `json:"time"`
	RequestID        string    `json:"request_id"`
	Path             string    `json:"path"`
	Account          string    `json:"account,omitempty"`
	Model            string    `json:"model,omitempty"`
	Status           int       `json:"status"`
	OK               bool      `json:"ok"`
	Outcome          string    `json:"outcome"`
	DurationMs       int64     `json:"duration_ms"`
	TTFBMs           int64     `json:"ttfb_ms,omitempty"`
	Attempts         int       `json:"attempts,omitempty"`
	PromptTokens     int64     `json:"prompt_tokens,omitempty"`
	CompletionTokens int64     `json:"completion_tokens,omitempty"`
	TotalTokens      int64     `json:"total_tokens,omitempty"`
	Credit           float64   `json:"credit,omitempty"`
	HasCredit        bool      `json:"credit_known"`
	// CacheHitTokens / CacheMissTokens 上游前缀缓存命中/未命中 token（issue #92）。
	// 上游未回该维度时两者皆零值省略；hit=0 + miss>0 即整段未命中。
	CacheHitTokens  int64 `json:"cache_hit_tokens,omitempty"`
	CacheMissTokens int64 `json:"cache_miss_tokens,omitempty"`
	ClientIP         string    `json:"client_ip,omitempty"`
	UserAgent        string    `json:"user_agent,omitempty"`
	// Realm 实际服务域（cn / global）。一个号都没选到的失败按请求声明的域记。
	// 面板「反代劣化」看板据此按域切分成功率。
	Realm string `json:"realm,omitempty"`
	// ErrKind 归一化失败分类（成功为空）。取值 = 上游错误分类
	// （channel_refused / content_blocked / model_blocked / soft_rate / model_busy /
	// upstream_busy / waf_block / server / …），外加 3 个本地合成值：
	// timeout（上游超时）、stream_read_error（流读取失败）、no_healthy_account（无号）。
	ErrKind string `json:"err_kind,omitempty"`
	// Fallback 本次请求发生了跨域回退（同名模型跨域回退 / 裸名跨域回退）。
	Fallback bool `json:"fallback,omitempty"`
	// Degraded 本次走了 Degraded 中性提示词（内容拦截降级期）。
	Degraded bool `json:"degraded,omitempty"`
	// SysAsUser 本次走过 system→user 角色搬移重试。
	SysAsUser bool `json:"sys_as_user,omitempty"`
}

// Filter 用于从归档中筛选最近记录。字符串字段一律「包含」匹配（大小写不敏感），
// 便于面板用一段 IP 前缀或 UA 片段捞请求；From/To 是闭区间（零值 = 该侧不设界），
// 供「今天 / 近 7 天 / 自定义区间」这类时间查询使用。
type Filter struct {
	Outcome   string
	Account   string
	Model     string
	ClientIP  string
	UserAgent string
	From      time.Time
	To        time.Time
}

// ArchiveStats 归档存储状态。
type ArchiveStats struct {
	Enabled       bool   `json:"enabled"`
	Dir           string `json:"dir,omitempty"`
	Files         int    `json:"files"`
	Bytes         int64  `json:"bytes"`
	DroppedWrites uint64 `json:"dropped_writes"`
	LastError     string `json:"last_error,omitempty"`
}

// Snapshot 一次面板读取的完整指标快照。
type Snapshot struct {
	StartedAt       time.Time    `json:"started_at"`
	Completed       int64        `json:"completed"`
	InFlight        int64        `json:"in_flight"`
	Succeeded       int64        `json:"succeeded"`
	Failed          int64        `json:"failed"`
	SuccessRate     float64      `json:"success_rate"`
	HTTPSuccessRate float64      `json:"http_success_rate"`
	AvgDurationMs   float64      `json:"avg_duration_ms"`
	Recent          []Event      `json:"recent"`
	Archive         ArchiveStats `json:"archive"`
}

// Recorder 并发安全的有界请求指标与归档记录器。
type Recorder struct {
	mu          sync.Mutex
	started     time.Time
	inFlight    int64
	completed   int64
	succeeded   int64
	httpSuccess int64
	durationSum int64
	recent      []Event
	archive     *archiveWriter
}

// New 创建记录器；Dir 为空或 Enabled=false 时只启用内存指标。
func New(cfg Config) *Recorder {
	if cfg.FileMaxBytes <= 0 {
		cfg.FileMaxBytes = defaultFileMax
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultQueue
	}
	r := &Recorder{started: time.Now(), recent: make([]Event, 0, recentLimit)}
	r.archive = newArchiveWriter(cfg)
	return r
}

// Begin 标记一个请求进入处理。
func (r *Recorder) Begin() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.inFlight++
	r.mu.Unlock()
}

// Record 记录一个请求完成事件并写入归档队列。
func (r *Recorder) Record(e Event) {
	if r == nil {
		return
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	if e.Outcome == "" {
		if e.Status == 200 && e.OK {
			e.Outcome = OutcomeSuccess
		} else {
			e.Outcome = OutcomeHTTPError
		}
	}
	r.mu.Lock()
	if r.inFlight > 0 {
		r.inFlight--
	}
	r.completed++
	if e.OK {
		r.succeeded++
	}
	if e.Status >= 200 && e.Status < 300 {
		r.httpSuccess++
	}
	r.durationSum += e.DurationMs
	r.recent = append([]Event{e}, r.recent...)
	if len(r.recent) > recentLimit {
		r.recent = r.recent[:recentLimit]
	}
	r.mu.Unlock()
	if r.archive != nil {
		r.archive.enqueue(e)
	}
}

// Snapshot 返回进程内指标和归档状态。
func (r *Recorder) Snapshot() Snapshot {
	if r == nil {
		return Snapshot{}
	}
	r.mu.Lock()
	s := Snapshot{
		StartedAt: r.started,
		Completed: r.completed,
		InFlight:  r.inFlight,
		Succeeded: r.succeeded,
		Recent:    append([]Event(nil), r.recent...),
	}
	if r.completed > 0 {
		s.Failed = r.completed - r.succeeded
		s.SuccessRate = round1(float64(r.succeeded) / float64(r.completed) * 100)
		s.HTTPSuccessRate = round1(float64(r.httpSuccess) / float64(r.completed) * 100)
		s.AvgDurationMs = float64(r.durationSum) / float64(r.completed)
	}
	r.mu.Unlock()
	if r.archive != nil {
		s.Archive = r.archive.stats()
	}
	return s
}

// ReadArchive 返回最近的归档事件（按时间倒序）。limit<=0 时回落 200，最大 1000。
func (r *Recorder) ReadArchive(limit int, filter Filter) ([]Event, error) {
	if r == nil || r.archive == nil {
		return nil, nil
	}
	return r.archive.read(limit, filter)
}

// Close 刷盘并停止后台归档。
func (r *Recorder) Close() {
	if r == nil || r.archive == nil {
		return
	}
	r.archive.close()
}

func round1(v float64) float64 {
	return float64(int(v*10+0.5)) / 10
}

type archiveWriter struct {
	cfg       Config
	ch        chan Event
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	dropped   atomic.Uint64
	lastErrMu sync.Mutex
	lastErr   string

	file *os.File
	buf  *bufio.Writer
	path string
	day  string
	size int64
}

func newArchiveWriter(cfg Config) *archiveWriter {
	if !cfg.Enabled || strings.TrimSpace(cfg.Dir) == "" {
		return &archiveWriter{cfg: cfg}
	}
	if cfg.RetentionDays <= 0 {
		cfg.RetentionDays = 7
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = 100 << 20
	}
	if cfg.FileMaxBytes <= 0 {
		cfg.FileMaxBytes = defaultFileMax
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultQueue
	}
	w := &archiveWriter{
		cfg:  cfg,
		ch:   make(chan Event, cfg.QueueSize),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		w.setErr(err)
		w.cfg.Enabled = false
		close(w.done)
		return w
	}
	go w.run()
	return w
}

func (w *archiveWriter) enabled() bool {
	return w != nil && w.cfg.Enabled && w.cfg.Dir != "" && w.done != nil
}

func (w *archiveWriter) enqueue(e Event) {
	if !w.enabled() {
		return
	}
	select {
	case w.ch <- e:
	default:
		w.dropped.Add(1)
	}
}

func (w *archiveWriter) run() {
	defer close(w.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case e := <-w.ch:
			w.writeEvent(e)
		case <-ticker.C:
			w.flush()
			w.prune()
		case <-w.stop:
			for {
				select {
				case e := <-w.ch:
					w.writeEvent(e)
				default:
					w.flush()
					w.closeFile()
					return
				}
			}
		}
	}
}

func (w *archiveWriter) writeEvent(e Event) {
	raw, err := json.Marshal(e)
	if err != nil {
		w.setErr(err)
		return
	}
	now := e.Time
	if now.IsZero() {
		now = time.Now()
	}
	day := now.Format("2006-01-02")
	if w.file == nil || w.day != day {
		if err := w.openFile(now, false); err != nil {
			w.setErr(err)
			return
		}
	}
	if w.size > 0 && w.size+int64(len(raw))+1 > w.cfg.FileMaxBytes {
		if err := w.openFile(now, true); err != nil {
			w.setErr(err)
			return
		}
	}
	if _, err := w.buf.Write(raw); err != nil {
		w.setErr(err)
		return
	}
	if err := w.buf.WriteByte('\n'); err != nil {
		w.setErr(err)
		return
	}
	w.size += int64(len(raw)) + 1
}

func (w *archiveWriter) openFile(now time.Time, rotate bool) error {
	w.closeFile()
	day := now.Format("2006-01-02")
	if err := os.MkdirAll(w.cfg.Dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(w.cfg.Dir, "requests-"+day+".jsonl")
	if rotate {
		path = nextArchivePath(w.cfg.Dir, day)
	} else if latest := latestArchiveForDay(w.cfg.Dir, day); latest != "" {
		path = filepath.Join(w.cfg.Dir, latest)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	w.file = f
	w.buf = bufio.NewWriterSize(f, 64<<10)
	w.path = path
	w.day = day
	w.size = info.Size()
	return nil
}

func latestArchiveForDay(dir, day string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	prefix := "requests-" + day
	best := ""
	bestIndex := -1
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		idx := 0
		if name[len(prefix):len(name)-len(".jsonl")] != "" {
			part := strings.TrimSuffix(strings.TrimPrefix(name[len(prefix):], "."), ".jsonl")
			n, err := strconv.Atoi(part)
			if err != nil {
				continue
			}
			idx = n
		}
		if idx > bestIndex {
			bestIndex = idx
			best = name
		}
	}
	if best == "" {
		return ""
	}
	return best
}

func nextArchivePath(dir, day string) string {
	base := filepath.Join(dir, "requests-"+day)
	path := base + ".jsonl"
	for i := 1; fileExists(path); i++ {
		path = fmt.Sprintf("%s.%d.jsonl", base, i)
	}
	return path
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

func (w *archiveWriter) flush() {
	if w.buf == nil {
		return
	}
	if err := w.buf.Flush(); err != nil {
		w.setErr(err)
	}
}

func (w *archiveWriter) closeFile() {
	if w.buf != nil {
		_ = w.buf.Flush()
	}
	if w.file != nil {
		_ = w.file.Close()
	}
	w.file = nil
	w.buf = nil
	w.path = ""
	w.day = ""
	w.size = 0
}

func (w *archiveWriter) prune() {
	if !w.enabled() {
		return
	}
	entries, err := os.ReadDir(w.cfg.Dir)
	if err != nil {
		w.setErr(err)
		return
	}
	type item struct {
		path  string
		size  int64
		mtime time.Time
	}
	items := make([]item, 0, len(entries))
	var total int64
	cutoff := time.Now().AddDate(0, 0, -w.cfg.RetentionDays)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "requests-") || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(w.cfg.Dir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}
		items = append(items, item{path: path, size: info.Size(), mtime: info.ModTime()})
		total += info.Size()
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].mtime.Equal(items[j].mtime) {
			return items[i].path < items[j].path
		}
		return items[i].mtime.Before(items[j].mtime)
	})
	for _, it := range items {
		if it.path != w.path && (it.mtime.Before(cutoff) || total > w.cfg.MaxBytes) {
			if err := os.Remove(it.path); err == nil {
				total -= it.size
			}
		}
	}
}

func (w *archiveWriter) read(limit int, filter Filter) ([]Event, error) {
	if !w.enabled() {
		return nil, nil
	}
	if limit <= 0 {
		limit = 200
	}
	if limit > defaultReadMax {
		limit = defaultReadMax
	}
	entries, err := os.ReadDir(w.cfg.Dir)
	if err != nil {
		return nil, err
	}
	// 文件读取顺序无关紧要：结果一律按事件时间排序。不能依赖归档文件的 mtime 还原时间
	// 顺序——同一秒内连续轮转写出的多个文件 mtime 经常完全相同（Linux 文件时间戳粒度粗），
	// os.ReadDir 的字典序又会把装着最早事件的基准文件 requests-<day>.jsonl 排在
	// requests-<day>.N.jsonl 之后；目录被整体拷贝 / 恢复备份后 mtime 更不可信。
	var out []Event
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "requests-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		rows, err := readFile(filepath.Join(w.cfg.Dir, name), filter)
		if err != nil {
			return out, err
		}
		out = append(out, rows...)
	}
	// 契约：按事件时间倒序返回最近 limit 条（同一时刻用 Stable 保留落盘先后）。
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func readFile(path string, filter Filter) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	var out []Event
	for scanner.Scan() {
		var e Event
		if json.Unmarshal(scanner.Bytes(), &e) != nil || !filter.match(e) {
			continue
		}
		out = append(out, e)
	}
	return out, scanner.Err()
}

func (f Filter) match(e Event) bool {
	if f.Outcome != "" && e.Outcome != f.Outcome {
		return false
	}
	if !containsFold(e.Account, f.Account) {
		return false
	}
	if !containsFold(e.Model, f.Model) {
		return false
	}
	if !containsFold(e.ClientIP, f.ClientIP) {
		return false
	}
	if !containsFold(e.UserAgent, f.UserAgent) {
		return false
	}
	if !f.From.IsZero() && e.Time.Before(f.From) {
		return false
	}
	if !f.To.IsZero() && e.Time.After(f.To) {
		return false
	}
	return true
}

// containsFold 大小写不敏感的子串匹配；needle 为空视为命中（不筛该字段）。
func containsFold(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

func (w *archiveWriter) stats() ArchiveStats {
	s := ArchiveStats{Enabled: w.enabled(), Dir: w.cfg.Dir, DroppedWrites: w.dropped.Load(), LastError: w.errString()}
	if !s.Enabled {
		return s
	}
	entries, err := os.ReadDir(w.cfg.Dir)
	if err != nil {
		s.LastError = err.Error()
		return s
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "requests-") || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		if info, err := entry.Info(); err == nil {
			s.Files++
			s.Bytes += info.Size()
		}
	}
	return s
}

func (w *archiveWriter) close() {
	if !w.enabled() {
		return
	}
	w.closeOnce.Do(func() {
		close(w.stop)
		<-w.done
	})
}

func (w *archiveWriter) setErr(err error) {
	if err == nil {
		return
	}
	w.lastErrMu.Lock()
	w.lastErr = err.Error()
	w.lastErrMu.Unlock()
}

func (w *archiveWriter) errString() string {
	w.lastErrMu.Lock()
	defer w.lastErrMu.Unlock()
	return w.lastErr
}

// ---------------------------------------------------------------------------
// 窗口聚合（面板「反代劣化」看板）
// ---------------------------------------------------------------------------

// AggTotals 一个窗口（或一个域）的汇总。字段口径与面板 KPI 卡一一对应。
type AggTotals struct {
	Requests   int64 `json:"requests"`
	Success    int64 `json:"success"`
	HTTPError  int64 `json:"http_error"`
	StreamError int64 `json:"stream_error"`
	Interrupted int64 `json:"interrupted"`
	// Fallbacks 跨域回退命中次数；Degraded / SysAsUser 走降级提示词 / 角色搬移的次数。
	Fallbacks int64   `json:"fallbacks"`
	Degraded  int64   `json:"degraded"`
	SysAsUser int64   `json:"sys_as_user"`
	SuccessRate   float64 `json:"success_rate"`
	AvgDurationMs float64 `json:"avg_duration_ms"`
	AvgTTFBMs     float64 `json:"avg_ttfb_ms"`

	durationSum int64
	ttfbSum     int64
	ttfbCount   int64
}

// add 累加一条事件（内部口径；finish 之前不对外）。
func (a *AggTotals) add(e Event) {
	a.Requests++
	switch e.Outcome {
	case OutcomeSuccess:
		a.Success++
	case OutcomeStreamError:
		a.StreamError++
	case OutcomeInterrupted:
		a.Interrupted++
	default:
		a.HTTPError++
	}
	if e.Fallback {
		a.Fallbacks++
	}
	if e.Degraded {
		a.Degraded++
	}
	if e.SysAsUser {
		a.SysAsUser++
	}
	if e.DurationMs > 0 {
		a.durationSum += e.DurationMs
	}
	// TTFB 只在流式成功时有观测；非流式恒 0，不能把它算进均值（会把平均拉成 0）。
	if e.TTFBMs > 0 {
		a.ttfbSum += e.TTFBMs
		a.ttfbCount++
	}
}

// finish 收敛派生指标。
func (a *AggTotals) finish() {
	if a.Requests > 0 {
		a.SuccessRate = round1(float64(a.Success) / float64(a.Requests) * 100)
		a.AvgDurationMs = float64(a.durationSum) / float64(a.Requests)
	}
	if a.ttfbCount > 0 {
		a.AvgTTFBMs = float64(a.ttfbSum) / float64(a.ttfbCount)
	}
}

// AggBucket 一个时间桶。t 的格式与 usage 的时序点一致（hour: 2006-01-02T15，
// day: 2006-01-02），前端可直接复用 parsePointTime / fmtTokTimeLabel。
type AggBucket struct {
	T           string           `json:"t"`
	Scope       string           `json:"scope"`
	Requests    int64            `json:"requests"`
	Success     int64            `json:"success"`
	HTTPError   int64            `json:"http_error"`
	StreamError int64            `json:"stream_error"`
	Interrupted int64            `json:"interrupted"`
	Fallbacks   int64            `json:"fallbacks"`
	Kinds       map[string]int64 `json:"kinds,omitempty"`
}

// KindCount 某失败分类在窗口内的次数与最近发生时间。
type KindCount struct {
	Kind   string    `json:"kind"`
	Count  int64     `json:"count"`
	LastAt time.Time `json:"last_at,omitempty"`
}

// Aggregate 窗口聚合结果。
type Aggregate struct {
	Bucket  string               `json:"bucket"` // "hour" / "day"
	From    time.Time            `json:"from"`
	To      time.Time            `json:"to"`
	Totals  AggTotals            `json:"totals"`
	Buckets []AggBucket          `json:"buckets"`
	Kinds   []KindCount          `json:"kinds"`
	Realms  map[string]AggTotals `json:"realms"`
	Archive ArchiveStats         `json:"archive"`
}

// aggregateKindUnknown 旧归档没有 err_kind 字段（或失败分类缺失）时的兜底桶。
const aggregateKindUnknown = "unknown"

// Aggregate 扫描归档并按时间桶聚合（只读、内存有界：只保留桶与分类计数，
// 不把事件整批读进内存）。
//
// 参数：from/to 为闭区间（零值 = 该侧不设界）；bucket<=0 按小时。
// 归档未启用/无目录 → 返回空结果（Archive.Enabled=false），不报错——面板据此
// 提示「归档已关闭」而不是显示一片 0。
//
// 与 ReadArchive 的取舍：后者为「最近 N 条」而生（全量读+排序+截断），窗口聚合
// 要的是全窗口计数，因此这里流式扫描、边读边累加，不保留事件本体。
func (r *Recorder) Aggregate(from, to time.Time, bucket time.Duration) (Aggregate, error) {
	// 空切片而不是 nil：面板把 null 与「接口不可用」混在一起会显示成加载失败
	// （与 ReadArchive 的空结果回 [] 同口径）。
	agg := Aggregate{
		From: from, To: to,
		Buckets: []AggBucket{},
		Kinds:   []KindCount{},
		Realms:  map[string]AggTotals{},
	}
	scope, layout := aggregateScope(bucket)
	agg.Bucket = scope
	if r == nil || r.archive == nil {
		return agg, nil
	}
	agg.Archive = r.archive.stats()
	if !r.archive.enabled() {
		return agg, nil
	}
	paths, err := archiveFiles(r.archive.cfg.Dir)
	if err != nil {
		return agg, err
	}
	buckets := map[string]*AggBucket{}
	kinds := map[string]*KindCount{}
	var total AggTotals
	// firstSeen/lastSeen 用于「不设界」时回显真实数据区间（面板右上角的口径说明）。
	var firstSeen, lastSeen time.Time
	for _, path := range paths {
		if err := scanFile(path, from, to, func(e Event) {
			if firstSeen.IsZero() || e.Time.Before(firstSeen) {
				firstSeen = e.Time
			}
			if e.Time.After(lastSeen) {
				lastSeen = e.Time
			}
			total.add(e)
			realm := e.Realm
			if realm == "" {
				realm = aggregateKindUnknown
			}
			ra := agg.Realms[realm]
			ra.add(e)
			agg.Realms[realm] = ra

			key := e.Time.In(time.Local).Format(layout)
			b := buckets[key]
			if b == nil {
				b = &AggBucket{T: key, Scope: scope, Kinds: map[string]int64{}}
				buckets[key] = b
			}
			b.Requests++
			switch e.Outcome {
			case OutcomeSuccess:
				b.Success++
			case OutcomeStreamError:
				b.StreamError++
			case OutcomeInterrupted:
				b.Interrupted++
			default:
				b.HTTPError++
			}
			if e.Fallback {
				b.Fallbacks++
			}
			kind := e.ErrKind
			if kind == "" {
				// 成功与「客户端断连」都不属于失败分类：前者无失败，后者是客户端
				// 主动离开（上游帧无恙），把它算进错误表会让看板虚高。
				if e.Outcome == OutcomeSuccess || e.Outcome == OutcomeInterrupted {
					return
				}
				kind = aggregateKindUnknown
			}
			b.Kinds[kind]++
			kc := kinds[kind]
			if kc == nil {
				kc = &KindCount{Kind: kind}
				kinds[kind] = kc
			}
			kc.Count++
			if e.Time.After(kc.LastAt) {
				kc.LastAt = e.Time
			}
		}); err != nil {
			return agg, err
		}
	}
	total.finish()
	agg.Totals = total
	for realm, ra := range agg.Realms {
		ra.finish()
		agg.Realms[realm] = ra
	}
	// 桶按时间升序（格式本身即字典序），空桶不补——前端按真实时间轴定位。
	keys := make([]string, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	agg.Buckets = make([]AggBucket, 0, len(keys))
	for _, k := range keys {
		b := buckets[k]
		if len(b.Kinds) == 0 {
			b.Kinds = nil
		}
		agg.Buckets = append(agg.Buckets, *b)
	}
	agg.Kinds = make([]KindCount, 0, len(kinds))
	for _, kc := range kinds {
		agg.Kinds = append(agg.Kinds, *kc)
	}
	sort.SliceStable(agg.Kinds, func(i, j int) bool {
		if agg.Kinds[i].Count == agg.Kinds[j].Count {
			return agg.Kinds[i].Kind < agg.Kinds[j].Kind
		}
		return agg.Kinds[i].Count > agg.Kinds[j].Count
	})
	// 不设界的一侧回显真实数据区间：面板右上角要显示「数据自 … → …」，
	// 零值时间会让前端渲染出 0001-01-01。
	if agg.From.IsZero() {
		agg.From = firstSeen
	}
	if agg.To.IsZero() {
		agg.To = lastSeen
	}
	return agg, nil
}

// aggregateScope 把桶宽映射成 (scope, 本地时间格式)。
func aggregateScope(bucket time.Duration) (scope, layout string) {
	if bucket >= 24*time.Hour {
		return "day", "2006-01-02"
	}
	return "hour", "2006-01-02T15"
}

// archiveFiles 列出归档目录下的 JSONL 文件名（完整路径，未排序）。
func archiveFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "requests-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	return out, nil
}

// scanFile 流式扫描单个归档文件：只回调落在 [from,to] 内的事件（零值侧不设界）。
// 解析失败的行跳过（半截写入/手工编辑），不中断整窗统计。
func scanFile(path string, from, to time.Time, fn func(Event)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		var e Event
		if json.Unmarshal(scanner.Bytes(), &e) != nil || e.Time.IsZero() {
			continue
		}
		if !from.IsZero() && e.Time.Before(from) {
			continue
		}
		if !to.IsZero() && e.Time.After(to) {
			continue
		}
		fn(e)
	}
	return scanner.Err()
}

