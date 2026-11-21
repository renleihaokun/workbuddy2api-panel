// aggregate_test.go 归档窗口聚合（面板「反代劣化」看板的数据源）：
// 按时间桶计数、失败分类、分域、回退/降级计数、窗口过滤、归档关闭/空目录。
package reqlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeArchive 直接落一个归档 JSONL（不走异步 writer，测试结果确定）。
func writeArchive(t *testing.T, dir, name string, events []Event) {
	t.Helper()
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, e := range events {
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(append(raw, '\n')); err != nil {
			t.Fatal(err)
		}
	}
}

// TestAggregateBucketsKindsRealms 一窗四点：两个时间桶、两类失败、两个域、
// 回退与降级计数、成功率与平均耗时。
func TestAggregateBucketsKindsRealms(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 11, 21, 10, 15, 0, 0, time.Local)
	events := []Event{
		{Time: base, Outcome: OutcomeSuccess, Realm: "cn", DurationMs: 100},
		{Time: base.Add(5 * time.Minute), Outcome: OutcomeHTTPError, ErrKind: "channel_refused", Realm: "cn", DurationMs: 200},
		{Time: base.Add(70 * time.Minute), Outcome: OutcomeStreamError, ErrKind: "soft_rate", Realm: "global",
			Fallback: true, Degraded: true, DurationMs: 0},
		{Time: base.Add(80 * time.Minute), Outcome: OutcomeSuccess, Realm: "global", Fallback: true},
	}
	writeArchive(t, dir, "requests-2026-11-21.jsonl", events)

	r := New(Config{Dir: dir, Enabled: true})
	defer r.Close()

	agg, err := r.Aggregate(time.Time{}, time.Time{}, time.Hour)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if agg.Bucket != "hour" {
		t.Errorf("bucket=%q want hour", agg.Bucket)
	}
	if len(agg.Buckets) != 2 {
		t.Fatalf("buckets=%d want 2（10 点桶 + 11 点桶）: %+v", len(agg.Buckets), agg.Buckets)
	}
	if agg.Buckets[0].T != "2026-11-21T10" || agg.Buckets[1].T != "2026-11-21T11" {
		t.Errorf("bucket keys=%q/%q want 10/11 点桶", agg.Buckets[0].T, agg.Buckets[1].T)
	}
	if agg.Buckets[0].Requests != 2 || agg.Buckets[0].Success != 1 || agg.Buckets[0].HTTPError != 1 {
		t.Errorf("bucket[0]=%+v want requests 2 / success 1 / http_error 1", agg.Buckets[0])
	}
	if agg.Buckets[0].Kinds["channel_refused"] != 1 {
		t.Errorf("bucket[0].kinds=%v want channel_refused=1", agg.Buckets[0].Kinds)
	}
	if agg.Buckets[1].Kinds["soft_rate"] != 1 || agg.Buckets[1].StreamError != 1 {
		t.Errorf("bucket[1]=%+v want soft_rate=1 + stream_error=1", agg.Buckets[1])
	}

	tot := agg.Totals
	if tot.Requests != 4 || tot.Success != 2 || tot.HTTPError != 1 || tot.StreamError != 1 {
		t.Errorf("totals=%+v want 4/2/1/1", tot)
	}
	if tot.SuccessRate != 50 {
		t.Errorf("success_rate=%v want 50", tot.SuccessRate)
	}
	if tot.AvgDurationMs != 75 { // (100+200+0+0)/4
		t.Errorf("avg_duration_ms=%v want 75", tot.AvgDurationMs)
	}
	if tot.Fallbacks != 2 || tot.Degraded != 1 {
		t.Errorf("fallbacks=%d degraded=%d want 2/1", tot.Fallbacks, tot.Degraded)
	}

	// 失败分类按次数降序、同次数按名字升序；成功不进分类表。
	if len(agg.Kinds) != 2 {
		t.Fatalf("kinds=%+v want 2（只有失败分类）", agg.Kinds)
	}
	if agg.Kinds[0].Kind != "channel_refused" || agg.Kinds[1].Kind != "soft_rate" {
		t.Errorf("kinds order=%v/%v want channel_refused,soft_rate", agg.Kinds[0].Kind, agg.Kinds[1].Kind)
	}
	if agg.Kinds[0].LastAt.IsZero() {
		t.Error("kind last_at 不得为零值（面板表格要显示最近一次）")
	}

	// 分域：cn 2 请求 1 成功；global 2 请求 1 成功 1 流错误。
	cn := agg.Realms["cn"]
	if cn.Requests != 2 || cn.Success != 1 {
		t.Errorf("realm cn=%+v want 2/1", cn)
	}
	gl := agg.Realms["global"]
	if gl.Requests != 2 || gl.Success != 1 || gl.StreamError != 1 || gl.Fallbacks != 2 {
		t.Errorf("realm global=%+v want 2/1/1 + fallbacks 2", gl)
	}
}

