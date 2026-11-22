// channelstat_test.go 反代通道事件计数器：分桶/补齐、日桶折叠、落盘重载、
// 回填种子合并、跨重启元信息（降级上次触发 / 累计学习）、并发与 nil 安全。
package channelstat

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestIncAndSeries(t *testing.T) {
	l := New("")
	base := time.Date(2026, 11, 21, 10, 5, 0, 0, time.Local)
	l.AddAt(base, "channel_refused", 3)
	l.AddAt(base.Add(20*time.Minute), "sys_as_user", 3) // 同一小时桶
	l.AddAt(base.Add(2*time.Hour), "cache_miss", 1)

	from := base.Add(-time.Hour)
	to := base.Add(3 * time.Hour)
	points, totals := l.Series(from, to, false)
	if len(points) != 5 {
		t.Fatalf("points=%d want 5（09:00–13:00 逐小时补齐）", len(points))
	}
	if points[0].Total != 0 || points[1].Kinds["channel_refused"] != 3 || points[1].Kinds["sys_as_user"] != 3 {
		t.Errorf("points[1]=%+v want channel_refused=3 sys_as_user=3", points[1])
	}
	if points[1].Total != 6 {
		t.Errorf("points[1].total=%d want 6", points[1].Total)
	}
	if points[3].Kinds["cache_miss"] != 1 {
		t.Errorf("points[3]=%+v want cache_miss=1（12:00 桶）", points[3])
	}
	if totals.Total != 7 || totals.Kinds["channel_refused"] != 3 {
		t.Errorf("totals=%+v want 7 / channel_refused=3", totals)
	}
	if totals.LastAt["channel_refused"] == "" {
		t.Error("last_at 应记录最近一次时刻")
	}
	if points[0].Scope != "hour" || points[0].T != "2026-11-21T09" {
		t.Errorf("point[0]=%+v want hour/2026-11-21T09", points[0])
	}
}

func TestSeriesDailyBucket(t *testing.T) {
	l := New("")
	d1 := time.Date(2026, 11, 20, 9, 0, 0, 0, time.Local)
	d2 := time.Date(2026, 11, 21, 15, 0, 0, 0, time.Local)
	l.AddAt(d1, "channel_refused", 2)
	l.AddAt(d1.Add(3*time.Hour), "channel_refused", 1)
	l.AddAt(d2, "channel_refused", 5)

	points, totals := l.Series(d1, d2.Add(time.Hour), true)
	if len(points) != 2 {
		t.Fatalf("points=%d want 2（两个自然日）", len(points))
	}
	if points[0].Kinds["channel_refused"] != 3 || points[0].Scope != "day" || points[0].T != "2026-11-20" {
		t.Errorf("points[0]=%+v want 2026-11-20 合并 3 次", points[0])
	}
	if points[1].Kinds["channel_refused"] != 5 {
		t.Errorf("points[1]=%+v want 5", points[1])
	}
	if totals.Total != 8 {
		t.Errorf("totals.total=%d want 8", totals.Total)
	}
}

// 日桶窗口必须包含「当天 00:00 之后的小时桶」——曾经按 21:00 与 00:00 直接比较，
// 把当天数据整片丢掉（看板全是 0）。
func TestSeriesDailyIncludesToday(t *testing.T) {
	l := New("")
	now := time.Now()
	l.AddAt(now, "channel_refused", 1)

	points, totals := l.Series(now.Add(-7*24*time.Hour), now, true)
	if totals.Total != 1 {
		t.Fatalf("totals.total=%d want 1（今天的数据不能被日桶窗口丢掉）", totals.Total)
	}
	last := points[len(points)-1]
	if last.Total != 1 {
		t.Errorf("最后一天=%+v want total=1", last)
	}
}

