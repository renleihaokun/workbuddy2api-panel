// jitter_test.go 按账号任务时刻抖动的口径契约。
//
// 抖动的意义在「时间维度分化」——单出口 IP 下，24 个账号若在同一整点做同样
// 的动作，上游不需要任何指纹就能把它们归到一起。所以这里钉四件事：
// 稳定（跨重启不漂）、按任务与日期区分、分布不塌缩、关掉后逐秒回到整点。
package scheduler

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestAccountSlotMinuteStable 同一 (uid, task, day) 恒得同一分钟——幂等是前提：
// 重启后若漂到别的分钟，「这个账号的行为时刻」本身就成了漂移特征。
func TestAccountSlotMinuteStable(t *testing.T) {
	const uid, task, day = "uid-abc", "checkin", "2026-11-25"
	first := accountSlotMinute(uid, task, day, 45)
	for i := 0; i < 50; i++ {
		if got := accountSlotMinute(uid, task, day, 45); got != first {
			t.Fatalf("第 %d 次调用 = %d，want 恒为 %d", i, got, first)
		}
	}
}

// TestAccountSlotMinuteInWindow 偏移恒落在 [0, window) 内。
func TestAccountSlotMinuteInWindow(t *testing.T) {
	for _, window := range []int{1, 15, 45, 59} {
		for _, uid := range []string{"a", "b", "uid-1", "uid-2", "很长的中文 uid 测试"} {
			got := accountSlotMinute(uid, "checkin", "2026-11-25", window)
			if got < 0 || got >= window {
				t.Errorf("window=%d uid=%q → %d 越界", window, uid, got)
			}
		}
	}
}

// TestAccountSlotMinuteDisabled window<=0 或空 uid 时必须返回 0（抖动关闭）。
func TestAccountSlotMinuteDisabled(t *testing.T) {
	if got := accountSlotMinute("uid-1", "checkin", "2026-11-25", 0); got != 0 {
		t.Errorf("window=0 → %d, want 0（关闭抖动）", got)
	}
	if got := accountSlotMinute("uid-1", "checkin", "2026-11-25", -5); got != 0 {
		t.Errorf("window<0 → %d, want 0", got)
	}
	if got := accountSlotMinute("", "checkin", "2026-11-25", 45); got != 0 {
		t.Errorf("空 uid → %d, want 0", got)
	}
}

// TestAccountSlotMinuteRotatesDaily 跨自然日必须换一批分钟——长期固定节律本身
// 也是一种可判别形态。
func TestAccountSlotMinuteRotatesDaily(t *testing.T) {
	const uid, task = "uid-abc", "checkin"
	const window = 45
	// 统计 14 天里有几天与首日同分钟。随机分布下期望极低；只要不是每天都一样
	// 就说明日期参与散列。
	first := accountSlotMinute(uid, task, "2026-11-01", window)
	same := 0
	for d := 2; d <= 15; d++ {
		day := fmt.Sprintf("2026-11-%02d", d)
		if accountSlotMinute(uid, task, day, window) == first {
			same++
		}
	}
	if same >= 13 {
		t.Errorf("14 天里有 %d 天与首日同分钟，日期似乎没有参与散列", same)
	}
}

// TestAccountSlotMinuteDistinctTasks 不同任务对同一账号可落在不同分钟
// （签到与上报不必同刻，否则又形成一个「同刻行为组」）。
func TestAccountSlotMinuteDistinctTasks(t *testing.T) {
	const uid, day = "uid-abc", "2026-11-25"
	const window = 45
	diff := 0
	for _, task := range []string{"checkin", "activity", "keepalive", "blackcat", "travel"} {
		if accountSlotMinute(uid, task, day, window) != accountSlotMinute(uid, "checkin", day, window) {
			diff++
		}
	}
	if diff == 0 {
		t.Error("五个任务的偏移全部相同，任务名似乎没有参与散列")
	}
}

