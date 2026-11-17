package pricing

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// approx 浮点比较（金额只到分，1e-9 足够）。
func approx(t *testing.T, got, want float64, what string) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

// 分段计价：命中×缓存价 + 未命中×输入价 + 输出×输出价。
// glm-5.3 = 8 / 28 / 2 元，PT=1e6（命中 5e5、未命中 5e5），CT=1e5
// → (5e5×2 + 5e5×8 + 1e5×28)/1e6 = 7.8 元。
func TestCostSplitsCacheHitAndMiss(t *testing.T) {
	tbl := Seed()
	at := time.Date(2026, 11, 17, 10, 0, 0, 0, beijing)
	got, ok := tbl.Cost("cn", "glm-5.3", at, true, 1_000_000, 100_000, 500_000, 500_000)
	if !ok {
		t.Fatal("glm-5.3 应在种子牌价表里")
	}
	approx(t, got, 7.8, "glm-5.3 cost")
}

// 上游没回缓存维度（CH=CM=0）时整段按未命中价：偏高但可解释。
func TestCostWithoutCacheObservation(t *testing.T) {
	tbl := Seed()
	at := time.Date(2026, 11, 17, 10, 0, 0, 0, beijing)
	got, _ := tbl.Cost("cn", "glm-5.3", at, true, 1_000_000, 0, 0, 0)
	approx(t, got, 8, "no-cache-observation cost")
}

// 只有命中观测、没有未命中观测：差额按未命中补齐。
func TestCostOnlyHitObservation(t *testing.T) {
	tbl := Seed()
	at := time.Date(2026, 11, 17, 10, 0, 0, 0, beijing)
	got, _ := tbl.Cost("cn", "glm-5.3", at, true, 1_000_000, 0, 400_000, 0)
	// 4e5×2 + 6e5×8 = 5.6
	approx(t, got, 5.6, "hit-only cost")
}

// 上游口径打架（命中+未命中 > prompt）时钳制到 prompt，不重复计费。
func TestCostClampsOverflow(t *testing.T) {
	tbl := Seed()
	at := time.Date(2026, 11, 17, 10, 0, 0, 0, beijing)
	got, _ := tbl.Cost("cn", "glm-5.3", at, true, 1_000_000, 0, 900_000, 900_000)
	// 命中 9e5×2 + 未命中 1e5×8 = 1.8 + 0.8 = 2.6
	approx(t, got, 2.6, "clamped cost")
}

// cache_hit 显式为 0 = 缓存命中免费；指针为 nil = 无缓存档位（按输入价）。
func TestCacheHitPointerSemantics(t *testing.T) {
	free := 0.0
	tbl := NewTable(map[string]Price{
		"free-cache": {Input: 10, Output: 20, CacheHit: &free},
		"no-cache":   {Input: 10, Output: 20},
	}, Meta{})
	at := time.Date(2026, 11, 17, 10, 0, 0, 0, beijing)

	got, ok := tbl.Cost("cn", "free-cache", at, true, 1_000_000, 0, 1_000_000, 0)
	if !ok {
		t.Fatal("free-cache 未命中")
	}
	approx(t, got, 0, "cache 免费")

	got, _ = tbl.Cost("cn", "no-cache", at, true, 1_000_000, 0, 1_000_000, 0)
	approx(t, got, 10, "无缓存档位按输入价")
}

// realm 覆盖优先于裸名；裸名对两域生效。
func TestRealmOverrideWins(t *testing.T) {
	tbl := NewTable(map[string]Price{
		"global:glm-5.3": {Input: 100, Output: 200},
	}, Meta{})

	if p, ok := tbl.Lookup("global", "glm-5.3"); !ok || p.Input != 100 {
		t.Fatalf("global 覆盖未生效: %+v ok=%v", p, ok)
	}
	if p, ok := tbl.Lookup("cn", "glm-5.3"); !ok || p.Input != 8 {
		t.Fatalf("cn 应回落裸名种子价: %+v ok=%v", p, ok)
	}
	if _, ok := tbl.Lookup("cn", "not-a-model"); ok {
		t.Fatal("未知模型不应命中")
	}
}