func TestRollupFoldsOldHourly(t *testing.T) {
	l := New("")
	old := time.Now().Add(-120 * 24 * time.Hour)
	l.AddAt(old, "channel_refused", 2)
	l.AddAt(old.Add(30*time.Minute), "sys_as_user", 1)
	l.mu.Lock()
	l.rollupLocked(time.Now())
	scopes := len(l.buckets)
	l.mu.Unlock()

	if scopes != 1 {
		t.Fatalf("buckets=%d want 1（两个旧小时桶折叠成一天）", scopes)
	}
	points, totals := l.Series(old.Add(-24*time.Hour), old.Add(24*time.Hour), true)
	if totals.Total != 3 || totals.Kinds["channel_refused"] != 2 {
		t.Errorf("折叠后 totals=%+v want 3 / 2", totals)
	}
	if len(points) != 3 {
		t.Errorf("points=%d want 3（-1/+0/+1 天）", len(points))
	}
}

func TestPersistAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "channel-stats.json")
	l := New(path)
	base := time.Date(2026, 11, 21, 10, 0, 0, 0, time.Local)
	l.AddAt(base, "channel_refused", 4)
	l.AddAt(base.Add(time.Hour), "sanitize", 12)
	l.MarkDegrade(base.Add(2 * time.Hour))
	l.AddLearned(7)
	l.Flush()

	l2 := New(path)
	since, lastDegrade, learned := l2.Snapshot()
	if since.IsZero() || !since.Equal(base) {
		t.Errorf("since=%v want %v", since, base)
	}
	if !lastDegrade.Equal(base.Add(2 * time.Hour)) {
		t.Errorf("degrade_last_at=%v want %v", lastDegrade, base.Add(2*time.Hour))
	}
	if learned != 7 {
		t.Errorf("learned_total=%d want 7", learned)
	}
	_, totals := l2.Series(base.Add(-time.Hour), base.Add(3*time.Hour), false)
	if totals.Kinds["channel_refused"] != 4 || totals.Kinds["sanitize"] != 12 {
		t.Errorf("重载后 totals=%+v want 4 / 12", totals.Kinds)
	}
}

// 回填种子：启动时合并一次，合并后改名 .imported，不重复累加。
func TestSeedImport(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "channel-stats.json")
	seed := seedPath(path)
	raw := []byte(`{"version":1,"since":"2026-11-14T00:00:00+08:00","buckets":[` +
		`{"s":"h:2026-11-14T10","k":{"channel_refused":5}},` +
		`{"s":"h:2026-11-15T11","k":{"role_normalized":100}}]}`)
	if err := os.WriteFile(seed, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	l := New(path)
	_, totals := l.Series(time.Date(2026, 11, 14, 0, 0, 0, 0, time.Local),
		time.Date(2026, 11, 16, 0, 0, 0, 0, time.Local), true)
	if totals.Kinds["channel_refused"] != 5 || totals.Kinds["role_normalized"] != 100 {
		t.Fatalf("种子未合并：%+v", totals.Kinds)
	}
	if _, err := os.Stat(seed); !os.IsNotExist(err) {
		t.Error("种子文件应改名，避免下次启动重复合并")
	}
	if _, err := os.Stat(seed + ".imported"); err != nil {
		t.Errorf(".imported 不存在: %v", err)
	}

	// 再启动一次：不重复累加。
	l2 := New(path)
	_, totals = l2.Series(time.Date(2026, 11, 14, 0, 0, 0, 0, time.Local),
		time.Date(2026, 11, 16, 0, 0, 0, 0, time.Local), true)
	if totals.Kinds["channel_refused"] != 5 {
		t.Errorf("重复启动后 channel_refused=%d want 5", totals.Kinds["channel_refused"])
	}
}

func TestSeriesWindowClampAndEmpty(t *testing.T) {
	l := New("")
	points, totals := l.Series(time.Time{}, time.Time{}, false)
	if len(points) == 0 {
		t.Error("空账本也要给出至少一个点（前端画空图）")
	}
	if totals.Total != 0 {
		t.Errorf("空账本 totals=%+v want 0", totals)
	}
	// 90 天以上跨度自动降级日桶。
	now := time.Now()
	points, _ = l.Series(now.Add(-120*24*time.Hour), now, false)
	if len(points) > 200 {
		t.Errorf("120 天窗口 points=%d 应自动切日桶（≤200）", len(points))
	}
	if points[0].Scope != "day" {
		t.Errorf("scope=%q want day", points[0].Scope)
	}
}

