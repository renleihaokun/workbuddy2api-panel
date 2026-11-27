// taskcenter.go 面板「任务中心」：全账号任务扫描 + 执行队列（可配并发）+
// 成长任务队列。
//
// 语义：
//   - 扫描（scan_all）：并发拉取每账号的成长任务列表（默认+小程序口径），
//     汇总出"未完成且可自动化"的待办清单（只读，不执行）。
//   - 执行队列（run_queue + queue）：把待办项按账号分组排队执行——账号内
//     串行（复用 per-account 锁，与单任务/一键完成互斥），账号间并发
//     （concurrency 信号量限制，默认 1）。队列状态可轮询。
package panel

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// ---------------------------------------------------------------------------
// 扫描（只读）
// ---------------------------------------------------------------------------

// scanAccountItem 单账号扫描结果。
type scanAccountItem struct {
	UID       string          `json:"uid"`
	Nickname  string          `json:"nickname"`
	Growth    []upstream.Task `json:"growth,omitempty"`
	GrowthErr string          `json:"growth_error,omitempty"`
}

// growthPending 任务是否"未完成且可自动化"。
func growthPending(t upstream.Task) bool {
	if t.Claimed {
		return false
	}
	// 上游锁定的任务不出待办：Sequential 族每日零点解锁一环，刚做完上一环时
	// 下一环以下发但 locked 形态出现在列表里——扫进队列只会 accept 不落账报
	// 失败（每日锁定窗口），零点解锁后自然回到待办。其余 locked（上游未开放）
	// 同语义：不该被自动化尝试。
	if t.Locked {
		return false
	}
	if t.Target > 0 && t.Current >= t.Target {
		// 达标未领：也入队（队列执行后会自动领）——但仅限有自动化动作的任务，
		// 否则队列执行时会因 autoActionFor 为 nil 直接报错。
		return autoActionFor(t.TaskCode) != nil
	}
	return autoActionFor(t.TaskCode) != nil
}

func (p *Panel) tasksScanMeta() map[string]any {
	p.scanMu.Lock()
	defer p.scanMu.Unlock()
	return map[string]any{
		"fetched_at":    p.scanAt,
		"scanning":      p.scanScanning,
		"done":          p.scanDone,
		"total":         p.scanTotal,
		"pending_count": p.scanPending,
	}
}

// tasksScanCached 读取最近一次待办扫描的内存缓存与进度（只读，零上游请求）。
func (p *Panel) tasksScanCached(w http.ResponseWriter, r *http.Request) {
	p.scanMu.Lock()
	items := make([]scanAccountItem, len(p.scanCache))
	copy(items, p.scanCache)
	pending := p.scanPending
	at := p.scanAt
	scanning := p.scanScanning
	done := p.scanDone
	total := p.scanTotal
	p.scanMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"accounts":      items,
		"pending_count": pending,
		"cached":        true,
		"fetched_at":    at,
		"scanning":      scanning,
		"done":          done,
		"total":         total,
	})
}

// tasksScanAll 异步单飞触发全账号成长待办扫描（避免长时间占用 HTTP 或连点重放）。
func (p *Panel) tasksScanAll(w http.ResponseWriter, r *http.Request) {
	started := p.startTasksScanAsync()
	p.scanMu.Lock()
	items := make([]scanAccountItem, len(p.scanCache))
	copy(items, p.scanCache)
	pending := p.scanPending
	at := p.scanAt
	scanning := p.scanScanning
	done := p.scanDone
	total := p.scanTotal
	p.scanMu.Unlock()
	msg := "全池待办扫描已开始（串行防探测节奏）"
	if !started {
		msg = "待办扫描正在进行中"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"started":       started,
		"scanning":      scanning,
		"done":          done,
		"total":         total,
		"fetched_at":    at,
		"accounts":      items,
		"pending_count": pending,
		"message":       msg,
	})
}

func (p *Panel) startTasksScanAsync() bool {
	p.scanMu.Lock()
	if p.scanScanning {
		p.scanMu.Unlock()
		return false
	}
	p.scanScanning = true
	p.scanDone = 0
	p.scanTotal = 0
	p.scanMu.Unlock()

	go func() {
		p.scanRun.Do(func() {
			p.runTasksScanSync()
		})
	}()
	return true
}

