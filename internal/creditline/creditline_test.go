// creditline_test.go 剩余积分走势采样账本：合计口径（排除企业不限量）、去抖、
// 上限淘汰、落盘重载、窗口读取、并发写入。
package creditline

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// TestSnapshotTotals 合计口径：按 realm 拆分、空 realm 归 cn、企业不限量号只计数。
func TestSnapshotTotals(t *testing.T) {
	p := Snapshot([]pool.Status{
		{UID: "cn1", Realm: "cn", Credits: 100, CreditsTotal: 500},
		{UID: "cn2", Realm: "", Credits: 50, CreditsTotal: 200}, // 空 realm = cn
		{UID: "gl1", Realm: "global", Credits: 30, CreditsTotal: 100},
		{UID: "ent", Realm: "cn", Credits: 999999, CreditsTotal: -1}, // 企业不限量：不计入
	})
	if p.Total != 180 || p.CN != 150 || p.Global != 30 {
		t.Errorf("snapshot=%+v want total 180 / cn 150 / global 30", p)
	}
	if p.Accounts != 3 || p.Unlimited != 1 {
		t.Errorf("accounts=%d unlimited=%d want 3/1", p.Accounts, p.Unlimited)
	}
}

// TestAddDebounce 一分钟内重复采样只保留先到的一个。
func TestAddDebounce(t *testing.T) {
	l := New("", 10)
	base := time.Date(2026, 11, 21, 10, 0, 0, 0, time.Local)
	l.Add(Point{Time: base, Total: 100, CN: 100, Accounts: 1})
	l.Add(Point{Time: base.Add(30 * time.Second), Total: 90, CN: 90, Accounts: 1})
	if l.Len() != 1 {
		t.Fatalf("len=%d want 1（30s 内的第二次采样应被去抖丢弃）", l.Len())
	}
	l.Add(Point{Time: base.Add(61 * time.Second), Total: 80, CN: 80, Accounts: 1})
	if l.Len() != 2 {
		t.Fatalf("len=%d want 2", l.Len())
	}
	last, ok := l.Last()
	if !ok || last.Total != 80 {
		t.Errorf("last=%+v ok=%v want total 80", last, ok)
	}
}

// TestAddCapKeepsNewest 超出上限丢最旧（长期趋势不需要无限历史）。
func TestAddCapKeepsNewest(t *testing.T) {
	l := New("", 3)
	base := time.Date(2026, 11, 21, 10, 0, 0, 0, time.Local)
	for i := 0; i < 5; i++ {
		l.Add(Point{Time: base.Add(time.Duration(i) * 2 * time.Minute), Total: int64(i)})
	}
	if l.Len() != 3 {
		t.Fatalf("len=%d want 3", l.Len())
	}
	pts := l.Read(time.Time{}, time.Time{})
	if len(pts) != 3 || pts[0].Total != 2 || pts[2].Total != 4 {
		t.Errorf("points=%+v want 保留最新的 2/3/4", pts)
	}
}

// TestPersistAndReload 落盘后重新 New 能恢复采样点（含企业不限量计数）。
// 落盘是 30 分钟节流，这里显式 Flush（与优雅停机同路径）。
func TestPersistAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credit-trend.json")
	l := New(path, 10)
	base := time.Date(2026, 11, 21, 10, 0, 0, 0, time.Local)
	l.Add(Point{Time: base, Total: 300, CN: 200, Global: 100, Accounts: 2, Unlimited: 1})
	l.Add(Point{Time: base.Add(5 * time.Minute), Total: 250, CN: 150, Global: 100, Accounts: 2})
	l.Flush()

	l2 := New(path, 10)
	if l2.Len() != 2 {
		t.Fatalf("reloaded len=%d want 2", l2.Len())
	}
	last, _ := l2.Last()
	if last.Total != 250 || last.Global != 100 || last.Time.IsZero() {
		t.Errorf("reloaded last=%+v want total 250 global 100", last)
	}
}

