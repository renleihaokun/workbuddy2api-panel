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
// （0 ~ window-1 分钟，且不超出本小时）。返回 true 表示可以执行。
//
// 注意语义是「已到期」而不是「恰好此刻」：调度 tick 可能晚于抖动时刻很多
// （例如进程刚启动、或上一轮任务跑久了），那时仍应执行——幂等由各任务自身
// 保证（签到返回「今天已签到」、上报每日一次、保活重复刷新无害）。
func dueForAccount(now, base time.Time, uid, task string, window int) bool {
	if now.Before(base) {
		return false // 还没到小时起点
	}
	return !now.Before(jitteredFireAt(base, task, uid, window))
}
