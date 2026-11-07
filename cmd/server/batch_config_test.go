package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestSaveConfigHotAppliesBatchConcurrency 面板保存 pool.batch_concurrency 后必须
// 立刻写进池闸门（下一轮批量任务生效），且不进「需重启」清单。
func TestSaveConfigHotAppliesBatchConcurrency(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(fp, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := pool.New("")
	live := livecfg.New(livecfg.Snapshot{})
	up := upstream.New()
	sch := scheduler.New(scheduler.Config{})
	if got := p.BatchConcurrency(); got != 3 {
		t.Fatalf("初始 BatchConcurrency=%d want 3", got)
	}

	restart, err := saveConfig([]byte(`{"pool":{"batch_concurrency":5}}`), fp, live, p, up, sch)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.BatchConcurrency(); got != 5 {
		t.Fatalf("保存后 BatchConcurrency=%d want 5（热生效）", got)
	}
	for _, k := range restart {
		if k == "pool.batch_concurrency" {
			t.Fatalf("batch_concurrency 应热生效，不该出现在重启清单：%v", restart)
		}
	}
}