// TestFlushThrottle 落盘节流：窗口内的新点留在内存，Flush 后一次性落盘。
// （每次采样都整体重写 1 MB 级文件对 SSD 是无谓写入，见 flushEvery。）
func TestFlushThrottle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credit-trend.json")
	l := New(path, 10)
	base := time.Date(2026, 11, 21, 10, 0, 0, 0, time.Local)
	l.Add(Point{Time: base, Total: 100, CN: 100, Accounts: 1})
	// 第一个点 lastWrite 为零值 → 立即落盘。
	if got := New(path, 10).Len(); got != 1 {
		t.Fatalf("首点应立即落盘，reloaded len=%d", got)
	}
	// 第二个点在 30 分钟窗口内 → 只进内存。
	l.Add(Point{Time: base.Add(5 * time.Minute), Total: 90, CN: 90, Accounts: 1})
	if got := New(path, 10).Len(); got != 1 {
		t.Fatalf("窗口内的点不应落盘，reloaded len=%d want 1", got)
	}
	if l.Len() != 2 {
		t.Fatalf("内存 len=%d want 2", l.Len())
	}
	l.Flush()
	if got := New(path, 10).Len(); got != 2 {
		t.Fatalf("Flush 后 reloaded len=%d want 2", got)
	}
}

// TestReadWindow 显式区间（闭区间）与零值侧不设界。
func TestReadWindow(t *testing.T) {
	l := New("", 10)
	base := time.Date(2026, 11, 21, 10, 0, 0, 0, time.Local)
	for i := 0; i < 4; i++ {
		l.Add(Point{Time: base.Add(time.Duration(i) * 10 * time.Minute), Total: int64(i)})
	}
	if got := l.Read(base.Add(5*time.Minute), base.Add(25*time.Minute)); len(got) != 2 || got[0].Total != 1 || got[1].Total != 2 {
		t.Errorf("window=%+v want 10:10/10:20 两点", got)
	}
	if got := l.Read(time.Time{}, base.Add(15*time.Minute)); len(got) != 2 {
		t.Errorf("from 零值 = 不设下界，want 2 got %d", len(got))
	}
	if got := l.Read(base.Add(15*time.Minute), time.Time{}); len(got) != 2 {
		t.Errorf("to 零值 = 不设上界，want 2 got %d", len(got))
	}
}

// TestConcurrentAdd 并发采样（余额刷新与签到撞车）不得 panic / 竞态。
// 去抖是「相对上一个点」的判定，并发乱序时间戳本来就会被吞掉一部分，故只断言
// 条数在合理区间；真正的检查是 go test -race 下的数据竞争检测。
func TestConcurrentAdd(t *testing.T) {
	l := New("", 1000)
	base := time.Date(2026, 11, 21, 10, 0, 0, 0, time.Local)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := 0; n < 5; n++ {
				l.Add(Point{Time: base.Add(time.Duration(i*10+n) * 2 * time.Minute), Total: int64(i*10 + n)})
			}
		}(i)
	}
	wg.Wait()
	if n := l.Len(); n < 1 || n > 40 {
		t.Errorf("len=%d want 1..40", n)
	}
}

// TestNilLedgerSafe 零值/nil 账本不得 panic（面板/测试可能裸用）。
func TestNilLedgerSafe(t *testing.T) {
	var l *Ledger
	l.Add(Point{Total: 1})
	if l.Len() != 0 {
		t.Errorf("nil len=%d want 0", l.Len())
	}
	if _, ok := l.Last(); ok {
		t.Error("nil last ok 应为 false")
	}
	if got := l.Read(time.Time{}, time.Time{}); len(got) != 0 {
		t.Errorf("nil read=%v want 空", got)
	}
}

// TestLoadCorruptFileFallsBackEmpty 损坏文件 → 从空账本开始（丢历史好过起不来）。
func TestLoadCorruptFileFallsBackEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credit-trend.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := New(path, 10)
	if l.Len() != 0 {
		t.Errorf("损坏文件 len=%d want 0", l.Len())
	}
	// 之后仍能正常采样。
	l.Add(Point{Time: time.Now(), Total: 1, Accounts: 1})
	if l.Len() != 1 {
		t.Errorf("len=%d want 1", l.Len())
	}
}

// TestSnapshotEdgeCases 空池 / 纯不限量池：不产生假的合计。
func TestSnapshotEdgeCases(t *testing.T) {
	if p := Snapshot(nil); p.Accounts != 0 || p.Total != 0 {
		t.Errorf("空池 snapshot=%+v want 全零", p)
	}
	if p := Snapshot([]pool.Status{{UID: "x", CreditsTotal: -1}}); p.Accounts != 0 || p.Unlimited != 1 {
		t.Errorf("纯不限量池 snapshot=%+v want accounts 0 / unlimited 1", p)
	}
}
