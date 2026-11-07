package pool

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestBatchConcurrencyDefaultAndHotSet 批量并发上限：默认 3、热改可读回、越界收敛。
func TestBatchConcurrencyDefaultAndHotSet(t *testing.T) {
	p := New("")
	if got := p.BatchConcurrency(); got != 3 {
		t.Fatalf("默认 BatchConcurrency=%d want 3", got)
	}
	p.SetBatchConcurrency(5)
	if got := p.BatchConcurrency(); got != 5 {
		t.Fatalf("SetBatchConcurrency(5) 后 =%d want 5", got)
	}
	p.SetBatchConcurrency(0)
	if got := p.BatchConcurrency(); got != 3 {
		t.Fatalf("SetBatchConcurrency(0) 后 =%d want 3（回落默认）", got)
	}
	p.SetBatchConcurrency(99)
	if got := p.BatchConcurrency(); got != 8 {
		t.Fatalf("SetBatchConcurrency(99) 后 =%d want 8（钳制）", got)
	}
}

// TestBatchForEachBounded 池的门面必须真的把并发压在上限内（上限 2 / 6 个账号）。
func TestBatchForEachBounded(t *testing.T) {
	p := New("")
	p.SetBatchConcurrency(2)
	var cur, peak atomic.Int32
	p.BatchForEach(6, func(int) {
		n := cur.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		cur.Add(-1)
	})
	if got := peak.Load(); got > 2 {
		t.Fatalf("峰值并发 %d 超过上限 2", got)
	}
}

// TestBatchGateZeroValuePool 零值 Pool（测试/裸构造）也要安全：懒创建闸门并用默认上限。
func TestBatchGateZeroValuePool(t *testing.T) {
	p := &Pool{}
	if got := p.BatchConcurrency(); got != 3 {
		t.Fatalf("零值 Pool BatchConcurrency=%d want 3", got)
	}
	var ran atomic.Int32
	p.BatchForEach(3, func(int) { ran.Add(1) })
	if got := ran.Load(); got != 3 {
		t.Fatalf("BatchForEach 执行 %d 次 want 3", got)
	}
}