// DeepSeek 峰谷：工作日高峰时段全价、午休/晚上半价、周末全天半价。
func TestPeakFactorHourly(t *testing.T) {
	tbl := Seed()
	cases := []struct {
		name string
		at   time.Time
		want float64
	}{
		{"工作日 10 点（高峰）", time.Date(2026, 11, 17, 10, 0, 0, 0, beijing), 9},
		{"工作日 12 点（高峰已结束）", time.Date(2026, 11, 17, 12, 0, 0, 0, beijing), 4.5},
		{"工作日 13 点（午休）", time.Date(2026, 11, 17, 13, 0, 0, 0, beijing), 4.5},
		{"工作日 15 点（高峰）", time.Date(2026, 11, 17, 15, 0, 0, 0, beijing), 9},
		{"工作日 18 点（下班）", time.Date(2026, 11, 17, 18, 0, 0, 0, beijing), 4.5},
		{"周六 10 点（全天半价）", time.Date(2026, 11, 21, 10, 0, 0, 0, beijing), 4.5},
	}
	for _, c := range cases {
		got, ok := tbl.Cost("cn", "deepseek-v4-pro", c.at, true, 1_000_000, 0, 0, 0)
		if !ok {
			t.Fatalf("%s: deepseek-v4-pro 不在种子表", c.name)
		}
		approx(t, got, c.want, c.name)
	}
}

// 日桶（90 天前折叠）按当天高峰小时数加权：工作日 9–12、14–18 共 7 小时。
func TestPeakFactorDailyWeighted(t *testing.T) {
	tbl := Seed()
	weekday := time.Date(2026, 11, 17, 0, 0, 0, 0, beijing)
	weekend := time.Date(2026, 11, 21, 0, 0, 0, 0, beijing)

	got, _ := tbl.Cost("cn", "deepseek-v4-pro", weekday, false, 1_000_000, 0, 0, 0)
	approx(t, got, 9*(7+17*0.5)/24, "工作日日桶加权价")

	got, _ = tbl.Cost("cn", "deepseek-v4-pro", weekend, false, 1_000_000, 0, 0, 0)
	approx(t, got, 4.5, "周末日桶半价")
}

// 法定节假日：配置了日期就按空闲价，不配则按工作日算（偏高，界面已说明）。
func TestPeakHolidayOverride(t *testing.T) {
	tbl := NewTable(map[string]Price{
		"peak-with-holiday": {
			Input: 10, Output: 20,
			Peak: &Peak{Factor: 0.5, Hours: [][2]int{{9, 12}, {14, 18}}, WeekdaysOnly: true, Holidays: []string{"2026-11-17"}},
		},
	}, Meta{})
	at := time.Date(2026, 11, 17, 10, 0, 0, 0, beijing) // 工作日高峰时段，但被标成节假日
	got, _ := tbl.Cost("cn", "peak-with-holiday", at, true, 1_000_000, 0, 0, 0)
	approx(t, got, 5, "节假日全天半价")
}

// 文件解析：非法行剔除并给告警，合法行生效；币种/单位不符整份拒绝。
func TestParseFileValidation(t *testing.T) {
	raw := []byte(`{
  "version": 1,
  "updated": "2026-11-17",
  "currency": "CNY",
  "unit": "per_1m_tokens",
  "models": {
    "glm-5.3": {"input": 9, "output": 30, "cache_hit": 2.5},
    "cn:glm-5.3-flash": {"input": 1, "output": 3},
    "broken-neg": {"input": -1, "output": 3},
    "broken-hours": {"input": 1, "output": 3, "peak": {"factor": 0.5, "hours": [[12, 9]]}}
  }
}`)
	rows, updated, warns, err := ParseFile(raw)
	if err != nil {
		t.Fatalf("合法文件解析失败: %v", err)
	}
	if updated != "2026-11-17" {
		t.Fatalf("updated = %q", updated)
	}
	if len(rows) != 2 {
		t.Fatalf("有效行 = %d 条, want 2（其余剔除）", len(rows))
	}
	if len(warns) != 2 {
		t.Fatalf("告警 = %d 条, want 2：%v", len(warns), warns)
	}
	if _, ok := rows["glm-5.3"]; !ok {
		t.Fatal("glm-5.3 覆盖行丢失")
	}
	if _, ok := rows["cn:glm-5.3-flash"]; !ok {
		t.Fatal("带 realm 前缀的行应归一化为 cn:glm-5.3-flash")
	}
	for _, bad := range []string{`{"currency":"USD","models":{}}`, `{"unit":"per_1k_tokens","models":{}}`, `not json`} {
		if _, _, _, err := ParseFile([]byte(bad)); err == nil {
			t.Fatalf("应拒绝: %s", bad)
		}
	}
}

