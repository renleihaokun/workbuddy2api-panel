// jitter.go 按账号的分钟级任务时刻抖动。
//
// 背景：排程只精确到小时（config 的 checkin_hours=[9,21] 等），24 个账号会在
// 同一个整点同时发起签到 / 上报 / 保活 / 夜猫子。网关的出口 IP 只有一个（反代
// 部署在单机上，出口 NAT 是固定的），所以「同一 IP 上多个账号在同一时刻做完全
// 相同的动作」是比 HTTP 头更直接的关联特征——上游不需要任何指纹就能把它们归到
// 一起。
//
// 出口 IP 分化不了（单个 NAT 出口），那就必须在时间上分化：让每个账号落在该
// 小时内的**不同分钟**。抖动按 uid 稳定散列、按自然日轮换——同一账号当天恒定
// （重试/重启不会漂到别的时刻），跨天换一批分钟（避免长期固定的行为节律）。
package scheduler

import (
	"crypto/sha256"
	"encoding/binary"
	"sync"
	"time"
)

// DefaultJitterMinutes 默认抖动窗口（分钟）。0 = 关闭抖动（回到整点同时打）。
//
// 取 45 而不是 60：留 15 分钟余量，避免抖到尾部的账号撞上下一小时的排程
// （例如 checkin_hours 同时含 9 和 21 时不会互相侵入）。
const DefaultJitterMinutes = 45

// accountSlotMinute 返回账号在某个任务、某个自然日的分钟偏移（0 <= 偏移 < window）。
//
// 口径：
//   - 按 uid 稳定（同一账号恒定），按 task 区分（签到与上报不必落在同一分钟），
//     按日期轮换（跨天换批）；
//   - window <= 0 时返回 0（抖动关闭，行为与引入前逐秒一致）。
//
// 用 sha256 而不是 math/rand：结果必须跨进程重启稳定——重启后同一账号不能漂到
// 另一个分钟，否则「这个账号的行为时刻」本身就成了漂移特征。
func accountSlotMinute(uid, task string, day string, window int) int {
	if window <= 0 || uid == "" {
		return 0
	}
	sum := sha256.Sum256([]byte("wb2a:jitter:" + task + ":" + day + ":" + uid))
	// 取前 4 字节作为无符号整数再取模：分布均匀且与平台字节序无关。
	n := binary.BigEndian.Uint32(sum[:4])
	return int(n % uint32(window))
}

// jitteredFireAt 把「某小时整点」按账号偏移到分钟级时刻。
//
// day 由调用方按**本地自然日**给出（与排程时区一致），保证跨天轮换的判定
// 与用户看到的日期对齐。
func jitteredFireAt(base time.Time, task, uid string, window int) time.Time {
	day := base.Format("2006-01-02")
	off := accountSlotMinute(uid, task, day, window)
	if off == 0 {
		return base
	}
	return base.Add(time.Duration(off) * time.Minute)
}

// dueForAccount 报告 now 是否已到该账号本次任务的抖动时刻。
//
// base 是本轮任务的小时起点（如 09:00），账号实际时点 = base + 抖动量
// （0 ~ window-1 分钟，且不超出本小时）。返回 true 表示已到期。
func dueForAccount(now, base time.Time, uid, task string, window int) bool {
	if now.Before(base) {
		return false // 还没到小时起点
	}
	return !now.Before(jitteredFireAt(base, task, uid, window))
}

// firedMarker 生成「本账号本任务本小时已执行过」的台账键。
//
// 为什么必须有台账：抖动续跑会在窗口内每分钟唤醒一轮，而 dueForAccount 是
// **「已到期」**语义——09:07 到点的账号在 09:08、09:09… 每一轮都会再次返回
// true。没有台账的话，45 分钟窗口 × 24 个账号会给上游打上千次重复签到
// （签到虽幂等，但这种请求量本身就是异常特征，比漏签更糟）。
//
// 键里带小时起点，所以：
//   - 同一小时窗口内，一个账号只执行一次；
//   - 下一个排程时点（如 21:00）是新键，照常执行；
//   - 跨天不冲突（日期已在时间戳里）。
func firedMarker(base time.Time, task, uid string) string {
	return base.Format("20060102T15") + "|" + task + "|" + uid
}

// slotLedger 按账号记录「本轮已执行」的台账（进程内，勿持久化）。
//
// 不持久化的理由：重启后重跑一轮是安全的——签到返回「今天已签到」、上报
// 服务端按每日一次计、保活重复刷新无害；而持久化反而会在时钟回拨/改配置后
// 留下「以为跑过其实没跑」的脏状态。重启频率远低于窗口内 tick 频率。
type slotLedger struct {
	mu   sync.Mutex
	seen map[string]time.Time // marker → 首次记录时刻（用于清理）
}

// claim 尝试占用一个标记；返回 true 表示本次由调用方执行，false 表示已执行过。
func (l *slotLedger) claim(marker string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seen == nil {
		l.seen = make(map[string]time.Time, 64)
	}
	if _, done := l.seen[marker]; done {
		return false
	}
	l.seen[marker] = now
	// 顺手清理：标记按小时递增，保留最近 48 小时足够覆盖任何窗口/回拨场景。
	// 不清理会让长跑进程的 map 无界增长（每小时 24 号 × 3 任务 ≈ 72 条）。
	if len(l.seen) > 4096 {
		cutoff := now.Add(-48 * time.Hour)
		for k, at := range l.seen {
			if at.Before(cutoff) {
				delete(l.seen, k)
			}
		}
	}
	return true
}

// reset 清空台账（测试用）。
func (l *slotLedger) reset() {
	l.mu.Lock()
	l.seen = nil
	l.mu.Unlock()
}

// shouldFire 综合判定「本账号本次任务此刻是否该执行」：
// 已到抖动时刻 **且** 本轮尚未执行过。
//
// 这是三个任务体（签到 / 活跃上报 / 保活）的唯一判定入口——把「到点」与
// 「只跑一次」两件事绑在一起，避免调用点各写一遍漏掉台账。
//
// scheduled=false（面板手动触发）时不做任何抖动与台账判定：用户点了按钮就
// 该立刻对全部账号执行，与定时排程互不干扰。
func (s *Scheduler) shouldFire(now, hourBase time.Time, uid, task string, scheduled bool) bool {
	if !scheduled {
		return true
	}
	window := s.jitterMinutes()
	if window <= 0 {
		return true // 抖动关闭：回到整点全跑（与引入前逐秒一致）
	}
	if !dueForAccount(now, hourBase, uid, task, window) {
		return false // 未到本账号的分钟点
	}
	return s.ledger.claim(firedMarker(hourBase, task, uid), now)
}