func TestConcurrentInc(t *testing.T) {
	l := New("")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				l.Inc("channel_refused")
			}
		}()
	}
	wg.Wait()
	_, totals := l.Series(time.Now().Add(-time.Hour), time.Now().Add(time.Hour), false)
	if totals.Kinds["channel_refused"] != 1000 {
		t.Fatalf("并发自增=%d want 1000", totals.Kinds["channel_refused"])
	}
}

func TestNilLedgerSafe(t *testing.T) {
	var l *Ledger
	l.Inc("x")
	l.IncN("x", 2)
	l.AddAt(time.Now(), "x", 1)
	l.MarkDegrade(time.Now())
	l.AddLearned(1)
	l.Flush()
	l.Stop()
	if _, totals := l.Series(time.Time{}, time.Time{}, false); totals.Total != 0 {
		t.Error("nil 账本应返回空汇总")
	}
	if _, _, n := l.Snapshot(); n != 0 {
		t.Error("nil 账本 Snapshot 应为零值")
	}
}

// Stop 未 Start 必须立即返回：此前 done 永远不会 close，Stop 会永久阻塞。
func TestStopWithoutStart(t *testing.T) {
	l := New("")
	done := make(chan struct{})
	go func() {
		l.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop without Start blocked")
	}
}

// Start 幂等：重复调用只起一个 goroutine，Stop 不 panic（双 close(done)）。
func TestStartTwiceIdempotent(t *testing.T) {
	l := New("")
	l.Start()
	l.Start()
	l.Inc(KindChannelRefused)
	l.Stop()
	l.Stop() // 重复 Stop 也必须安全
	if _, totals := l.Series(time.Now().Add(-time.Hour), time.Now().Add(time.Hour), false); totals.Total != 1 {
		t.Errorf("totals.total=%d want 1", totals.Total)
	}
}

// 未来 from 收敛到当前窗口末尾，不返回一串未来空点。
func TestSeriesFutureWindowClamped(t *testing.T) {
	l := New("")
	now := time.Now()
	l.AddAt(now, KindChannelRefused, 1)

	points, totals := l.Series(now.Add(24*time.Hour), now, false)
	if totals.Total != 1 {
		t.Fatalf("totals.total=%d want 1（窗口应收敛到 now）", totals.Total)
	}
	if len(points) != 1 {
		t.Fatalf("points=%d want 1（未来起点不应展开成未来窗口）", len(points))
	}
	want := truncate(now, false).Format(hourLayout)
	if points[0].T != want {
		t.Errorf("point.t=%q want %q", points[0].T, want)
	}
}

// 磁盘里的 last_at 是 UTC、查询窗口是本地时区：不能因为时区不同把「最近一次」
// 全部过滤掉（生产 6/7 行显示「—」的根因）。
func TestSeriesLastAtNormalizesTimezone(t *testing.T) {
	l := New("")
	// 固定 +08:00 的事件时刻；在 UTC 机器上等价于 02:27 UTC。
	at := time.Date(2026, 11, 22, 10, 27, 0, 0, time.FixedZone("CST", 8*3600))
	l.mu.Lock()
	l.last[string(KindChannelRefused)] = at
	l.mu.Unlock()

	from := time.Date(2026, 11, 22, 0, 0, 0, 0, time.Local)
	to := time.Date(2026, 11, 22, 23, 59, 0, 0, time.Local)
	_, totals := l.Series(from, to, true)
	raw := totals.LastAt[string(KindChannelRefused)]
	if raw == "" {
		t.Fatal("last_at 被时区比较吞掉（B26 回归）")
	}
	got, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("last_at %q 不是 RFC3339: %v", raw, err)
	}
	if !got.Equal(at) {
		t.Errorf("last_at=%v want %v（应按绝对时刻返回）", got, at)
	}
}