// Store：文件改动即时重载；坏文件保留上一版并透出错误；文件缺失回落种子。
func TestStoreReloadAndFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prices.json")
	if err := os.WriteFile(path, []byte(`{"currency":"CNY","unit":"per_1m_tokens","updated":"2026-11-17","models":{"glm-5.3":{"input":9,"output":30,"cache_hit":2.5}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewStore(path)
	if p, ok := s.Current().Lookup("cn", "glm-5.3"); !ok || p.Input != 9 {
		t.Fatalf("文件覆盖未生效: %+v ok=%v", p, ok)
	}
	if m := s.Meta(); m.Source != "seed+file" || m.Updated != "2026-11-17" {
		t.Fatalf("meta = %+v", m)
	}

	// 改价（内容长度也变，避免同尺寸同 mtime 的极端情况）
	if err := os.WriteFile(path, []byte(`{"currency":"CNY","unit":"per_1m_tokens","updated":"2026-11-18","models":{"glm-5.3":{"input":12.5,"output":30,"cache_hit":2.5}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Current().Lookup("cn", "glm-5.3"); p.Input != 12.5 {
		t.Fatalf("热重载未生效: input=%v", p.Input)
	}

	// 坏文件：沿用上一版，Meta().Error 非空
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	cur := s.Current()
	if p, _ := cur.Lookup("cn", "glm-5.3"); p.Input != 12.5 {
		t.Fatalf("坏文件不应冲掉上一版: input=%v", p.Input)
	}
	if cur.Meta().Error == "" {
		t.Fatal("坏文件应透出 Meta().Error")
	}

	// 删文件：回种子（glm-5.3 = 8）
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Current().Lookup("cn", "glm-5.3"); p.Input != 8 {
		t.Fatalf("删文件后应回落种子: input=%v", p.Input)
	}
}

// 种子表自检：所有条目合法、来源非空、峰谷模型的时段在范围内。
func TestSeedTableSane(t *testing.T) {
	tbl := Seed()
	m := tbl.Meta()
	if !m.Enabled || m.Currency != CurrencyCNY || m.Unit != UnitPer1M {
		t.Fatalf("种子 meta = %+v", m)
	}
	if m.Models != len(seedPrices) {
		t.Fatalf("models = %d, want %d", m.Models, len(seedPrices))
	}
	for name, p := range seedPrices {
		if err := validatePrice(p); err != nil {
			t.Fatalf("%s 非法: %v", name, err)
		}
		if p.Source == "" {
			t.Fatalf("%s 缺 source（出处必须可追溯）", name)
		}
	}
	if len(m.PeakModels) != 3 {
		t.Fatalf("峰谷模型 = %v, want 3 个 deepseek 系列", m.PeakModels)
	}
}

// 仓库根目录的 prices.example.json 必须能被当前解析器原样吃下（防止示例
// 与实现漂移：示例里写了新字段、实现没认，用户复制过去就静默失效）。
func TestExampleFileParses(t *testing.T) {
	raw, err := os.ReadFile("../../prices.example.json")
	if err != nil {
		t.Fatalf("读取 prices.example.json 失败: %v", err)
	}
	rows, updated, warns, err := ParseFile(raw)
	if err != nil {
		t.Fatalf("示例文件解析失败: %v", err)
	}
	if len(warns) != 0 {
		t.Fatalf("示例文件不该有告警: %v", warns)
	}
	if updated == "" {
		t.Fatal("示例文件缺 updated 快照日期")
	}
	if len(rows) < 9 {
		t.Fatalf("示例文件模型行 = %d, want >= 9", len(rows))
	}
	// 示例文件里 deepseek 系列必须带峰谷规则（这条是用户最可能踩的坑）
	if p, ok := rows["deepseek-v4-pro"]; !ok || p.Peak == nil {
		t.Fatal("示例文件缺 deepseek-v4-pro 的峰谷配置")
	}
}

// 计算器缓存不能改变结果（对比逐次直算）。
func TestCalculatorMatchesTable(t *testing.T) {
	tbl := Seed()
	calc := tbl.NewCalculator()
	at := time.Date(2026, 11, 17, 10, 0, 0, 0, beijing)
	for i := 0; i < 3; i++ {
		got, ok := calc.Cost("cn", "deepseek-v4-pro", at, true, 1_000_000, 1000, 200, 800)
		want, _ := tbl.Cost("cn", "deepseek-v4-pro", at, true, 1_000_000, 1000, 200, 800)
		if !ok {
			t.Fatal("计算器未命中")
		}
		approx(t, got, want, "calculator cost")
	}
	if _, ok := calc.Cost("cn", "nope", at, true, 1, 0, 0, 0); ok {
		t.Fatal("未知模型应返回 ok=false")
	}
}