// runTasksScanSync 真正按 pool 闸门（默认串行+随机间隔）扫全池待办，单号双口径间插入微短停顿，并更新缓存。
func (p *Panel) runTasksScanSync() ([]scanAccountItem, int) {
	states := p.cfg.Pool.List()
	items := make([]scanAccountItem, len(states))
	type scanJob struct {
		idx int
		uid string
	}
	var jobs []scanJob
	for i, st := range states {
		if st.Disabled {
			continue
		}
		jobs = append(jobs, scanJob{idx: i, uid: st.UID})
	}
	p.scanMu.Lock()
	p.scanScanning = true
	p.scanDone = 0
	p.scanTotal = len(jobs)
	p.scanMu.Unlock()

	p.cfg.Pool.BatchForEachProgress(len(jobs), func(k int) {
		j := jobs[k]
		a := p.cfg.Pool.AuthByUID(j.uid)
		if a == nil {
			return
		}
		it := &items[j.idx]
		it.UID, it.Nickname = j.uid, a.Nickname
		// D4 门控：global 账号无 CN 成长任务体系，不发起任何上游调用。
		if a.IsGlobal() {
			return
		}
		// 企业版门控：同上——企业版无个人成长体系（GET /v2/activity/growth/tasks
		// 上游 403「growth system is only available for personal users」）。
		if a.IsEnterprise() {
			return
		}
		if tasks, err := p.cfg.Upstream.ListTasks(a); err != nil {
			it.GrowthErr = err.Error()
		} else {
			for _, t := range tasks {
				if growthPending(t) {
					it.Growth = append(it.Growth, t)
				}
			}
		}
		if gap := p.batchSubDelay(); gap > 0 {
			time.Sleep(gap)
		}
		// 小程序口径任务（school_season 校园日 / Sequential_Tasks_1 小程序首对话）
		// 仅在 mp 头列表下发，与默认口径不重叠——合并进待办列表；mp 列表失败
		// 静默（无 mp 任务的部署/活动结束时零影响）。
		if mpTasks, err := p.cfg.Upstream.ListTasksMP(a); err == nil {
			seen := map[string]bool{}
			for _, t := range it.Growth {
				seen[t.TaskCode] = true
			}
			for _, t := range mpTasks {
				if growthPending(t) && !seen[t.TaskCode] {
					it.Growth = append(it.Growth, t)
				}
			}
		}
	}, func(done, total int) {
		p.scanMu.Lock()
		p.scanDone = done
		p.scanTotal = total
		p.scanMu.Unlock()
	})
	pending := 0
	for _, it := range items {
		pending += len(it.Growth)
	}
	cached := make([]scanAccountItem, len(items))
	copy(cached, items)
	p.scanMu.Lock()
	p.scanCache = cached
	p.scanPending = pending
	p.scanAt = time.Now()
	p.scanScanning = false
	p.scanDone = len(jobs)
	p.scanTotal = len(jobs)
	p.scanMu.Unlock()
	log.Printf("panel: 队列扫描完成：全部账号待办 %d 项", pending)
	return items, pending
}

// ---------------------------------------------------------------------------
// 执行队列
// ---------------------------------------------------------------------------

// queueItem 队列执行单元。
type queueItem struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	Kind     string `json:"kind"` // growth
	Code     string `json:"code"`
	Status   string `json:"status"` // pending | running | done | skipped | error
	Message  string `json:"message,omitempty"`
}

// queueState 队列运行状态。Seq 每次启动 +1——前端只渲染"自己启动的那一轮"，
// 执行结束后的残留 items 不会覆盖后续的扫描结果视图。
type queueState struct {
	mu        sync.Mutex
	running   bool
	startedAt time.Time
	items     []queueItem
	conc      int
	seq       int
}

// Panel 队列字段在 Panel 结构体上（panel.go）由 initQueue 惰性初始化；
// 这里集中访问器，避免改动 New 构造链。
func (p *Panel) queue() *queueState {
	p.queueOnce.Do(func() { p.q = &queueState{} })
	return p.q
}

