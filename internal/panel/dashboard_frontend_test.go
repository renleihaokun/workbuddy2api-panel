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

// TestAppJSChannelChart 反代劣化 100% 堆叠柱：系列按 kind 分组（成功/通道拒绝/
// 内容拦截/限流/其他），柱子类名 dbar，空数据走空态。
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
const seriesStart = src.indexOf('const DG_SERIES');
const seriesEnd = src.indexOf('function renderChannelHealth');
const drawStart = src.indexOf('function renderChannelChart');
const drawEnd = src.indexOf('async function loadChannelHealth');
if ([chartStart, chartEnd, escStart, escEnd, seriesStart, seriesEnd, drawStart, drawEnd].some(v => v < 0)) {
  throw new Error('channel chart helpers not found');
}
const hosts = {};
const doc = { getElementById: id => (hosts[id] = hosts[id] || { innerHTML: '', textContent: '', title: '' }) };
const ctx = { Date, Number, String, Math, RegExp, isNaN, Set, Array, Object, Infinity, document: doc, $: doc.getElementById };
vm.createContext(ctx);
vm.runInContext(
  src.slice(escStart, escEnd) + src.slice(chartStart, chartEnd) +
  src.slice(seriesStart, seriesEnd) + src.slice(drawStart, drawEnd) +
  '\nthis.renderChannelChart=renderChannelChart; this.dgSeriesCount=dgSeriesCount;', ctx);

const buckets = [
  { t: '2026-11-21T10', scope: 'hour', requests: 10, success: 5, kinds: { channel_refused: 3, soft_rate: 2 } },
  { t: '2026-11-21T11', scope: 'hour', requests: 4, success: 4 },
  { t: '2026-11-21T12', scope: 'hour', requests: 2, success: 1, kinds: { content_blocked: 1 } },
];
ctx.renderChannelChart(buckets);
const svg = hosts.dgChart.innerHTML;
const other = ctx.dgSeriesCount({ requests: 10, success: 5, kinds: { channel_refused: 3, soft_rate: 1 } },
  { key: 'other', kinds: null });
ctx.renderChannelChart([]);
const empty = hosts.dgChart.innerHTML;

process.stdout.write(JSON.stringify({
  bars: (svg.match(/class="dbar"/g) || []).length,
  noUsbar: !/class="usbar"/.test(svg),
  titles: (svg.match(/<title>/g) || []).length,
  has100: svg.includes('>100%<'),
  hasGrid: svg.includes('class="gl"'),
  otherRest: other,
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
	// b1: 成功5 + 通道拒绝3 + 限流2 = 10 → 3 段；b2: 成功4 → 1 段；
	// b3: 成功1 + 内容拦截1 = 2 → 2 段；合计 6 根。
	want := `{"bars":6,"noUsbar":true,"titles":3,"has100":true,"hasGrid":true,"otherRest":1,"emptyState":true}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("channel chart=%s\nwant %s", out, want)
	}
}