// TestAccountSlotsSpreadAcrossAccounts 关键性质：多个账号必须散开，不能塌缩到
// 同一分钟。若全部账号同刻，抖动就完全没起作用。
func TestAccountSlotsSpreadAcrossAccounts(t *testing.T) {
	const task, day = "checkin", "2026-11-25"
	const window = 45
	const n = 24 // 生产池规模

	seen := make(map[int]int)
	for i := 0; i < n; i++ {
		uid := fmt.Sprintf("workbuddy-%04d", i)
		seen[accountSlotMinute(uid, task, day, window)]++
	}
	// 24 个账号落进 45 个槽位：允许碰撞，但绝不能全挤在少数几个槽里。
	if len(seen) < 10 {
		t.Errorf("24 个账号只用到 %d 个分钟槽（window=%d），分布塌缩：%v", len(seen), window, seen)
	}
	// 最大同槽数不应超过 6（期望值 ≈ 24/45×e ≈ 2.3，6 已是宽松上界）。
	maxBucket := 0
	for _, c := range seen {
		if c > maxBucket {
			maxBucket = c
		}
	}
	if maxBucket > 6 {
		t.Errorf("单个分钟槽最多挤了 %d 个账号（window=%d），分布不均：%v", maxBucket, window, seen)
	}
}

// TestJitteredFireAt 抖动后的时刻 = 小时基点 + 偏移分钟，不跨越下一小时。
func TestJitteredFireAt(t *testing.T) {
	base := time.Date(2026, 11, 25, 9, 0, 0, 0, time.Local)
	got := jitteredFireAt(base, "checkin", "uid-abc", 45)
	if got.Before(base) {
		t.Errorf("抖动后的时刻 %v 早于基点 %v", got, base)
	}
	if d := got.Sub(base); d >= 45*time.Minute {
		t.Errorf("偏移 %v 超出窗口 45 分钟", d)
	}
	// 不得跨进下一小时。
	if got.Hour() != 9 {
		t.Errorf("抖动后落到 %v（跨出了 9 点窗口）", got)
	}
	// 秒级不得被改动（只在分钟维度偏移）。
	if got.Second() != 0 || got.Nanosecond() != 0 {
		t.Errorf("抖动后秒/纳秒非零：%v", got)
	}
}

// TestJitteredFireAtDisabled 窗口关闭时逐秒等于基点（零行为回归）。
func TestJitteredFireAtDisabled(t *testing.T) {
	base := time.Date(2026, 11, 25, 9, 0, 0, 0, time.Local)
	if got := jitteredFireAt(base, "checkin", "uid-abc", 0); !got.Equal(base) {
		t.Errorf("window=0 → %v, want 恒等基点 %v", got, base)
	}
}

// TestDueForAccount 到期判定：早于基点不执行；介于基点与抖动时刻之间不执行；
// 到点或之后执行（tick 晚到也应补执行，幂等由各任务保证）。
func TestDueForAccount(t *testing.T) {
	base := time.Date(2026, 11, 25, 9, 0, 0, 0, time.Local)
	const uid, task, window = "uid-abc", "checkin", 45
	slot := jitteredFireAt(base, task, uid, window)

	if dueForAccount(base.Add(-time.Minute), base, uid, task, window) {
		t.Error("基点之前不应执行")
	}
	// 若抖动偏移 > 0，则基点当刻尚未到点。
	if slot.After(base) && dueForAccount(base, base, uid, task, window) {
		t.Error("抖动时刻之前不应执行")
	}
	if !dueForAccount(slot, base, uid, task, window) {
		t.Error("恰好到点应执行")
	}
	if !dueForAccount(slot.Add(30*time.Minute), base, uid, task, window) {
		t.Error("tick 晚到仍应补执行（幂等由任务保证）")
	}
}

// TestDueForAccountDisabledAlwaysDue 关闭抖动时基点到点即执行。
func TestDueForAccountDisabledAlwaysDue(t *testing.T) {
	base := time.Date(2026, 11, 25, 9, 0, 0, 0, time.Local)
	if !dueForAccount(base, base, "uid-abc", "checkin", 0) {
		t.Error("抖动关闭时基点到点应执行")
	}
	if dueForAccount(base.Add(-time.Second), base, "uid-abc", "checkin", 0) {
		t.Error("基点之前不应执行")
	}
}

