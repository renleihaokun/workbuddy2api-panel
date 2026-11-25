// jitter_test.go 按账号任务时刻抖动的口径契约。
//
// 抖动的意义在「时间维度分化」——单出口 IP 下，24 个账号若在同一整点做同样
// 的动作，上游不需要任何指纹就能把它们归到一起。所以这里钉四件事：
// 稳定（跨重启不漂）、按任务与日期区分、分布不塌缩、关掉后逐秒回到整点。
package scheduler

import (
	"fmt"
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
