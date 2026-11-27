package pool

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestBatchDeadGateTripsOnDistinctAccounts(t *testing.T) {
	p := New("")
	for _, uid := range []string{"u1", "u2", "u3"} {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1})
	}

	var trippedCount atomic.Int32
	var lossEvents []string
	p.SetBatchDeadGate(3, 10*time.Minute)
	p.SetBatchDeadTripHook(func(count int, window time.Duration) {
		trippedCount.Add(int32(count))
	})
	p.SetLossObserver(func(ev LossEvent) {
		lossEvents = append(lossEvents, ev.Event+":"+ev.UID)
	})

	// 每个号需达到 sessionDeadThreshold (3) 才判 session_dead。
	for _, uid := range []string{"u1", "u2"} {
		for i := 0; i < sessionDeadThreshold; i++ {
			p.NoteSessionDead(uid)
		}
	}
	if st := p.BatchDeadGateStatus(); st.Tripped || st.RecentCount != 2 {
		t.Fatalf("2 个号不应触发闸门: %+v", st)
	}

	// 第 3 个不同账号判 session_dead → 触发闸门。
	for i := 0; i < sessionDeadThreshold; i++ {
		p.NoteSessionDead("u3")
	}
	st := p.BatchDeadGateStatus()
	if !st.Tripped || st.RecentCount != 3 || st.Threshold != 3 {
		t.Fatalf("第 3 个号应触发闸门: %+v", st)
	}
	if got := trippedCount.Load(); got != 3 {
		t.Errorf("trip hook count=%d want 3", got)
	}
	if len(lossEvents) != 4 {
		t.Errorf("lossEvents=%v want 4 条（3 session_dead + 1 batch_gate_tripped）", lossEvents)
	}

	// 复位后恢复未触发状态。
	p.ResetBatchDeadGate()
	st2 := p.BatchDeadGateStatus()
	if st2.Tripped || st2.RecentCount != 0 {
		t.Errorf("复位后应清零: %+v", st2)
	}
}