// TestJitterMinutesDefaultFromConfig 零值 Config = **不抖动**（关键契约）。
//
// 默认 45 分钟由配置装配层（cmd/server normalize）落地，不在 scheduler 包里
// 硬编码。原因：scheduler 的零值 Config 被大量排程推演用例直接构造，若这里
// 凭空给出 45 分钟窗口，既有的整点排程断言全部失真（首版就踩了这条）。
// 生产路径必过 normalize，所以线上默认仍是 45。
func TestJitterMinutesDefaultFromConfig(t *testing.T) {
	s := &Scheduler{}
	if got := s.jitterMinutes(); got != 0 {
		t.Errorf("零值 Config 的 jitterMinutes = %d, want 0（装配层负责给默认值）", got)
	}

	zero := 0
	s.SetJitterMinutes(&zero)
	if got := s.jitterMinutes(); got != 0 {
		t.Errorf("显式 0 时 jitterMinutes = %d, want 0（关闭）", got)
	}

	twenty := 20
	s.SetJitterMinutes(&twenty)
	if got := s.jitterMinutes(); got != 20 {
		t.Errorf("显式 20 时 jitterMinutes = %d, want 20", got)
	}

	s.SetJitterMinutes(nil)
	if got := s.jitterMinutes(); got != 0 {
		t.Errorf("清空后 jitterMinutes = %d, want 0", got)
	}

	// 装配层给的默认值（45）必须能被本包正确识别并启用续跑。
	def := DefaultJitterMinutes
	s2 := &Scheduler{cfg: Config{CheckinHours: []int{9}, JitterMinutes: &def}}
	at := time.Date(2026, 11, 25, 9, 0, 10, 0, time.Local)
	if next, _ := s2.jitterContinuation(at); next.IsZero() {
		t.Error("装配层给出默认 45 分钟时，窗口内应续跑")
	}
}

// TestManualTriggerBypassesJitter 回归：**面板手动触发不受抖动门控**。
//
// 抖动只该作用于定时排程。若把门控加到手动入口上，用户点「立即签到」时多数
// 账号会被跳过（只有恰好到点的那个执行），表现为「点了没反应」——这是一次
// 真实的实现错误（首版就踩了：RunCheckinNow 走同一路径导致测试挂死）。
func TestManualTriggerBypassesJitter(t *testing.T) {
	// 手动口径走 jitter=0，此时 dueForAccount 在小时起点即恒真。
	base := time.Date(2026, 11, 25, 9, 0, 0, 0, time.Local)
	for _, uid := range []string{"u00", "u01", "u02", "u03", "u04", "u05"} {
		if !dueForAccount(base, base, uid, "checkin", 0) {
			t.Errorf("uid=%s: 手动口径（jitter=0）在小时起点应全部放行", uid)
		}
	}
	// 对照：开启抖动后，同一时刻并非所有账号都已到点 —— 证明门控确实有作用，
	// 否则上面那条断言就是空转。
	jittered := 0
	for _, uid := range []string{"u00", "u01", "u02", "u03", "u04", "u05"} {
		if dueForAccount(base, base, uid, "checkin", DefaultJitterMinutes) {
			jittered++
		}
	}
	if jittered == 6 {
		t.Error("抖动窗口内 6 个账号在基点全部到点，抖动似乎没有生效")
	}
}