// tasksRunQueue 启动执行队列：{concurrency:1-4, growth:bool, school:bool}。
// 先做一次扫描，把全部待办项排队（growth 按账号内 autoActions 顺序执行，
// school 逐账号跑闭环），账号内串行、账号间受并发信号量约束。
func (p *Panel) tasksRunQueue(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Concurrency int  `json:"concurrency"`
		Growth      bool `json:"growth"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if !body.Growth {
		body.Growth = true
	}
	if body.Concurrency < 1 {
		body.Concurrency = 1
	}
	if body.Concurrency > 4 {
		body.Concurrency = 4
	}
	started, total, seq, msg := p.startGrowthQueue(body.Concurrency, body.Growth)
	switch {
	case seq == -1:
		writeErr(w, http.StatusConflict, msg)
	case !started:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": false, "message": msg})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true, "total": total, "seq": seq})
	}
}

// startGrowthQueue 扫描全部账号待办并启动队列（HTTP「执行全部待办」与调度器
// growth 时点共用核心）。返回 (started, total, seq, msg)：seq==-1 表示队列
// 已在执行（冲突）；started=false 时 msg 为无可执行待办的说明。并发夹取
// [1,4]；growth 开关同 HTTP 入参语义。
func (p *Panel) startGrowthQueue(concurrency int, growth bool) (started bool, total int, seq int, msg string) {
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > 4 {
		concurrency = 4
	}
	q := p.queue()
	q.mu.Lock()
	if q.running {
		q.mu.Unlock()
		return false, 0, -1, "队列正在执行中（可在任务中心查看进度）"
	}
	// 先占位：扫描（数秒级网络耗时）期间若并发再次触发，直接命中上面的 running
	// 判拒，避免两个 goroutine 同时启动互相覆盖 q.items/q.seq。无待办时回滚。
	q.running = true
	q.startedAt = time.Now()
	q.mu.Unlock()

	// 若 3 分钟内刚完成过一次全池扫描，直接复用缓存待办列表，避免刚点扫描又点执行时重复扫两遍；
	// 若池中新增了缓存未覆盖的活跃个人号，或缓存行含扫描报错，则回退完整扫描。
	var accts []queueAccount
	usedCache := false
	if growth {
		var snap []scanAccountItem
		var snapAt time.Time
		p.scanMu.Lock()
		if !p.scanAt.IsZero() && time.Since(p.scanAt) < 3*time.Minute && len(p.scanCache) > 0 {
			snap = make([]scanAccountItem, len(p.scanCache))
			copy(snap, p.scanCache)
			snapAt = p.scanAt
		}
		p.scanMu.Unlock()
		if !snapAt.IsZero() && len(snap) > 0 {
			seenUID := make(map[string]bool, len(snap))
			cacheHealthy := true
			for _, item := range snap {
				if item.UID == "" {
					continue
				}
				seenUID[item.UID] = true
				if item.GrowthErr != "" {
					if st, ok := p.cfg.Pool.Status(item.UID); ok && !st.Disabled {
						if a := p.cfg.Pool.AuthByUID(item.UID); a != nil && !a.IsGlobal() && !a.IsEnterprise() && !a.IsEnterpriseSession() {
							cacheHealthy = false
							break
						}
					}
				}
			}
			if cacheHealthy {
				for _, st := range p.cfg.Pool.List() {
					if st.Disabled {
						continue
					}
					a := p.cfg.Pool.AuthByUID(st.UID)
					if a == nil || a.IsGlobal() || a.IsEnterprise() || a.IsEnterpriseSession() {
						continue
					}
					if !seenUID[st.UID] {
						cacheHealthy = false
						break
					}
				}
			}
			if cacheHealthy {
				for _, item := range snap {
					if item.UID == "" || len(item.Growth) == 0 {
						continue
					}
					if st, ok := p.cfg.Pool.Status(item.UID); !ok || st.Disabled {
						continue
					}
					a := p.cfg.Pool.AuthByUID(item.UID)
					if a == nil || a.IsGlobal() || a.IsEnterprise() || a.IsEnterpriseSession() {
						continue
					}
					grow := make([]upstream.Task, len(item.Growth))
					copy(grow, item.Growth)
					sort.Slice(grow, func(i, j int) bool {
						return autoActionIndex(grow[i].TaskCode) < autoActionIndex(grow[j].TaskCode)
					})
					accts = append(accts, queueAccount{a: a, grow: grow})
				}
				usedCache = true
			}
		}
	}

	if !usedCache {
		// 扫描待办（复用扫描逻辑的拉取部分）。
		states := p.cfg.Pool.List()
		var mu sync.Mutex
		var scanAccts []*auth.Auth
		for _, st := range states {
			if st.Disabled {
				continue
			}
			a := p.cfg.Pool.AuthByUID(st.UID)
			if a == nil {
				continue
			}
			scanAccts = append(scanAccts, a)
		}
		// 扫描段并发上限与停顿走 pool.BatchForEach（默认串行+随机抖动）。
		p.cfg.Pool.BatchForEach(len(scanAccts), func(i int) {
			a := scanAccts[i]
			one := queueAccount{a: a}
			if a.IsGlobal() || a.IsEnterprise() {
				return
			}
			if a.IsEnterpriseSession() {
				log.Printf("panel: WARN 队列跳过 uid=%s nickname=%q：凭证带企业上下文（JWT enterprise_id 非空），成长任务行为事件会被上游丢弃；请移除该账号后以个人身份重新登录", a.UID, a.Nickname)
				return
			}
			if growth {
				if tasks, err := p.cfg.Upstream.ListTasks(a); err == nil {
					for _, t := range tasks {
						if growthPending(t) {
							one.grow = append(one.grow, t)
						}
					}
				}
				if gap := p.batchSubDelay(); gap > 0 {
					time.Sleep(gap)
				}
				if mpTasks, mpErr := p.cfg.Upstream.ListTasksMP(a); mpErr == nil {
					seen := map[string]bool{}
					for _, t := range one.grow {
						seen[t.TaskCode] = true
					}
					for _, t := range mpTasks {
						if growthPending(t) && !seen[t.TaskCode] {
							one.grow = append(one.grow, t)
						}
					}
				}
				sort.Slice(one.grow, func(i, j int) bool {
					return autoActionIndex(one.grow[i].TaskCode) < autoActionIndex(one.grow[j].TaskCode)
				})
			}
			if len(one.grow) > 0 {
				mu.Lock()
				accts = append(accts, one)
				mu.Unlock()
			}
		})
	}

	// 组装队列（账号分组，保持顺序）。
	var items []queueItem
	for _, one := range accts {
		for _, t := range one.grow {
			items = append(items, queueItem{UID: one.a.UID, Nickname: one.a.Nickname, Kind: "growth", Code: t.TaskCode, Status: "pending"})
		}
	}
	if len(items) == 0 {
		log.Printf("panel: 队列启动：无可执行待办（全部账号任务已完成）")
		q.mu.Lock()
		q.running = false
		q.startedAt = time.Time{}
		q.mu.Unlock()
		return false, 0, 0, "全部账号没有待办任务"
	}

	q.mu.Lock()
	q.items = items
	q.conc = concurrency
	q.seq++
	seq = q.seq
	q.mu.Unlock()

	go p.runQueueItems(accts, items, concurrency)
	log.Printf("panel: 队列启动：%d 项（并发 %d，成长 %v）", len(items), concurrency, growth)
	return true, len(items), seq, ""
}

// RunGrowthQueueOnce 调度器 growth 时点回调（sch.SetGrowthHook 挂载）：与
// 「执行全部待办」按钮完全同管线（成长，串行并发 1）。Sequential 族
// 每日零点解锁一环，此前只能手动扫描推进；此回调让链条每天自动走一环。
// 异步执行（startGrowthQueue 启动 goroutine 即返），已在跑/无待办安全跳过。
func (p *Panel) RunGrowthQueueOnce() {
	started, total, _, _ := p.startGrowthQueue(1, true)
	if started {
		log.Printf("panel: 定时成长任务队列已启动（%d 项）", total)
	}
}

// runQueueItems 队列执行主体：按账号分组，账号内串行（per-account 锁），
// 账号间并发（信号量）。每项结果写回队列状态。
func (p *Panel) runQueueItems(accts []queueAccount, items []queueItem, concurrency int) {
	q := p.queue()
	defer func() {
		q.mu.Lock()
		q.running = false
		q.mu.Unlock()
		log.Printf("panel: 队列执行结束（共 %d 项）", len(items))
	}()

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for idx, one := range accts {
		if idx > 0 {
			if d := p.batchPaceDelay(); d > 0 {
				time.Sleep(d)
			}
		}
		wg.Add(1)
		go func(one queueAccount) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			// per-account 互斥：与单任务/一键完成共用一把锁。
			if !p.tryLockAccount(one.a.UID) {
				p.queueSet(q, one.a.UID, func(it *queueItem) {
					it.Status, it.Message = "skipped", "该账号有其它任务动作在执行，跳过"
				})
				return
			}
			defer p.unlockAccount(one.a.UID)
			// 前置：批量接受尚未接受的任务。上游对 not_accepted 的任务不计数——
			// 面板「一键完成」一直有这步，队列路径此前漏了（表现为上报 200 但进度
			// 一直 not_accepted、无法领奖）。失败不阻塞（行为事件才是进度判据）。
			if accepted := p.acceptPendingTasks(one.a); accepted > 0 {
				time.Sleep(reportGap) // 给上游状态流转留时间
			}
			for i := range q.items {
				uid, kind, code := q.snapshotAt(i)
				if uid != one.a.UID {
					continue
				}
				p.queueMarkAt(i, "running", "")
				var msg string
				var err error
				switch kind {
				case "growth":
					msg, err = p.runGrowthQueued(one.a, code)
				}
				if err != nil {
					p.queueMarkAt(i, "error", err.Error())
				} else {
					p.queueMarkAt(i, "done", msg)
				}
				time.Sleep(reportGap) // 项间节流
			}
		}(one)
	}
	wg.Wait()
}

// queueAccount 队列执行的账号单元（runQueueItems 参数）。
type queueAccount struct {
	a    *auth.Auth
	grow []upstream.Task
}

// snapshotAt 锁内读条目三元组（避免锁外持有指针）。
func (q *queueState) snapshotAt(i int) (uid, kind, code string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.items[i].UID, q.items[i].Kind, q.items[i].Code
}

// queueMarkAt 按索引更新队列条目状态（条目数组固定不再增删）。
func (p *Panel) queueMarkAt(i int, status, msg string) {
	q := p.queue()
	q.mu.Lock()
	q.items[i].Status, q.items[i].Message = status, msg
	q.mu.Unlock()
}

// queueSet 按 uid 批量改状态。
func (p *Panel) queueSet(q *queueState, uid string, fn func(*queueItem)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.items {
		if q.items[i].UID == uid {
			fn(&q.items[i])
		}
	}
}

// acceptPendingTasks 批量接受该账号未接受的任务，返回接受的个数（失败返回 0 不阻塞）。
func (p *Panel) acceptPendingTasks(a *auth.Auth) int {
	tasks, err := p.cfg.Upstream.ListTasks(a)
	if err != nil {
		return 0
	}
	var codes []string
	for _, t := range tasks {
		if !t.Claimed && !t.Locked && t.AcceptStatus != "accepted" && t.AcceptStatus != "completed" {
			codes = append(codes, t.TaskCode)
		}
	}
	if len(codes) == 0 {
		return 0
	}
	if err := p.cfg.Upstream.AcceptTasks(a, codes); err != nil {
		log.Printf("panel: 队列 accept uid=%s: %v（不阻塞）", a.UID, err)
		return 0
	}
	log.Printf("panel: 队列 accept uid=%s: 已接受 %d 个任务", a.UID, len(codes))
	return len(codes)
}

// runGrowthQueued 执行单个成长任务（动作 + 回读 + 自动领奖；与
// accountTaskAuto 同语义，结果以文字返回）。
func (p *Panel) runGrowthQueued(a *auth.Auth, code string) (string, error) {
	act := autoActionFor(code)
	if act == nil {
		return "", fmt.Errorf("任务 %s 无自动动作", code)
	}
	// taskByCode 已双口径（mp 专属码自动回落 mp 列表）。
	before, err := p.taskByCode(a, code)
	if err != nil {
		return "", err
	}
	if before == nil {
		return "该账号无此任务", nil
	}
	isMP := isMPTaskCode(code)
	if before.Claimed {
		return "已完成（已领取）", nil
	}
	msg, err := act.run(p, a)
	if err != nil {
		return "", err
	}
	var after *upstream.Task
	if isMP {
		after, _ = p.taskByCodeMP(a, code)
	} else {
		after, _ = p.taskByCodeWaiting(a, code)
	}
	if after != nil && after.Claimable {
		var credit, energy int64
		var cerr error
		if isMP {
			credit, energy, cerr = p.cfg.Upstream.ClaimRewardMP(a, code)
		} else {
			credit, energy, cerr = p.cfg.Upstream.ClaimReward(a, code)
		}
		if cerr == nil && (credit > 0 || energy > 0) {
			msg += fmt.Sprintf("；自动领奖 +%d 分 +%d 能", credit, energy)
		}
	}
	if after != nil {
		msg += "（进度 " + taskProgressText(after) + "）"
	}
	log.Printf("panel: 队列 growth uid=%s code=%s: %s", a.UID, code, msg)
	return msg, nil
}

// tasksQueueStatus 队列状态（轮询用）。
func (p *Panel) tasksQueueStatus(w http.ResponseWriter, r *http.Request) {
	q := p.queue()
	q.mu.Lock()
	defer q.mu.Unlock()
	items := make([]queueItem, len(q.items))
	copy(items, q.items)
	writeJSON(w, http.StatusOK, map[string]any{
		"running":    q.running,
		"total":      len(items),
		"conc":       q.conc,
		"started":    !q.startedAt.IsZero(),
		"started_at": q.startedAt,
		"seq":        q.seq,
		"items":      items,
	})
}

func (p *Panel) vouchersMeta() cacheMeta {
	p.vcMu.Lock()
	defer p.vcMu.Unlock()
	return cacheMeta{
		FetchedAt: p.vcAt,
		Scanning:  p.vcScanning,
		Done:      p.vcDone,
		Total:     p.vcTotal,
	}
}

// schoolVouchers 只读返回开学季券码扫描的内存缓存快照（零上游请求）。
func (p *Panel) schoolVouchers(w http.ResponseWriter, r *http.Request) {
	p.vcMu.Lock()
	res := make([]voucherRow, len(p.vcCache))
	copy(res, p.vcCache)
	at := p.vcAt
	scanning := p.vcScanning
	done := p.vcDone
	total := p.vcTotal
	p.vcMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"accounts":   res,
		"cached":     true,
		"fetched_at": at,
		"scanning":   scanning,
		"done":       done,
		"total":      total,
	})
}

// schoolVouchersRefresh 显式异步单飞触发券码查询（串行+随机间隔，防并发扫号）。
func (p *Panel) schoolVouchersRefresh(w http.ResponseWriter, r *http.Request) {
	started := p.startSchoolVouchersAsync()
	meta := p.vouchersMeta()
	msg := "券码查询已开始（串行防探测节奏）"
	if !started {
		msg = "券码查询正在进行中"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"started":    started,
		"scanning":   meta.Scanning,
		"done":       meta.Done,
		"total":      meta.Total,
		"fetched_at": meta.FetchedAt,
		"message":    msg,
	})
}

func (p *Panel) startSchoolVouchersAsync() bool {
	p.vcMu.Lock()
	if p.vcScanning {
		p.vcMu.Unlock()
		return false
	}
	p.vcScanning = true
	p.vcDone = 0
	p.vcTotal = 0
	p.vcMu.Unlock()

	go func() {
		p.vcRun.Do(func() {
			p.refreshSchoolVouchersSync()
		})
	}()
	return true
}

func (p *Panel) refreshSchoolVouchersSync() []voucherRow {
	accts := p.cfg.Pool.List()
	out := make([]voucherRow, len(accts))
	type voucherJob struct {
		idx int
		a   *auth.Auth
	}
	var jobs []voucherJob
	for i, st := range accts {
		if st.Disabled {
			continue // 未占位，行末统一压掉
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		jobs = append(jobs, voucherJob{idx: i, a: a})
	}
	p.vcMu.Lock()
	p.vcScanning = true
	p.vcDone = 0
	p.vcTotal = len(jobs)
	p.vcMu.Unlock()

	p.cfg.Pool.BatchForEachProgress(len(jobs), func(k int) {
		j := jobs[k]
		a := j.a
		it := voucherRow{UID: a.UID, Nickname: a.Nickname}
		switch {
		case a.IsGlobal():
			it.Err = "global realm（无开学季活动）"
		default:
			vs, err := p.cfg.Upstream.SchoolVouchers(a)
			if err != nil {
				it.Err = err.Error()
			} else {
				it.Vouchers = vs
			}
		}
		out[j.idx] = it
	}, func(done, total int) {
		p.vcMu.Lock()
		p.vcDone = done
		p.vcTotal = total
		p.vcMu.Unlock()
	})
	res := make([]voucherRow, 0, len(out))
	for _, it := range out {
		if it.UID != "" {
			res = append(res, it)
		}
	}
	cached := make([]voucherRow, len(res))
	copy(cached, res)
	p.vcMu.Lock()
	p.vcCache = cached
	p.vcAt = time.Now()
	p.vcScanning = false
	p.vcDone = len(jobs)
	p.vcTotal = len(jobs)
	p.vcMu.Unlock()
	return res
}
