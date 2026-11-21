// dashboard_frontend_test.go 两个新看板的前端渲染切片测试（node + vm）：
// 积分走势折线（三条线 / 面积 / 圆点 tooltip / 单点与空态）与反代劣化的
// 100% 堆叠柱（系列分组 / 类名 / 空态）。沿用 frontend_test.go 的切片手法，
// 只截取纯渲染函数，不依赖真实 DOM。
package panel

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestAppJSCreditTrendChart 积分走势：三条折线 + 合计面积 + 每点 tooltip；
// 单点不画折线（只有圆点）；空数据走空态。
func TestAppJSCreditTrendChart(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; credit trend chart test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const chartStart = src.indexOf('function parsePointTime');
const chartEnd = src.indexOf('function fmtTokTip');
const escStart = src.indexOf('function esc(');
const escEnd = src.indexOf('function ago(');
const fmtStart = src.indexOf('function fmtTok(');
const fmtEnd = src.indexOf('function usStat(');
const stampStart = src.indexOf('function fmtStampShort');
const stampEnd = src.indexOf('function dtLocalValue');
const trendStart = src.indexOf('function renderCreditTrend');
const trendEnd = src.indexOf('async function loadCreditTrend');
if ([chartStart, chartEnd, escStart, escEnd, fmtStart, fmtEnd, stampStart, stampEnd, trendStart, trendEnd].some(v => v < 0)) {
  throw new Error('credit trend helpers not found');
}
const hosts = {};
const doc = { getElementById: id => (hosts[id] = hosts[id] || { innerHTML: '', textContent: '', title: '' }) };
const ctx = { Date, Number, String, Math, RegExp, isNaN, Set, Array, Object, Infinity, document: doc, $: doc.getElementById };
vm.createContext(ctx);
vm.runInContext(
  src.slice(escStart, escEnd) + src.slice(fmtStart, fmtEnd) + src.slice(stampStart, stampEnd) +
  src.slice(chartStart, chartEnd) + src.slice(trendStart, trendEnd) +
  '\nthis.renderCreditTrend=renderCreditTrend;', ctx);

const points = [
  { t: '2026-11-21T10', scope: 'hour', total: 900, cn: 700, global: 200, accounts: 2 },
  { t: '2026-11-21T11', scope: 'hour', total: 850, cn: 650, global: 200, accounts: 2 },
  { t: '2026-11-21T12', scope: 'hour', total: 800, cn: 600, global: 200, accounts: 2 },
];
ctx.renderCreditTrend({ bucket: 'hour', points, current: { total: 780, cn: 580, global: 200, accounts: 2 }, last_sample_at: '2026-11-21T12:00:00Z' });
const svg = hosts.ctChart.innerHTML;
const note = hosts.ctChartNote.textContent;
const headline = hosts.ctNote.textContent;

ctx.renderCreditTrend({ bucket: 'hour', points: [{ t: '2026-11-21T10', scope: 'hour', total: 5, cn: 5, global: 0 }], current: {} });
const single = hosts.ctChart.innerHTML;

ctx.renderCreditTrend({ points: [], current: {}, refresh_sec: 300 });
const empty = hosts.ctChart.innerHTML;

process.stdout.write(JSON.stringify({
  total: (svg.match(/ctl-total/g) || []).length,
  cn: (svg.match(/ctl-cn/g) || []).length,
  gl: (svg.match(/ctl-gl/g) || []).length,
  area: (svg.match(/class="ctarea"/g) || []).length,
  dots: (svg.match(/class="ctdot"/g) || []).length,
  titles: (svg.match(/<title>/g) || []).length,
  hasGrid: svg.includes('class="gl"'),
  noteHasPoints: note.includes('3 个采样点'),
  headlineHasCurrent: headline.includes('当前合计'),
  singleNoLine: !single.includes('ctl-total') && (single.match(/class="ctdot"/g) || []).length === 1,
  emptyState: empty.includes('us-empty') && empty.includes('余额后台刷新'),
}));`
	f, err := os.CreateTemp(t.TempDir(), "credit-trend-chart-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("credit trend chart node test failed: %v\n%s", err, out)
	}
	want := `{"total":1,"cn":1,"gl":1,"area":1,"dots":3,"titles":3,"hasGrid":true,` +
		`"noteHasPoints":true,"headlineHasCurrent":true,"singleNoLine":true,"emptyState":true}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("credit trend chart=%s\nwant %s", out, want)
	}
}