// TestJitterContinuationRearmsWithinWindow 回归（线上暴露的真实缺陷）：
//
// 排程原本只在整点唤醒一次，抖动把部分账号摊到 h:07/h:23…，那一轮它们因
// 「未到点」被跳过，调度器随即排到下一天 —— 这些账号当天就永远不执行了。
// 修复后窗口内必须每分钟续跑一轮。
func TestJitterContinuationRearmsWithinWindow(t *testing.T) {
	fortyFive := DefaultJitterMinutes
	s := &Scheduler{cfg: Config{CheckinHours: []int{9}, JitterMinutes: &fortyFive}}

	// 9:00（窗口内第 0 分钟）→ 应续跑，下一跳在 9:01。
	at0 := time.Date(2026, 11, 25, 9, 0, 10, 0, time.Local)
	next, kinds := s.jitterContinuation(at0)
	if next.IsZero() {
		t.Fatal("9:00 在窗口内应续跑，实际没有排下一跳")
	}
	if want := time.Date(2026, 11, 25, 9, 1, 0, 0, time.Local); !next.Equal(want) {
		t.Errorf("下一次唤醒 = %v, want %v", next, want)
	}
	if len(kinds) != 1 || kinds[0] != taskCheckin {
		t.Errorf("kinds = %v, want [taskCheckin]", kinds)
	}

	// 9:30（仍在 45 分钟窗口内）→ 继续续跑。
	at30 := time.Date(2026, 11, 25, 9, 30, 5, 0, time.Local)
	if next, _ := s.jitterContinuation(at30); next.IsZero() {
		t.Error("9:30 仍在窗口内，应继续续跑")
	}

	// 9:45（窗口已满 45 分钟）→ 不再续跑，交回 nextFire 排下一整点。
	at45 := time.Date(2026, 11, 25, 9, 45, 0, 0, time.Local)
	if next, _ := s.jitterContinuation(at45); !next.IsZero() {
		t.Errorf("9:45 窗口已结束，不该再续跑，得到 %v", next)
	}

	// 10:00 不是配置时点 → 不续跑。
	at10 := time.Date(2026, 11, 25, 10, 5, 0, 0, time.Local)
	if next, _ := s.jitterContinuation(at10); !next.IsZero() {
		t.Errorf("10:05 不是签到时点，不该续跑，得到 %v", next)
	}
}

// TestJitterContinuationDisabledWhenJitterOff 关闭抖动时绝不续跑（零行为回归）。
func TestJitterContinuationDisabledWhenJitterOff(t *testing.T) {
	zero := 0
	s := &Scheduler{cfg: Config{CheckinHours: []int{9}, JitterMinutes: &zero}}
	at := time.Date(2026, 11, 25, 9, 0, 10, 0, time.Local)
	if next, _ := s.jitterContinuation(at); !next.IsZero() {
		t.Errorf("jitter=0 时不该续跑，得到 %v", next)
	}
}

// TestJitterContinuationSkipsDisabledTasks 关闭的任务不进续跑候选。
func TestJitterContinuationSkipsDisabledTasks(t *testing.T) {
	fortyFive := 45
	s := &Scheduler{cfg: Config{
		CheckinHours:    []int{9},
		ActivityHours:   []int{9},
		CheckinDisabled: true,
		JitterMinutes:   &fortyFive,
	}}
	at := time.Date(2026, 11, 25, 9, 0, 10, 0, time.Local)
	_, kinds := s.jitterContinuation(at)
	for _, k := range kinds {
		if k == taskCheckin {
			t.Error("签到已禁用，不该出现在续跑候选里")
		}
	}
	if len(kinds) != 1 || kinds[0] != taskActivity {
		t.Errorf("kinds = %v, want 只剩 [taskActivity]", kinds)
	}
}

// TestJitterWindowEventuallyReleasesEveryAccount 端到端语义：
// 在一个抖动窗口内逐分钟 tick，24 个账号必须**全部**最终被放到。
//
// 这条锁死「不会漏号」——正是线上缺陷的本质（跑一轮就跳到下一天）。
func TestJitterWindowEventuallyReleasesEveryAccount(t *testing.T) {
	const window = DefaultJitterMinutes
	uids := make([]string, 24)
	for i := range uids {
		uids[i] = fmt.Sprintf("workbuddy-%04d", i)
	}

	released := make(map[string]bool, len(uids))
	hourBase := time.Date(2026, 11, 25, 9, 0, 0, 0, time.Local)
	// 窗口内每分钟一个 tick（模拟调度器续跑）。
	for m := 0; m < window; m++ {
		now := hourBase.Add(time.Duration(m) * time.Minute)
		for _, uid := range uids {
			if dueForAccount(now, hourBase, uid, "checkin", window) {
				released[uid] = true
			}
		}
	}
	if len(released) != len(uids) {
		missing := make([]string, 0)
		for _, uid := range uids {
			if !released[uid] {
				missing = append(missing, uid)
			}
		}
		t.Fatalf("窗口结束后仍有 %d 个账号没被放出: %v", len(missing), missing)
	}
}

