// dashboard_frontend_test.go 两个看板的前端渲染切片测试（node + vm）：
// 积分走势折线（三条线 / 面积 / 圆点 tooltip / 单点与空态）与通道健康
// （双度量趋势 / 覆盖带 / 每桶 tooltip / 状态条 / 事件表 / 分域表）。
// 沿用 frontend_test.go 的切片手法，只截取纯渲染函数，不依赖真实 DOM。
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
if ([chartStart, chartEnd, escStart, escEnd, fmtStart, fmtEnd, stampStart, stampEnd, trendStart, trendEnd].some(v => v < 0) ||
    chartEnd <= chartStart || escEnd <= escStart || fmtEnd <= fmtStart || stampEnd <= stampStart || trendEnd <= trendStart) {
  throw new Error('credit trend helpers not found or slices inverted');
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

// TestAppJSChannelDashboard 通道健康看板：groups 驱动的堆叠柱、每千请求模式、
// 出站改写双折线、覆盖带/数据起点、每桶 tooltip、状态条、事件表（work 单位
// 不换算）、分域覆盖率、空态。
func TestAppJSChannelDashboard(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; channel dashboard test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const escStart = src.indexOf('function esc(');
const escEnd = src.indexOf('function ago(');
const agoStart = src.indexOf('function ago(');
const agoEnd = src.indexOf('function dur(');
const timeStart = src.indexOf('function parseAPITime');
const timeEnd = src.indexOf('/* ── 时间范围控件');
const fmtStart = src.indexOf('function fmtTok(');
const fmtEnd = src.indexOf('function usStat(');
const kpiStart = src.indexOf('function usKpi(');
const kpiEnd = src.indexOf('function usMixBar');
const chStart = src.indexOf('let channelData = null');
const chEnd = src.indexOf('async function loadChannelHealth');
const bounds = [escStart, escEnd, agoStart, agoEnd, timeStart, timeEnd, fmtStart, fmtEnd, kpiStart, kpiEnd, chStart, chEnd];
if (bounds.some(v => v < 0) || escEnd <= escStart || agoEnd <= agoStart || timeEnd <= timeStart ||
    fmtEnd <= fmtStart || kpiEnd <= kpiStart || chEnd <= chStart) {
  throw new Error('channel dashboard helpers not found or slices inverted');
}
const hosts = {};
const el = () => ({ innerHTML: '', textContent: '', hidden: false, value: '', onclick: null, onchange: null, querySelectorAll: () => [] });
const doc = {
  getElementById: id => (hosts[id] = hosts[id] || el()),
  querySelectorAll: () => [],
};
const ctx = {
  Date, Number, String, Math, RegExp, isNaN, Set, Array, Object, Infinity, JSON, Map,
  document: doc, $: doc.getElementById,
  localStorage: { getItem: () => null, setItem() {} },
  matchMedia: () => ({ matches: false, addEventListener() {} }),
  setInterval: () => 1, clearInterval() {},
  view: 'channel',
};
vm.createContext(ctx);
const stub = 'function parsePointTime(p){const s=p.t.length===13?p.t+":00:00":p.t+"T00:00:00";const d=new Date(s);return isNaN(d.getTime())?null:d.getTime();}';
vm.runInContext(
  stub +
  src.slice(escStart, escEnd) + src.slice(agoStart, agoEnd) + src.slice(timeStart, timeEnd) +
  src.slice(fmtStart, fmtEnd) + src.slice(kpiStart, kpiEnd) + src.slice(chStart, chEnd) +
  '\nthis.api={chGroupCount,chChartTotal,chStateChips,chKindRowsHtml,chRealmRowsHtml,chSortedKinds,renderChannelTrend,renderChannelRateChart};' +
  '\nthis.setMode=function(m){chMode=m;};this.setMeasure=function(m){chMeasure=m;};' +
  '\nthis.setSort=function(k,d){chSortKey=k;chSortDir=d;};', ctx);

const d = {
  window_from: '2026-11-21T10:00:00+00:00', window_to: '2026-11-21T13:00:00+00:00',
  bucket: 'hour', tz: '+00:00',
  groups: [{key:'refusal',label:'通道拒绝'},{key:'throttle',label:'限流·容量'},{key:'stream',label:'流中断·超时'},{key:'other',label:'其他'}],
  events: [
    {t:'2026-11-21T10',scope:'hour',kinds:{channel_refused:2,role_normalized:40},groups:{refusal:2,throttle:0,stream:0,other:0},total:42},
    {t:'2026-11-21T11',scope:'hour',kinds:{soft_rate:1,stream_error:1,sanitize:10},groups:{refusal:0,throttle:1,stream:1,other:0},total:12},
    {t:'2026-11-21T12',scope:'hour',kinds:{},groups:{refusal:0,throttle:0,stream:0,other:0},total:0},
  ],
  requests: [
    {t:'2026-11-21T10',scope:'hour',req:100,err:1},
    {t:'2026-11-21T11',scope:'hour',req:0,err:0},
    {t:'2026-11-21T12',scope:'hour',req:50,err:0},
  ],
  sources: {events:{since:'2026-11-21T11:00:00+00:00'}, usage:{since:'2026-10-14T12:00:00+00:00'}, archive:{}},
  totals: {requests:150, errors:1, failure_rate:0.7, events:54,
    kinds:{channel_refused:2, soft_rate:1, stream_error:1, role_normalized:40, sanitize:10},
    groups:{refusal:2, throttle:1, stream:1, other:0}},
  kinds: [
    {kind:'channel_refused',label:'通道拒绝（11-128）',group:'refusal',unit:'request',hint:'检查提示词模式',count:2,per_1k:13.33,last_at:'2026-11-21T10:30:00+00:00'},
    {kind:'role_normalized',label:'developer→system 归一',group:'offchart',unit:'work',hint:'按消息条数计',count:40,per_1k:0,last_at:''},
  ],
  state: {active:false, mode:'passthrough', learned_now:1, learned_total:5, degrade_last_at:'0001-01-01T00:00:00Z'},
  realms: {
    cn: {requests:10, success:9, success_rate:90, fallbacks:1},
    global: {requests:20, success:20, success_rate:100, fallbacks:0},
    unknown: {requests:70, success:70, success_rate:100, fallbacks:0},
  },
  realm_labeled: 30, realm_total: 100,
};

const a = ctx.api;
const out = {};
out.total = a.chChartTotal({groups:{refusal:3,throttle:1,stream:0,other:2}});
out.refusal = a.chGroupCount({groups:{refusal:3}}, 'refusal');

// 状态条：未接线 / 正常（零值时间不显示 0001）/ 降级中。
out.stateNull = a.chStateChips(null, 0).includes('状态不可用');
const okChips = a.chStateChips(d.state, 3);
out.stateOk = okChips.includes('通道正常') && okChips.includes('从未') && okChips.includes('跨域回退');
out.stateNoZeroTime = !okChips.includes('0001');
const warnChips = a.chStateChips({active:true, until:'2026-11-22T16:00:00Z', mode:'passthrough', learned_now:2, learned_total:5}, 0);
out.stateWarn = warnChips.includes('提示词降级中') && warnChips.includes('透传');

// 事件表：work 单位不换算每千请求，Hint 进说明列。
const kindHtml = a.chKindRowsHtml(d.kinds);
out.kindRow = kindHtml.includes('通道拒绝（11-128）') && kindHtml.includes('13.33');
out.kindWorkDash = kindHtml.includes('—') && kindHtml.includes('按消息条数计');
out.kindEmpty = a.chKindRowsHtml([]).includes('没有通道事件');

// 分域：只比较 cn/global，未标注不占一行；覆盖率文案由调用方给。
const realmHtml = a.chRealmRowsHtml(d);
out.realmRows = realmHtml.includes('cn') && realmHtml.includes('global') && !realmHtml.includes('unknown');
out.realmEmpty = a.chRealmRowsHtml({realm_labeled:0, realm_total:100, realms:{unknown:{requests:100}}}).includes('没有带域标记');

// 主图（拦截/绝对量）：3 根柱、3 个命中区、覆盖带 + 数据起点。
a.renderChannelTrend(d);
const svg = hosts.dgTrendChart.innerHTML;
out.bars = (svg.match(/class="dbar"/g) || []).length;
out.hits = (svg.match(/class="chhit"/g) || []).length;
out.gap = svg.includes('class="chgap"') && svg.includes('数据起点');
out.legend = hosts.dgTrendLegend.innerHTML.includes('通道拒绝');
out.tip = svg.includes('请求 100') && svg.includes('另：出站改写 40 条');

// 每千请求：11 点 req=0 不画，10 点 2/100*1000=20 → 1 根柱；图注写「每千请求」。
ctx.setMode('per1k');
a.renderChannelTrend(d);
const svg1k = hosts.dgTrendChart.innerHTML;
out.per1k = (svg1k.match(/class="dbar"/g) || []).length === 1 && hosts.dgTrendNote.textContent.includes('每千请求');

// 分母太小（5 请求 1 拦截 = 200‰）不画，避免假峰值。
a.renderChannelTrend({
  window_from: '2026-11-21T10:00:00+00:00', window_to: '2026-11-21T11:00:00+00:00',
  bucket: 'hour', tz: '+00:00', groups: d.groups,
  events: [{t:'2026-11-21T10',scope:'hour',kinds:{channel_refused:1},groups:{refusal:1,throttle:0,stream:0,other:0},total:1}],
  requests: [{t:'2026-11-21T10',scope:'hour',req:5,err:0}],
  sources: {},
});
out.per1kSmall = (hosts.dgTrendChart.innerHTML.match(/class="dbar"/g) || []).length === 0;

// 出站改写：双折线 + 独立图例。
ctx.setMode('abs'); ctx.setMeasure('rewrite');
a.renderChannelTrend(d);
const svgRw = hosts.dgTrendChart.innerHTML;
out.rewrite = (svgRw.match(/<polyline/g) || []).length === 2 &&
  hosts.dgTrendLegend.innerHTML.includes('归一') && hosts.dgTrendLegend.innerHTML.includes('指纹清洗');

// 失败率：req=0 的桶跳过（2 个命中区），断档分段（2 个单点圆、无折线），均值/峰值在图注。
ctx.setMeasure('intercept');
a.renderChannelRateChart(d);
const rate = hosts.dgRateChart.innerHTML;
out.rateHits = (rate.match(/class="chhit"/g) || []).length;
out.rateDots = (rate.match(/<circle/g) || []).length;
out.rateNote = hosts.dgRateNote.textContent.includes('均值') && hosts.dgRateNote.textContent.includes('峰值');

// 空态。
a.renderChannelTrend({events:[]});
out.trendEmpty = hosts.dgTrendChart.innerHTML.includes('没有事件数据');
a.renderChannelRateChart({requests:[]});
out.rateEmpty = hosts.dgRateChart.innerHTML.includes('没有请求数据');

// 排序：按次数升/降序。
ctx.setSort('count', 1);
const asc = a.chSortedKinds(d);
ctx.setSort('count', -1);
const desc = a.chSortedKinds(d);
out.sort = asc[0].kind === 'channel_refused' && desc[0].kind === 'role_normalized';

process.stdout.write(JSON.stringify(out));`
	f, err := os.CreateTemp(t.TempDir(), "channel-dashboard-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("channel dashboard node test failed: %v\n%s", err, out)
	}
	want := `{"total":6,"refusal":3,"stateNull":true,"stateOk":true,"stateNoZeroTime":true,` +
		`"stateWarn":true,"kindRow":true,"kindWorkDash":true,"kindEmpty":true,` +
		`"realmRows":true,"realmEmpty":true,"bars":3,"hits":3,"gap":true,"legend":true,"tip":true,` +
		`"per1k":true,"per1kSmall":true,"rewrite":true,"rateHits":2,"rateDots":2,"rateNote":true,` +
		`"trendEmpty":true,"rateEmpty":true,"sort":true}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("channel dashboard=%s\nwant %s", out, want)
	}
}