// TestAppJSChannelChart 反代劣化事件堆叠柱：按系列分组（通道拒绝/搬移/脱敏/
// 限流/流中断/其他），柱子类名 dbar，空数据与全零都走空态。
func TestAppJSChannelChart(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; channel chart test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const chartStart = src.indexOf('function parsePointTime');
const chartEnd = src.indexOf('function fmtTokTip');
const escStart = src.indexOf('function esc(');
const escEnd = src.indexOf('function ago(');
const fmtStart = src.indexOf('function fmtTok(');
const fmtEnd = src.indexOf('function usStat(');
const seriesStart = src.indexOf('const CH_EVENT_SERIES');
const drawStart = src.indexOf('function renderChannelEventChart');
const drawEnd = src.indexOf('function renderChannelRateChart');
if ([chartStart, chartEnd, escStart, escEnd, fmtStart, fmtEnd, seriesStart, drawStart, drawEnd].some(v => v < 0)) {
  throw new Error('channel chart helpers not found');
}
const hosts = {};
const doc = { getElementById: id => (hosts[id] = hosts[id] || { innerHTML: '', textContent: '', title: '' }) };
const ctx = { Date, Number, String, Math, RegExp, isNaN, Set, Array, Object, Infinity, document: doc, $: doc.getElementById };
vm.createContext(ctx);
vm.runInContext(
  src.slice(escStart, escEnd) + src.slice(fmtStart, fmtEnd) + src.slice(chartStart, chartEnd) +
  src.slice(seriesStart, drawStart) + src.slice(drawStart, drawEnd) +
  '\nthis.renderChannelEventChart=renderChannelEventChart; this.chSeriesCount=chSeriesCount;', ctx);

const points = [
  { t: '2026-11-21T10', scope: 'hour', total: 6, kinds: { channel_refused: 3, sys_as_user: 3 } },
  { t: '2026-11-21T11', scope: 'hour', total: 12, kinds: { sanitize: 2, role_normalized: 10 } },
  { t: '2026-11-21T12', scope: 'hour', total: 3, kinds: { cache_miss: 3 } },
];
ctx.renderChannelEventChart(points);
const svg = hosts.dgEventChart.innerHTML;
const other = ctx.chSeriesCount({ total: 10, kinds: { channel_refused: 4, sanitize: 5 } },
  { key: 'other', kinds: null });
ctx.renderChannelEventChart([{ t: '2026-11-21T10', scope: 'hour', total: 0, kinds: {} }]);
const zero = hosts.dgEventChart.innerHTML;
ctx.renderChannelEventChart([]);
const empty = hosts.dgEventChart.innerHTML;

process.stdout.write(JSON.stringify({
  bars: (svg.match(/class="dbar"/g) || []).length,
  titles: (svg.match(/<title>/g) || []).length,
  hasGrid: svg.includes('class="gl"'),
  otherRest: other,
  zeroState: zero.includes('us-empty'),
  emptyState: empty.includes('us-empty'),
}));`
	f, err := os.CreateTemp(t.TempDir(), "channel-chart-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("channel chart node test failed: %v\n%s", err, out)
	}
	// b1: 只有通道拒绝 3（搬移重试不进图）→ 1 段；b2: 全是脱敏改写 → 0 段；
	// b3: cache_miss 3 归「其他」→ 1 段。合计 2 根柱子、3 条 tooltip。
	want := `{"bars":2,"titles":3,"hasGrid":true,"otherRest":1,"zeroState":true,"emptyState":true}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("channel chart=%s\nwant %s", out, want)
	}
}

// TestAppJSChannelRateChart 失败率折线：无请求的桶不画点（避免 0/0 假谷），
// 有请求才连线；空数据走空态。
func TestAppJSChannelRateChart(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; channel rate chart test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const chartStart = src.indexOf('function parsePointTime');
const chartEnd = src.indexOf('function fmtTokTip');
const escStart = src.indexOf('function esc(');
const escEnd = src.indexOf('function ago(');
const fmtStart = src.indexOf('function fmtTok(');
const fmtEnd = src.indexOf('function usStat(');
const seriesStart = src.indexOf('const CH_EVENT_SERIES');
const drawStart = src.indexOf('function renderChannelRateChart');
const drawEnd = src.indexOf('function renderChannelHealth');
if ([chartStart, chartEnd, escStart, escEnd, fmtStart, fmtEnd, seriesStart, drawStart, drawEnd].some(v => v < 0)) {
  throw new Error('rate chart helpers not found');
}
const hosts = {};
const doc = { getElementById: id => (hosts[id] = hosts[id] || { innerHTML: '', textContent: '', title: '' }) };
const ctx = { Date, Number, String, Math, RegExp, isNaN, Set, Array, Object, Infinity, document: doc, $: doc.getElementById };
vm.createContext(ctx);
vm.runInContext(
  src.slice(escStart, escEnd) + src.slice(fmtStart, fmtEnd) + src.slice(chartStart, chartEnd) +
  src.slice(seriesStart, drawStart) + src.slice(drawStart, drawEnd) +
  '\nthis.renderChannelRateChart=renderChannelRateChart;', ctx);

ctx.renderChannelRateChart([
  { t: '2026-11-21T10', scope: 'hour', req: 100, err: 1 },
  { t: '2026-11-21T11', scope: 'hour', req: 0, err: 0 },
  { t: '2026-11-21T12', scope: 'hour', req: 200, err: 2 },
]);
const svg = hosts.dgRateChart.innerHTML;
ctx.renderChannelRateChart([]);
const empty = hosts.dgRateChart.innerHTML;

process.stdout.write(JSON.stringify({
  lines: (svg.match(/ctl-total/g) || []).length,
  area: (svg.match(/class="ctarea"/g) || []).length,
  dots: (svg.match(/class="ctdot"/g) || []).length,
  titles: (svg.match(/<title>/g) || []).length,
  hasPct: svg.includes('%<'),
  emptyState: empty.includes('us-empty'),
}));`
	f, err := os.CreateTemp(t.TempDir(), "channel-rate-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("channel rate chart node test failed: %v\n%s", err, out)
	}
	// 3 个点里 1 个 req=0 被跳过 → 2 个圆点 / 2 条 tooltip。
	want := `{"lines":1,"area":1,"dots":2,"titles":2,"hasPct":true,"emptyState":true}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("rate chart=%s\nwant %s", out, want)
	}
}