// TestShouldFireClaimsOnce 复核补的关键约束：**窗口内每个账号只执行一次**。
//
// dueForAccount 是「已到期」语义——到点之后每一轮都返回 true。续跑会每分钟
// 醒一次，若不记账，45 分钟窗口 × 24 个账号会给上游打上千次重复签到（签到
// 虽幂等，但这种请求量本身比漏签更像异常）。shouldFire 必须把「到点」与
// 「只跑一次」绑在一起。
func TestShouldFireClaimsOnce(t *testing.T) {
	const window = DefaultJitterMinutes
	def := window
	s := &Scheduler{cfg: Config{JitterMinutes: &def}}
	hourBase := time.Date(2026, 11, 25, 9, 0, 0, 0, time.Local)

	const uid = "workbuddy-0007"
	slot := jitteredFireAt(hourBase, "checkin", uid, window)

	// 到点之前：不执行。
	if s.shouldFire(slot.Add(-time.Minute), hourBase, uid, "checkin", true) {
		t.Fatal("未到抖动时刻不该执行")
	}
	// 恰好到点：执行一次。
	if !s.shouldFire(slot, hourBase, uid, "checkin", true) {
		t.Fatal("到点应立即执行")
	}
	// 窗口内后续每一轮 tick：都必须返回 false（已执行过）。
	for m := 0; m < window; m++ {
		now := hourBase.Add(time.Duration(m) * time.Minute)
		if !now.Before(slot) && s.shouldFire(now, hourBase, uid, "checkin", true) {
			t.Fatalf("%v 时该账号已在 %v 执行过，不该重复执行", now, slot)
		}
	}
	// 下一个排程时点（21:00）是新键：照常执行。
	nextHour := hourBase.Add(12 * time.Hour)
	nextSlot := jitteredFireAt(nextHour, "checkin", uid, window)
	if !s.shouldFire(nextSlot, nextHour, uid, "checkin", true) {
		t.Error("下一个排程时点应重新允许执行（台账按小时分键）")
	}
}

// TestShouldFireWholeWindowRequestCount 复核量化约束：
// 一个窗口内对 24 个账号的判定，必须恰好放出 24 次，不多不少。
//
// 这条同时挡住两个方向的错误：漏号（<24）与重复（>24）。
func TestShouldFireWholeWindowRequestCount(t *testing.T) {
	const window = DefaultJitterMinutes
	def := window
	s := &Scheduler{cfg: Config{JitterMinutes: &def}}
	hourBase := time.Date(2026, 11, 25, 9, 0, 0, 0, time.Local)

	uids := make([]string, 24)
	for i := range uids {
		uids[i] = fmt.Sprintf("workbuddy-%04d", i)
	}

	fired := make(map[string]int)
	for m := 0; m < window; m++ {
		now := hourBase.Add(time.Duration(m) * time.Minute)
		for _, uid := range uids {
			if s.shouldFire(now, hourBase, uid, "checkin", true) {
				fired[uid]++
			}
		}
	}

	total := 0
	for _, uid := range uids {
		switch fired[uid] {
		case 1:
			// 正好
		case 0:
			t.Errorf("%s 整个窗口内没被执行（漏号）", uid)
		default:
			t.Errorf("%s 被执行了 %d 次（重复）", uid, fired[uid])
		}
		total += fired[uid]
	}
	if total != len(uids) {
		t.Errorf("窗口内总执行次数 = %d, want %d（每号恰好一次）", total, len(uids))
	}
}

// TestShouldFireManualAlwaysTrue 手动触发不受抖动与台账影响。
func TestShouldFireManualAlwaysTrue(t *testing.T) {
	def := DefaultJitterMinutes
	s := &Scheduler{cfg: Config{JitterMinutes: &def}}
	hourBase := time.Date(2026, 11, 25, 9, 0, 0, 0, time.Local)
	for i := 0; i < 3; i++ {
		if !s.shouldFire(hourBase, hourBase, "u1", "checkin", false) {
			t.Fatalf("第 %d 次手动触发被拦截（手动口径应恒 true）", i+1)
		}
	}
}