// 种子导入不能覆盖主账本元信息：since 取最早、degrade_last_at 取最晚、
// learned_total 累加、last_at 每 kind 取最晚；桶照常累加。
func TestSeedImportPreservesMainMetadata(t *testing.T) {
	cst := time.FixedZone("CST", 8*3600)
	dir := t.TempDir()
	path := filepath.Join(dir, "channel-stats.json")
	mainRaw := `{"version":1,"since":"2026-11-21T10:00:00+08:00",` +
		`"buckets":[{"s":"h:2026-11-21T10","k":{"channel_refused":2}}],` +
		`"last_at":{"channel_refused":"2026-11-21T10:30:00+08:00"},` +
		`"degrade_last_at":"2026-11-21T11:00:00+08:00","learned_total":7}`
	seedRaw := `{"version":1,"since":"2026-11-14T00:00:00+08:00",` +
		`"buckets":[{"s":"h:2026-11-14T10","k":{"channel_refused":5}}],` +
		`"last_at":{"channel_refused":"2026-11-14T10:30:00+08:00","sanitize":"2026-11-14T10:00:00+08:00"},` +
		`"learned_total":3}`
	if err := os.WriteFile(path, []byte(mainRaw), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(seedPath(path), []byte(seedRaw), 0o600); err != nil {
		t.Fatal(err)
	}

	l := New(path)
	since, degrade, learned := l.Snapshot()
	if want := time.Date(2026, 11, 14, 0, 0, 0, 0, cst); !since.Equal(want) {
		t.Errorf("since=%v want %v（种子更早，取最早）", since, want)
	}
	if want := time.Date(2026, 11, 21, 11, 0, 0, 0, cst); !degrade.Equal(want) {
		t.Errorf("degrade_last_at=%v want %v（不能被更旧的种子回滚）", degrade, want)
	}
	if learned != 10 {
		t.Errorf("learned_total=%d want 10（主账本 7 + 种子 3）", learned)
	}

	_, totals := l.Series(
		time.Date(2026, 11, 14, 0, 0, 0, 0, cst),
		time.Date(2026, 11, 22, 0, 0, 0, 0, cst), true)
	if totals.Kinds["channel_refused"] != 7 {
		t.Errorf("channel_refused=%d want 7（桶照常累加）", totals.Kinds["channel_refused"])
	}
	if got, err := time.Parse(time.RFC3339, totals.LastAt["channel_refused"]); err != nil ||
		!got.Equal(time.Date(2026, 11, 21, 10, 30, 0, 0, cst)) {
		t.Errorf("channel_refused last_at=%q want 11-21 10:30（主账本更新，胜出）", totals.LastAt["channel_refused"])
	}
	if got, err := time.Parse(time.RFC3339, totals.LastAt["sanitize"]); err != nil ||
		!got.Equal(time.Date(2026, 11, 14, 10, 0, 0, 0, cst)) {
		t.Errorf("sanitize last_at=%q want 11-14 10:00（来自种子）", totals.LastAt["sanitize"])
	}
}

// 每点 groups：未登记 kind 落 other、offchart 不计、空桶四组零值。
func TestSeriesGroups(t *testing.T) {
	l := New("")
	base := time.Date(2026, 11, 21, 10, 0, 0, 0, time.Local)
	l.AddAt(base, KindChannelRefused, 3)
	l.AddAt(base, KindSoftRate, 1)
	l.AddAt(base, KindStreamError, 2)
	l.AddAt(base, KindRoleNormalized, 10) // offchart
	l.AddAt(base, KindCacheMiss, 4)       // 显式 other
	l.AddAt(base, Kind("weird_kind"), 5)  // 未登记 → other

	points, totals := l.Series(base.Add(-time.Hour), base.Add(time.Hour), false)
	if len(points) != 3 {
		t.Fatalf("points=%d want 3", len(points))
	}
	pt := points[1]
	if pt.Total != 25 {
		t.Errorf("point.total=%d want 25", pt.Total)
	}
	want := map[string]int64{"refusal": 3, "throttle": 1, "stream": 2, "other": 9}
	for k, v := range want {
		if pt.Groups[k] != v {
			t.Errorf("point.groups[%s]=%d want %d (all=%v)", k, pt.Groups[k], v, pt.Groups)
		}
		if totals.Groups[k] != v {
			t.Errorf("totals.groups[%s]=%d want %d", k, totals.Groups[k], v)
		}
	}
	// 空桶：四组零值而不是 nil。
	empty := points[0].Groups
	if len(empty) != 4 {
		t.Fatalf("empty groups=%v want 4 entries", empty)
	}
	for k, v := range empty {
		if v != 0 {
			t.Errorf("empty groups[%s]=%d want 0", k, v)
		}
	}
}
