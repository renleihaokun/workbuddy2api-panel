// loss_gate.go 批量掉号风控闸门与账号失效事件通知。
//
// 当短时间窗口内多个不同账号连续被判 session_dead（默认 10 分钟内 ≥ 3 个），
// 说明上游可能正在进行批量风控或会话清理：触发闸门暂停后台自动调度任务并
// 记录高亮告警，避免继续盲目保活/轮询扩大损失；人工核查后经面板或重启恢复。
package pool

import (
	"fmt"
	"log"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

type batchDeadEvent struct {
	at  time.Time
	uid string
}

// LossEvent 账号失效或风控闸门触发事件（供外部 append-only 台账消费）。
type LossEvent struct {
	Event  string
	UID    string
	Reason string
	Auth   *auth.Auth
}

// BatchDeadGateState 批量掉号闸门当前状态（供面板 /panel/api/overview 展示）。
type BatchDeadGateState struct {
	Tripped       bool      `json:"tripped"`
	TrippedAt     time.Time `json:"tripped_at,omitempty"`
	RecentCount   int       `json:"recent_count"`
	Threshold     int       `json:"threshold"`
	WindowSeconds int       `json:"window_seconds"`
}

// SetBatchDeadGate 配置批量判死闸门阈值与滑动窗口；threshold<=0 表示关闭闸门。
func (p *Pool) SetBatchDeadGate(threshold int, window time.Duration) {
	if threshold < 0 {
		threshold = 0
	}
	if window <= 0 {
		window = 10 * time.Minute
	}
	p.mu.Lock()
	p.batchDeadThreshold = threshold
	p.batchDeadWindow = window
	p.mu.Unlock()
}

// SetBatchDeadTripHook 设置闸门首次触发时的回调（在 p.mu 锁外调用）。
func (p *Pool) SetBatchDeadTripHook(fn func(count int, window time.Duration)) {
	p.mu.Lock()
	p.onBatchDeadTrip = fn
	p.mu.Unlock()
}

// SetLossObserver 设置账号失效台账回调（在 p.mu 锁外调用）。
func (p *Pool) SetLossObserver(fn func(LossEvent)) {
	p.mu.Lock()
	p.onLoss = fn
	p.mu.Unlock()
}

// BatchDeadGateStatus 返回批量掉号闸门快照。
func (p *Pool) BatchDeadGateStatus() BatchDeadGateState {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pruneBatchDeadLocked(now)
	return BatchDeadGateState{
		Tripped:       p.batchDeadTripped,
		TrippedAt:     p.batchDeadTrippedAt,
		RecentCount:   p.distinctBatchDeadLocked(),
		Threshold:     p.batchDeadThreshold,
		WindowSeconds: int(p.batchDeadWindow / time.Second),
	}
}

// ResetBatchDeadGate 手动复位批量掉号闸门并清空滑动窗口事件。
func (p *Pool) ResetBatchDeadGate() {
	p.mu.Lock()
	p.batchDeadTripped = false
	p.batchDeadTrippedAt = time.Time{}
	p.batchDeadTrippedCount = 0
	p.batchDeadEvents = nil
	p.mu.Unlock()
}

// recordBatchDeadLocked 在持 p.mu 下记录一次新增 session_dead，返回是否本次新触发闸门。
func (p *Pool) recordBatchDeadLocked(uid string, now time.Time) (tripped bool, count int, window time.Duration) {
	if p.batchDeadThreshold <= 0 || p.batchDeadWindow <= 0 {
		return false, 0, 0
	}
	p.batchDeadEvents = append(p.batchDeadEvents, batchDeadEvent{at: now, uid: uid})
	p.pruneBatchDeadLocked(now)
	distinct := p.distinctBatchDeadLocked()
	if distinct >= p.batchDeadThreshold && !p.batchDeadTripped {
		p.batchDeadTripped = true
		p.batchDeadTrippedAt = now
		p.batchDeadTrippedCount = distinct
		log.Printf("ERR: [pool] 批量掉号闸门触发：%s 内 %d 个账号连续判 session_dead（阈值 %d），暂停后台定时任务",
			p.batchDeadWindow, distinct, p.batchDeadThreshold)
		return true, distinct, p.batchDeadWindow
	}
	return false, distinct, p.batchDeadWindow
}

func (p *Pool) pruneBatchDeadLocked(now time.Time) {
	if p.batchDeadWindow <= 0 || len(p.batchDeadEvents) == 0 {
		return
	}
	cutoff := now.Add(-p.batchDeadWindow)
	kept := p.batchDeadEvents[:0]
	for _, ev := range p.batchDeadEvents {
		if !ev.at.Before(cutoff) {
			kept = append(kept, ev)
		}
	}
	p.batchDeadEvents = kept
}

func (p *Pool) distinctBatchDeadLocked() int {
	if len(p.batchDeadEvents) == 0 {
		return 0
	}
	seen := make(map[string]struct{}, len(p.batchDeadEvents))
	for _, ev := range p.batchDeadEvents {
		seen[ev.uid] = struct{}{}
	}
	return len(seen)
}

func formatBatchTripReason(count int, window time.Duration) string {
	return fmt.Sprintf("%s 内 %d 个账号判 session_dead", window, count)
}