// TestShouldFireJitterOffAlwaysTrue 抖动关闭时恒 true（零行为回归）。
func TestShouldFireJitterOffAlwaysTrue(t *testing.T) {
	zero := 0
	s := &Scheduler{cfg: Config{JitterMinutes: &zero}}
	hourBase := time.Date(2026, 11, 25, 9, 0, 0, 0, time.Local)
	for i := 0; i < 3; i++ {
		if !s.shouldFire(hourBase, hourBase, "u1", "checkin", true) {
			t.Fatal("抖动关闭时定时排程应恒执行")
		}
	}
}

// TestFiredMarkerScopes 台账键必须按 小时 / 任务 / 账号 三者隔离。
func TestFiredMarkerScopes(t *testing.T) {
	h9 := time.Date(2026, 11, 25, 9, 0, 0, 0, time.Local)
	h21 := time.Date(2026, 11, 25, 21, 0, 0, 0, time.Local)

	base := firedMarker(h9, "checkin", "u1")
	if base == firedMarker(h21, "checkin", "u1") {
		t.Error("不同小时必须分键")
	}
	if base == firedMarker(h9, "activity", "u1") {
		t.Error("不同任务必须分键")
	}
	if base == firedMarker(h9, "checkin", "u2") {
		t.Error("不同账号必须分键")
	}
	if base != firedMarker(h9, "checkin", "u1") {
		t.Error("同小时同任务同账号必须同键（幂等）")
	}
}

// TestSlotLedgerClaimAndCleanup 台账 claim 语义与容量清理。
func TestSlotLedgerClaimAndCleanup(t *testing.T) {
	var l slotLedger
	now := time.Date(2026, 11, 25, 9, 0, 0, 0, time.Local)
	if !l.claim("k1", now) {
		t.Fatal("首次 claim 应成功")
	}
	if l.claim("k1", now) {
		t.Fatal("重复 claim 应失败")
	}
	if !l.claim("k2", now) {
		t.Fatal("不同键首次 claim 应成功")
	}

	// 容量清理：塞入大量旧标记后 map 不应无限增长。
	for i := 0; i < 5000; i++ {
		l.claim(fmt.Sprintf("old-%d", i), now.Add(-72*time.Hour))
	}
	l.claim("fresh", now)
	l.mu.Lock()
	size := len(l.seen)
	l.mu.Unlock()
	if size > 5000 {
		t.Errorf("台账未清理，size=%d", size)
	}
}

// TestSlotLedgerConcurrent 并发 claim 同一键只能有一个成功。
func TestSlotLedgerConcurrent(t *testing.T) {
	var l slotLedger
	now := time.Now()
	const n = 50
	var wg sync.WaitGroup
	var ok atomic.Int32
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.claim("same-key", now) {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := ok.Load(); got != 1 {
		t.Errorf("并发 claim 同一键成功了 %d 次，want 恰好 1 次", got)
	}
}

// TestShouldFireConcurrentNoDoubleFire 并发压力：多个 goroutine 同时对一个
// 窗口内的 24 个账号做判定，每个账号仍必须恰好执行一次。
//
// 续跑 tick 与面板手动触发可能并发进入（复查发现锁是**非嵌套**持有：
// jitterMinutes 取 schedMu 后即释放，再取 ledger 自己的锁），这里用压力
// 用例钉住「不会双发、不会死锁」。
func TestShouldFireConcurrentNoDoubleFire(t *testing.T) {
	const window = DefaultJitterMinutes
	def := window
	s := &Scheduler{cfg: Config{JitterMinutes: &def}}
	hourBase := time.Date(2026, 11, 25, 9, 0, 0, 0, time.Local)

	uids := make([]string, 24)
	for i := range uids {
		uids[i] = fmt.Sprintf("cb-%04d", i)
	}

	var mu sync.Mutex
	fired := make(map[string]int)
	var wg sync.WaitGroup

	// 8 个并发 worker，每个跑完整个窗口的每分钟 tick。
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for m := 0; m < window; m++ {
				now := hourBase.Add(time.Duration(m) * time.Minute)
				for _, uid := range uids {
					if s.shouldFire(now, hourBase, uid, "checkin", true) {
						mu.Lock()
						fired[uid]++
						mu.Unlock()
					}
				}
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	for _, uid := range uids {
		if fired[uid] != 1 {
			t.Errorf("%s 并发场景下被执行 %d 次，want 恰好 1 次", uid, fired[uid])
		}
	}
}