// TestAggregateWindowFilter 显式区间只统计落在区间内的事件（闭区间）。
func TestAggregateWindowFilter(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 11, 21, 10, 0, 0, 0, time.Local)
	writeArchive(t, dir, "requests-2026-11-21.jsonl", []Event{
		{Time: base, Outcome: OutcomeSuccess, Realm: "cn"},
		{Time: base.Add(30 * time.Minute), Outcome: OutcomeHTTPError, ErrKind: "server", Realm: "cn"},
		{Time: base.Add(2 * time.Hour), Outcome: OutcomeSuccess, Realm: "global"},
	})
	r := New(Config{Dir: dir, Enabled: true})
	defer r.Close()

	agg, err := r.Aggregate(base.Add(20*time.Minute), base.Add(40*time.Minute), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if agg.Totals.Requests != 1 || agg.Totals.HTTPError != 1 {
		t.Fatalf("totals=%+v want 只统计 10:30 那条", agg.Totals)
	}
	if len(agg.Kinds) != 1 || agg.Kinds[0].Kind != "server" {
		t.Errorf("kinds=%+v want server", agg.Kinds)
	}
}

// TestAggregateUnknownKind 旧归档（无 err_kind）的失败归入 unknown；成功不进分类表。
func TestAggregateUnknownKind(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 11, 21, 9, 0, 0, 0, time.Local)
	writeArchive(t, dir, "requests-2026-11-21.jsonl", []Event{
		{Time: base, Outcome: OutcomeHTTPError},                     // 旧字段缺失
		{Time: base.Add(time.Minute), Outcome: OutcomeSuccess},      // 成功
		{Time: base.Add(2 * time.Minute), Outcome: OutcomeInterrupted}, // 断连也不算失败分类
	})
	r := New(Config{Dir: dir, Enabled: true})
	defer r.Close()

	agg, err := r.Aggregate(time.Time{}, time.Time{}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(agg.Kinds) != 1 || agg.Kinds[0].Kind != "unknown" || agg.Kinds[0].Count != 1 {
		t.Errorf("kinds=%+v want 只有 unknown×1", agg.Kinds)
	}
	if agg.Totals.Interrupted != 1 {
		t.Errorf("interrupted=%d want 1", agg.Totals.Interrupted)
	}
	// 无 realm 字段的事件归入 unknown 域，不与 cn/global 混算。
	if agg.Realms["unknown"].Requests != 3 {
		t.Errorf("realm unknown=%+v want 3 请求", agg.Realms["unknown"])
	}
}

// TestAggregateDayBucket 桶宽 >= 24h 时按自然日聚合（scope=day，键为 2006-01-02）。
func TestAggregateDayBucket(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 11, 20, 23, 0, 0, 0, time.Local)
	writeArchive(t, dir, "requests-2026-11-20.jsonl", []Event{
		{Time: base, Outcome: OutcomeSuccess},
		{Time: base.Add(2 * time.Hour), Outcome: OutcomeSuccess}, // 次日 01:00
	})
	r := New(Config{Dir: dir, Enabled: true})
	defer r.Close()

	agg, err := r.Aggregate(time.Time{}, time.Time{}, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if agg.Bucket != "day" {
		t.Errorf("bucket=%q want day", agg.Bucket)
	}
	if len(agg.Buckets) != 2 || agg.Buckets[0].T != "2026-11-20" || agg.Buckets[1].T != "2026-11-21" {
		t.Errorf("buckets=%+v want 两个自然日桶", agg.Buckets)
	}
	if agg.Buckets[0].Scope != "day" {
		t.Errorf("scope=%q want day", agg.Buckets[0].Scope)
	}
}

// TestAggregateArchiveDisabled 归档关闭/空目录：返回空结果 + Archive.Enabled=false，
// 不报错（面板据此显示「归档已关闭」而不是一片 0）。
func TestAggregateArchiveDisabled(t *testing.T) {
	r := New(Config{Dir: t.TempDir(), Enabled: false})
	defer r.Close()
	agg, err := r.Aggregate(time.Time{}, time.Time{}, time.Hour)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if agg.Archive.Enabled {
		t.Error("archive.enabled 应为 false")
	}
	if agg.Totals.Requests != 0 || len(agg.Buckets) != 0 || len(agg.Kinds) != 0 {
		t.Errorf("关闭归档应返回空结果: %+v", agg)
	}

	// nil recorder 不得 panic。
	var nilRec *Recorder
	if _, err := nilRec.Aggregate(time.Time{}, time.Time{}, time.Hour); err != nil {
		t.Errorf("nil recorder: %v", err)
	}
}
