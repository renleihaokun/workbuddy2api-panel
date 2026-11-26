'use strict';
/* ── 状态 ─────────────────────────────────────────────────────────── */
const LS_KEY = 'wb2api.key', LS_THEME = 'wb2api.theme';
/* 账号备注与「积分到期提醒」折叠的本机键（同上：模块级声明放顶部，避开 TDZ）。 */
const LS_REMARKS = 'wb2api.remarks.v1', LS_EXP_FOLD = 'wb2api.expfold.v1';
let remarksCache = null;
let theme = localStorage.getItem(LS_THEME) || 'auto';   // auto | light | dark
let view = 'accounts';
let overviewData = null, cfgLoaded = null;
/* 配置是否已成功从服务端回填。为 false 时整张表单禁用、保存按钮禁用：
   未回填就提交会把 checkbox 的 HTML 默认值（false）写回服务端，等于用空表单
   覆盖真实配置（历史事故，plan P0-A1）。 */
let cfgReady = false;
let cfgDirty = 0;
let cfgBaseline = null; // 表单回填后的快照：dirty 判定只看用户是否改过，不把 HTML 默认值算进去
let logPin = true, loginState = null, loginTimer = null;
// reloginUID 非空 = 当前弹窗是「给这个账号重登」而不是「添加新账号」：
// realm 跟账号走（不让选）、落盘就地改写、成功后自动解禁。
let reloginUID = null;
let refTimer = null;
/* 视图级筛选状态（模块级声明放在文件顶部，避免顶层 go() 早于声明执行时踩 TDZ）。 */
let mdFilter = { q: '', realm: '', cap: '', effort: '', promo: '', sort: 'default' };
let mdAll = [], mdProbes = {}, mdProbeByKey = {}, mdProbeOf = () => undefined;
let mdLocks = [], mdRecent = [];
let mdVisible = 20; // 手机/平板首屏只渲染 20 个模型，避免 59 张大卡一次进 DOM
let reqFilter = { q: '', outcome: '' };
let reqEntries = [];
let reqVisible = 20; // 手机/平板首屏只渲染 20 条，避免 100 张卡一次进 DOM
let usDim = 'account', usCreditDim = 'account', usSort = 'total';
let usageData = null;
let reqRangeState = null; // 请求记录的时间范围（用量页的见 trangeState）
let pkSelectedUid = '';    // 积分构成：当前查看逐包明细的账号

const $ = id => document.getElementById(id);

/* ── 主题 ─────────────────────────────────────────────────────────── */
/* 三态循环：跟随系统 → 浅色 → 深色 → 跟随系统（与按钮 title 的承诺一致）。
   「跟随系统」时**不写 data-theme 属性**——外观交给 CSS 的
   @media (prefers-color-scheme: light) 决定。这样做有两个好处：
     1. 系统换主题时浏览器自己就跟着变，连 JS 都不需要参与；
     2. app.js 尚未执行的第一帧就已经是正确配色，不会闪一下深色。
   手动锁定后才写 [data-theme]，由显式选择器覆盖媒体查询。 */
const THEME_ORDER = ['auto', 'light', 'dark'];
const THEME_LABEL = { auto: '跟随系统', light: '浅色', dark: '深色' };
const ICON_SUN = '<circle cx="8" cy="8" r="3"/><path d="M8 1v2M8 13v2M1 8h2M13 8h2M3.2 3.2l1.4 1.4M11.4 11.4l1.4 1.4M12.8 3.2l-1.4 1.4M4.6 11.4l-1.4 1.4"/>';
const ICON_MOON = '<path d="M13.2 9.6A5.6 5.6 0 0 1 6.4 2.8a5.6 5.6 0 1 0 6.8 6.8z"/>';
/* 「自动」用半填充的圆：右半实心表示"随系统明暗走"，与太阳/月亮可区分 */
const ICON_AUTO = '<circle cx="8" cy="8" r="5.6"/><path d="M8 2.4a5.6 5.6 0 0 1 0 11.2z" fill="currentColor" stroke="none"/>';
const SYS_LIGHT_MQ = matchMedia('(prefers-color-scheme: light)');
/* 存档里的值可能是历史脏数据（旧版本只有两态），不合法就退回 auto。 */
if (THEME_ORDER.indexOf(theme) < 0) theme = 'auto';

function effTheme() {
  return theme === 'auto' ? (SYS_LIGHT_MQ.matches ? 'light' : 'dark') : theme;
}
/* applyTheme 只负责「把当前状态同步到 DOM」，不读也不写 localStorage：
   系统偏好变化时可以直接复用，不会把 auto 意外固化成某个具体主题。 */
function applyTheme() {
  const root = document.documentElement;
  if (theme === 'auto') root.removeAttribute('data-theme');
  else root.setAttribute('data-theme', theme);
  const eff = effTheme();
  const ico = $('icoTheme'), btn = $('btnTheme');
  if (ico) ico.innerHTML = eff === 'light' ? ICON_SUN : (theme === 'auto' ? ICON_AUTO : ICON_MOON);
  if (btn) {
    const next = THEME_ORDER[(THEME_ORDER.indexOf(theme) + 1) % THEME_ORDER.length];
    const tip = '主题：' + THEME_LABEL[theme] + '（点击切到「' + THEME_LABEL[next] + '」）';
    btn.title = tip;
    btn.setAttribute('aria-label', tip);
  }
}
/* 系统主题变化：只有「跟随系统」时需要重绘图标（外观本身由 CSS 自动跟随）。 */
function onSystemThemeChange() { if (theme === 'auto') applyTheme(); }
if (SYS_LIGHT_MQ.addEventListener) SYS_LIGHT_MQ.addEventListener('change', onSystemThemeChange);
else if (SYS_LIGHT_MQ.addListener) SYS_LIGHT_MQ.addListener(onSystemThemeChange); // 老内核兜底

/* 切主题时给 <html> 挂一个短暂的过渡窗口类，让颜色平滑过渡而不是硬切；
   过渡结束就摘掉——不给全站元素常驻 transition，避免无谓的合成开销。 */
let themeAnimTimer = null;
function withThemeTransition(fn) {
  const root = document.documentElement;
  root.classList.add('theme-anim');
  fn();
  if (themeAnimTimer) clearTimeout(themeAnimTimer);
  themeAnimTimer = setTimeout(() => { root.classList.remove('theme-anim'); themeAnimTimer = null; }, 320);
}
$('btnTheme').onclick = () => {
  theme = THEME_ORDER[(THEME_ORDER.indexOf(theme) + 1) % THEME_ORDER.length];
  try { localStorage.setItem(LS_THEME, theme); } catch (e) { /* 隐私模式禁写：忽略 */ }
  withThemeTransition(applyTheme);
};
applyTheme();

/* ── 请求 ─────────────────────────────────────────────────────────── */
async function api(path, opts = {}) {
  const h = Object.assign({}, opts.headers || {});
  const k = localStorage.getItem(LS_KEY);
  if (k) h['Authorization'] = 'Bearer ' + k;
  if (opts.body) h['Content-Type'] = 'application/json';
  const r = await fetch('/panel/api/' + path, Object.assign({}, opts, { headers: h }));
  if (r.status === 401) { openKey(); throw new Error('密钥无效或未填写'); }
  const d = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(d.error || ('HTTP ' + r.status));
  return d;
}
/* toast：分级 + 可关闭 + 合并重复 + 最多 3 条。
   以前是「3.6 秒后无条件 remove」：长错误读不完、配置页会盖住保存按钮、
   连续报错叠成一片、色弱用户只能靠左边框颜色分辨。 */
const TST_MAX = 3;
function toast(msg, cls) {
  const host = $('toasts');
  if (!host) return;
  const text = shortErr(msg, 240);
  const kind = cls === 'err' ? 'err' : (cls === 'ok' ? 'ok' : '');
  const now = Date.now();
  // 2 秒内同文案合并成「×N」，避免一次 401 触发多条一模一样的错误。
  const dup = [...host.children].find(el => el.dataset.text === text && now - Number(el.dataset.at || 0) < 2000);
  if (dup) {
    const n = (Number(dup.dataset.count) || 1) + 1;
    dup.dataset.count = n; dup.dataset.at = now;
    let badge = dup.querySelector('.tst-n');
    if (!badge) { badge = document.createElement('span'); badge.className = 'tst-n'; dup.appendChild(badge); }
    badge.textContent = '×' + n;
    return;
  }
  const el = document.createElement('div');
  el.className = 'tst ' + kind;
  el.dataset.text = text; el.dataset.at = now; el.dataset.count = '1';
  el.setAttribute('role', kind === 'err' ? 'alert' : 'status');
  const textEl = document.createElement('span');
  textEl.className = 'tst-msg';
  textEl.textContent = text;
  const close = document.createElement('button');
  close.type = 'button'; close.className = 'tst-x'; close.setAttribute('aria-label', '关闭提示');
  close.textContent = '×';
  el.append(textEl, close);
  let timer = null;
  const kill = () => {
    if (!el.isConnected) return;
    el.classList.add('out');
    setTimeout(() => el.remove(), 180);
  };
  const arm = ms => { clearTimeout(timer); timer = setTimeout(kill, ms); };
  close.addEventListener('click', kill);
  el.addEventListener('mouseenter', () => clearTimeout(timer));
  el.addEventListener('mouseleave', () => arm(1800));
  arm(kind === 'err' ? 8000 : 3200);
  host.appendChild(el);
  while (host.children.length > TST_MAX) host.firstElementChild.remove();
}
/* setBusy 统一异步按钮的「进行中」外观：禁用 + aria-busy + 可选文案替换。
   转圈由 CSS 的 button[aria-busy="true"]::after 负责，这里只管状态与文案。
   返回恢复函数，配合 try/finally 使用：
     const done = setBusy(btn, '保存中…');
     try { ... } finally { done('保存配置'); }
   返回函数的参数为空时，恢复成进入时的原始文案。 */
function setBusy(btn, label) {
  if (!btn) return () => {};
  const prevText = btn.textContent;
  btn.disabled = true;
  btn.setAttribute('aria-busy', 'true');
  if (label) btn.textContent = label;
  return function done(nextLabel) {
    btn.disabled = false;
    btn.removeAttribute('aria-busy');
    btn.textContent = nextLabel == null ? prevText : nextLabel;
  };
}
// esc 文本/属性双安全转义。不能只用 div.innerHTML（它转义 <>& 但不转义引号），
// 否则字符串拼进 HTML 属性（如 title="uid: ..."）时引号可闭合属性并注入事件处理器。
// 显式替换 5 个字符：& < > " '（& 必须最先，避免二次转义）。
function esc(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}
/* shortErr 把外部来的错误文本压成一行可读短句。
   上游出错时响应体常常是整页 HTML（"<html><head><title>401 Authorization Required…"），
   原样铺进界面或弹条既占地方、又像页面自己坏了。这里去标签 + 折叠空白 + 截断。 */
function shortErr(msg, max) {
  const s = String(msg == null ? '' : msg)
    .replace(/<[^>]*>/g, ' ')   // 完整标签
    .replace(/<[^>]*$/, ' ')    // 被截断的半个标签（后端也会截断错误文本，可能停在 "<a href=" 中间）
    .replace(/\s+/g, ' ')
    .trim();
  const n = max || 140;
  return s.length > n ? s.slice(0, n - 1) + '…' : s;
}
/* askConfirm 优先使用 controls.js 的自绘确认框（同一套视觉、可键盘操作）；
   极端情况下 controls.js 没加载成功就退回原生 confirm，保证危险操作仍有确认。 */
async function askConfirm(opts) {
  if (window.WBControls && typeof window.WBControls.confirm === 'function') {
    return window.WBControls.confirm(opts);
  }
  return window.confirm((opts && (opts.body || opts.title)) || '确认操作');
}
function ago(iso) {
  if (!iso || iso.startsWith('0001-')) return '—';
  const s = (Date.now() - new Date(iso)) / 1000;
  if (s < 0) return '刚刚';
  if (s < 60) return Math.floor(s) + ' 秒前';
  if (s < 3600) return Math.floor(s / 60) + ' 分钟前';
  if (s < 86400) return Math.floor(s / 3600) + ' 小时前';
  return Math.floor(s / 86400) + ' 天前';
}
// shortUid 弹窗标题里的 uid：前 8 + … + 后 4。以前是裸 slice(0,16)，看着像
// 「0ff3a47b-d495-48」——既没省略号也不知道是不是被截了，四个号前缀相同时分不清。
function shortUid(uid) {
  const s = String(uid || '');
  if (!s) return '—';
  return s.length > 14 ? s.slice(0, 8) + '…' + s.slice(-4) : s;
}
function dur(sec) {
  sec = Math.max(0, Math.round(sec));
  const h = Math.floor(sec / 3600), m = Math.floor(sec % 3600 / 60), s = sec % 60;
  return h ? h + '时' + String(m).padStart(2, '0') + '分' : m ? m + '分' + String(s).padStart(2, '0') + '秒' : s + '秒';
}
function parseAPITime(value) {
  const text = String(value || '');
  if (!text || text.startsWith('0001-')) return 0;
  const ms = Date.parse(text);
  return Number.isFinite(ms) ? ms : 0;
}
function fmtLocalDateTime(ms) {
  const d = new Date(ms);
  const p = n => String(n).padStart(2, '0');
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' +
    p(d.getHours()) + ':' + p(d.getMinutes());
}

/* fmtStampShort 表格/日志里的时间戳：MM-DD HH:mm:ss。
   请求记录、积分历史、运行日志都可能跨天，只给时分秒会出现两行都写 14:05:06、
   却分不清哪天的情形。年份不进列宽，完整值挂在单元格 title 上。 */
function fmtStampShort(ts) {
  const d = new Date(ts);
  if (isNaN(d.getTime())) return '—';
  const p = n => String(n).padStart(2, '0');
  return p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' +
    p(d.getHours()) + ':' + p(d.getMinutes()) + ':' + p(d.getSeconds());
}

/* ── 时间范围控件（用量 / 请求记录共用）────────────────────────────────
   预设项：今天 / 近 24 小时 / 近 3 天 / 近 7 天 / 近 30 天 / 全部历史 / 自定义。

   为什么区间一律由前端算好再发：
     - 「今天」必须是**浏览器本地时区**的 00:00 起。服务端时区未必与浏览器一致
       （容器常挂 TZ=Asia/Shanghai，而浏览器可能在任何时区），让服务端算"今天"
       会在跨时区时切错日子。
     - 「自定义」本来就是用户挑的具体时刻，没有任何服务端推导空间。

   滚动预设（近 N 小时/天）则保留 hours 参数：服务端按整点对齐的滚动窗口与旧
   行为逐位一致，前端自己减 N 小时会多算/少算一个边界桶。 */
const TRANGE_PRESETS = [
  ['today', '今天'],
  ['24', '近 24 小时'],
  ['72', '近 3 天'],
  ['168', '近 7 天'],
  ['720', '近 30 天'],
  ['0', '全部历史'],
  ['custom', '自定义…'],
];
const TRANGE_DEFAULT = '72';
const trangeStates = new Map(); // hostId → { preset, from: Date|null, to: Date|null }

// dtLocalValue / dtLocalParse 与 <input type=datetime-local> 的取值格式互转
// （YYYY-MM-DDTHH:mm，本地时区；ES 里"带时间的日期串"按本地解析，正是我们要的）。
function dtLocalValue(d) {
  const p = n => String(n).padStart(2, '0');
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + 'T' +
    p(d.getHours()) + ':' + p(d.getMinutes());
}
function dtLocalParse(s) {
  if (!s) return null;
  const d = new Date(s);
  return isNaN(d.getTime()) ? null : d;
}

// trangeMidnight 今天 00:00（本地时区）。
function trangeMidnight() {
  const d = new Date();
  d.setHours(0, 0, 0, 0);
  return d;
}

function trangeState(id) {
  if (!trangeStates.has(id)) {
    // 「自定义」的初始值给一段有意义的默认：今天 00:00 → 现在。
    trangeStates.set(id, { preset: TRANGE_DEFAULT, from: trangeMidnight(), to: new Date() });
  }
  return trangeStates.get(id);
}

// trangeRender 画出控件骨架（幂等：重复调用会保留当前状态）。
function trangeRender(id) {
  const host = $(id);
  if (!host) return;
  const st = trangeState(id);
  const custom = st.preset === 'custom';
  host.innerHTML =
    '<select class="tr-preset" name="tr-preset" aria-label="时间范围">' +
    TRANGE_PRESETS.map(([v, label]) =>
      '<option value="' + v + '"' + (v === st.preset ? ' selected' : '') + '>' + esc(label) + '</option>').join('') +
    '</select>' +
    '<span class="tr-custom"' + (custom ? '' : ' hidden') + '>' +
    '<input type="datetime-local" class="tr-from" name="tr-from" value="' + esc(st.from ? dtLocalValue(st.from) : '') + '" aria-label="起始时间">' +
    '<span class="tr-sep">→</span>' +
    '<input type="datetime-local" class="tr-to" name="tr-to" value="' + esc(st.to ? dtLocalValue(st.to) : '') + '" aria-label="结束时间">' +
    '</span>';
  const preset = host.querySelector('.tr-preset');
  if (preset) preset.onchange = () => {
    st.preset = preset.value;
    // 从别的预设切到自定义时，把区间重置为"今天 00:00 → 现在"，
    // 免得用户上次留下的半年区间被无声沿用。
    if (st.preset === 'custom' && (!st.from || !st.to)) { st.from = trangeMidnight(); st.to = new Date(); }
    trangeRender(id);
    trangeEmit(id);
  };
  const fromEl = host.querySelector('.tr-from');
  const toEl = host.querySelector('.tr-to');
  const readCustom = () => {
    st.from = dtLocalParse(fromEl.value);
    st.to = dtLocalParse(toEl.value);
    // 起止颠倒就地标红（不静默纠正：用户可能正输到一半）。
    const bad = st.from && st.to && st.from > st.to;
    fromEl.classList.toggle('tr-bad', !!bad);
    toEl.classList.toggle('tr-bad', !!bad);
    if (bad) return;
    trangeEmit(id);
  };
  if (fromEl) fromEl.onchange = readCustom;
  if (toEl) toEl.onchange = readCustom;
}

const trangeHandlers = new Map();
// trangeBind 渲染控件并登记变化回调。**不**在绑定时触发回调：各视图的首次加载
// 由 go() 统一驱动，这里再触发一次会让打开页面时打两遍接口。
function trangeBind(id, onChange, preset) {
  trangeHandlers.set(id, onChange);
  if (preset) trangeState(id).preset = preset;
  trangeRender(id);
}
function trangeEmit(id) {
  const fn = trangeHandlers.get(id);
  if (fn) fn();
}

// trangeQuery 把当前选择翻译成查询参数。
//   rolling=true  → 滚动预设发 hours（服务端整点对齐），今天/自定义发 from/to
//   rolling=false → 一律发 from/to（归档是线性日志，前端算区间更直观）
// 「全部历史」两者都不发。
function trangeQuery(id, rolling) {
  const st = trangeState(id);
  const q = new URLSearchParams();
  const sec = d => Math.floor(d.getTime() / 1000);
  if (st.preset === 'custom') {
    if (st.from) q.set('from', sec(st.from));
    if (st.to) q.set('to', sec(st.to));
    return q;
  }
  if (st.preset === 'today') {
    q.set('from', sec(trangeMidnight()));
    return q;
  }
  // 全部历史：滚动端点（用量）必须**显式**传 hours=0。
  //
  // 后端对「什么都不给」的缺省是 72 小时（见 panel.go 的说明：
  // 「都不给：等同于 hours=72（保持旧调用方行为）」），所以这里返回空 query 会被
  // 当成「近 3 天」—— 正是 issue #121 报的现象：选了「全部历史」，数字却和
  // 「近 3 天」一模一样。
  //
  // 非滚动端点（请求记录）没有缺省窗口：不传 from/to 即"不限起点"，保持空 query。
  if (st.preset === '0') {
    if (rolling) q.set('hours', '0');
    return q;
  }
  if (rolling) { q.set('hours', st.preset); return q; }
  q.set('from', sec(new Date(Date.now() - Number(st.preset) * 3600 * 1000)));
  return q;
}

// trangeLabel 人读口径，用于「用量总览」右上角这类需要回显区间的位置。
function trangeLabel(id) {
  const st = trangeState(id);
  const found = TRANGE_PRESETS.find(p => p[0] === st.preset);
  if (st.preset !== 'custom') return found ? found[1] : '';
  if (!st.from && !st.to) return '自定义';
  const f = d => d ? (d.getMonth() + 1) + '-' + String(d.getDate()).padStart(2, '0') + ' ' +
    String(d.getHours()).padStart(2, '0') + ':' + String(d.getMinutes()).padStart(2, '0') : '…';
  return f(st.from) + ' → ' + f(st.to);
}
function rateLimitMeta(row, now) {
  const model = String(row && row.model || '未知模型');
  const kind = String(row && row.kind || 'rate_limit');
  const resetAt = parseAPITime(row && row.reset_at);
  const until = parseAPITime(row && row.until);
  const deadline = resetAt || until;
  const remaining = deadline > now ? Math.round((deadline - now) / 1000) : 0;
  if (kind === 'model_unavailable') {
    return {
      model,
      kind,
      detail: remaining ? '预计 ' + dur(remaining) + ' 后重试' : '等待重新探测',
      title: model + '\n模型当前不可用' + (deadline ? '\n最早重试：' + fmtLocalDateTime(deadline) : ''),
    };
  }
  let detail;
  const title = [model, resetAt ? '上游重置：' + fmtLocalDateTime(resetAt) : '上游重置：时间未知'];
  if (until && resetAt && until < resetAt) {
    // 双时钟（issue #171 问题二）：黄框出现/消失由 until 决定（网关为防超长
    // 限流拖死全池，把重试封顶在 now+softRateMax），主文字却按 resetAt 显示，
    // 导致「写的解封时间」与「实际掉框时刻」不一致。此时主文字同时明示两个
    // 时刻与口径，倒计时跟随 until（与实际掉框一致，也与「模型与档位」页
    // 锁池表的 until 口径对齐）。
    const retryIn = until > now ? Math.round((until - now) / 1000) : 0;
    detail = '网关 ' + fmtLocalDateTime(until) + ' 起重试' + (retryIn ? '（剩余 ' + dur(retryIn) + '）' : '') +
      ' · 上游称 ' + fmtLocalDateTime(resetAt) + ' 恢复';
    title.push('网关最早重试：' + fmtLocalDateTime(until));
  } else {
    detail = resetAt
      ? '预计 ' + fmtLocalDateTime(resetAt) + ' 解封' + (remaining ? '（剩余 ' + dur(remaining) + '）' : '')
      : (until ? '预计 ' + fmtLocalDateTime(until) + ' 恢复（剩余 ' + dur(remaining) + '）' : '预计解封时间未知');
  }
  return { model, kind, detail, title: title.join('\n') };
}
function rateLimitRowsHtml(rows, now) {
  const list = Array.isArray(rows) ? rows.filter(row => row && row.model) : [];
  if (!list.length) return '';
  return '<div class="rate-limits">' + list.map(row => {
    const m = rateLimitMeta(row, now);
    return '<div class="rate-limit ' + (m.kind === 'model_unavailable' ? 'model-unavailable' : '') +
      '" title="' + esc(m.title) + '"><b>' + esc(m.model) + '</b><span>' + esc(m.detail) + '</span></div>';
  }).join('') + '</div>';
}

/* ── 弹层管理器 ─────────────────────────────────────────────────────
   五个 .veil 以前只有 CSS 的 display 切换：读屏不知道是对话框、Tab 会跑到背后、
   Esc 关不掉、背景还能滚。这里统一补语义与行为，open/close 两个入口是唯一开关。

   - role="dialog" / aria-modal / aria-labelledby（取弹层里的 h3）
   - 打开时记录触发元素、把焦点送进弹层；关闭时归还
   - Tab 在弹层内循环；Esc 关最上层（data-lock 的密钥门除外）
   - html.lock 锁背景滚动；点遮罩关闭（密钥门除外） */
const DLG_STACK = [];
const DLG_FOCUS_SEL = 'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';

function dlgFocusables(veil) {
  return [...veil.querySelectorAll(DLG_FOCUS_SEL)].filter(el => {
    const r = el.getBoundingClientRect();
    return r.width > 0 || r.height > 0;
  });
}

function openVeil(id) {
  const veil = $(id);
  if (!veil) return;
  const dlg = veil.querySelector('.dlg');
  if (dlg) {
    dlg.setAttribute('role', 'dialog');
    dlg.setAttribute('aria-modal', 'true');
    const h = dlg.querySelector('h3');
    if (h) {
      if (!h.id) h.id = id + 'Title';
      dlg.setAttribute('aria-labelledby', h.id);
    }
  }
  if (veil.classList.contains('on') && DLG_STACK.some(x => x.veil === veil)) return;
  const prev = document.activeElement;
  veil.classList.add('on');
  document.documentElement.classList.add('lock');
  DLG_STACK.push({ veil, prev });
  setTimeout(() => {
    // 初焦点策略（v5）：以前一律落在「第一个可聚焦元素」，积分任务弹窗就把焦点
    // 压在了「全部接受」上——一个批量动作默认吃 Enter，而且它的蓝色焦点环跟真正的
    // 主按钮「一键完成可自动任务」打架，看起来像两个主按钮。
    // 现在的顺序：显式 data-autofocus → 第一个文本输入（有输入框的弹窗就是来打字的）
    // → 页脚里最后一个非主色按钮（通常是「关闭」，最不具破坏性）→ 兜底第一个。
    const f = dlgFocusables(veil);
    const field = f.find(el => el.matches &&
      el.matches('input:not([type=checkbox]):not([type=radio]):not([type=hidden]), textarea, select'));
    const auto = dlg.querySelector('[data-autofocus]');
    const dismiss = dlg.querySelector('footer button:not(.primary):not(.danger):last-of-type');
    const target = auto || field || dismiss || f[0] || dlg;
    if (target && target.focus) target.focus({ preventScroll: true });
  }, 30);
}

function closeVeil(id) {
  const veil = $(id);
  const i = DLG_STACK.findIndex(x => x.veil === veil);
  if (i < 0) { if (veil) veil.classList.remove('on'); return; }
  const { prev } = DLG_STACK.splice(i, 1)[0];
  veil.classList.remove('on');
  if (!DLG_STACK.length) document.documentElement.classList.remove('lock');
  if (prev && prev.focus) { try { prev.focus({ preventScroll: true }); } catch (e) { prev.focus(); } }
}

document.addEventListener('keydown', e => {
  if (!DLG_STACK.length) return;
  // 下拉候选层/行内菜单开着时，Esc 先交给它们处理（controls.js 的 capture 监听更晚注册）
  if (document.querySelector('.sel-pop, .sel-sheet, .ctx-pop')) return;
  const top = DLG_STACK[DLG_STACK.length - 1];
  if (e.key === 'Escape') {
    if (top.veil.hasAttribute('data-lock')) return;
    e.preventDefault();
    closeVeil(top.veil.id);
    return;
  }
  if (e.key === 'Tab') {
    const f = dlgFocusables(top.veil);
    if (!f.length) { e.preventDefault(); return; }
    const first = f[0], last = f[f.length - 1];
    if (!top.veil.contains(document.activeElement)) { e.preventDefault(); first.focus(); return; }
    if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
    else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
  }
}, true);

document.querySelectorAll('.veil').forEach(v => {
  v.addEventListener('pointerdown', e => {
    if (e.target !== v || v.hasAttribute('data-lock')) return;
    e.preventDefault();
    closeVeil(v.id);
  });
});

/* ── 密钥门 ───────────────────────────────────────────────────────── */
function openKey() { openVeil('keyVeil'); setTimeout(() => $('keyInput').focus(), 60); }
$('btnKey').onclick = async () => {
  const v = $('keyInput').value.trim();
  if (!v) return;
  localStorage.setItem(LS_KEY, v);
  try {
    await api('overview');
    $('keyErr').hidden = true;
    closeVeil('keyVeil');
    start();
  } catch (e) { $('keyErr').hidden = false; }
};
/* 回车提交走表单的 submit，而不是 keydown：
   index.html 里原本是 onsubmit="return false"，但内联事件处理器被 CSP
   (script-src 'self') 静默拦掉，于是回车会触发浏览器默认提交、整页重载。
   现在改成同源脚本里 preventDefault，回车=点「进入」，且不会刷新页面。 */
$('keyForm').addEventListener('submit', e => { e.preventDefault(); $('btnKey').click(); });

/* ── 路由 ─────────────────────────────────────────────────────────── */
/* 积分包明细共享缓存：「积分构成」视图与首页「积分到期提醒」卡片共用同一份
   /panel/api/packages 数据（逐账号实时查上游，能省一次是一次）。声明在路由区
   是因为 go() 的初始调用就会触发首页卡片的渲染，必须先于它就位。 */
let lastPackages = null;
let lastPackagesAt = 0;
let expFetching = false;                       // 到期卡片在途标记（防重复打上游）
const EXP_FRESH_MS = 2 * 60 * 1000;            // 缓存新鲜窗口：2 分钟内复用

const TITLES = { accounts: '账号池', usage: '用量', packages: '积分构成', channel: '通道健康', taskscenter: '任务中心', models: '模型与档位', config: '配置', logs: '运行日志' };
function viewFromHash() {
  const hash = (location.hash || '#accounts').slice(1);
  return hash in TITLES ? hash : 'accounts';
}
function go(v) {
  view = v;
  // 视图切换是整页级跳转：不复用上一页的滚动位置。以前从 11k 高的
  // 「积分构成」切到短页会落在页底（浏览器只做 scrollY 夹取），看起来像
  // 页面坏了；这里显式回顶。
  window.scrollTo(0, 0);
  document.querySelectorAll('.view').forEach(s => s.hidden = s.id !== 'view-' + v);
  document.querySelectorAll('.nav a').forEach(a => {
    const on = a.dataset.view === v;
    a.classList.toggle('on', on);
    if (on) a.setAttribute('aria-current', 'page'); else a.removeAttribute('aria-current');
  });
  // 当前视图挂在 body 上：CSS 用它做「上下文动作」（如仅账号池显示「添加账号」）
  document.body.dataset.view = v;
  $('ttl').textContent = TITLES[v];
  document.title = TITLES[v] + ' · WorkBuddy2API';
  if (v === 'models' && !$('mdBody').children.length) loadModels();
  if (v === 'config') loadConfig();
  if (v === 'logs') loadLogs();
  if (v === 'usage') loadUsage();
  if (v === 'packages') { loadPackages(); loadCreditTrend(); }
  if (v === 'channel') loadChannelHealth();
  // 到期提醒卡片：默认可见，进页面即用 overview 快照渲染（零上游请求，随首页统计
  // 每轮刷新）。不再「打开账号池就打上游」——逐账号实时查在账号多时会卡死面板。
  // 完整逐包明细由卡片上的「检查」按钮显式触发（loadPackages/loadExpiry）。
  if (v === 'taskscenter') reattachQueueView();
  // 视图从 hidden 变可见后才有真实宽度：此时再判定一次表格是否溢出。
  setTimeout(markScrollables, 30);
}
document.querySelectorAll('.nav a').forEach(a => a.onclick = e => {
  e.preventDefault();
  const next = a.dataset.view;
  if (next === view) {
    // 重复点当前项只归一化 URL，不新增历史记录。
    history.replaceState({ view: next }, '', '#' + next);
    return;
  }
  go(next);
  // pushState 而不是 replaceState：浏览器后退键应回到上一个面板视图，
  // 而不是直接退出面板。初始加载仍用 replaceState 归一化，见文件末尾。
  history.pushState({ view: next }, '', '#' + next);
});
/* 浏览器前进/后退：popstate 是主路径，hashchange 兜底手动改 hash 的场景。
   两者都走 viewFromHash()，并跳过「已经在这个视图」的重复调用。 */
function syncFromHistory() {
  const next = viewFromHash();
  if (next !== view) go(next);
}
addEventListener('popstate', syncFromHistory);
addEventListener('hashchange', syncFromHistory);
/* 首次进入延到本轮脚本求值之后再 go()。
   原因：go() 会同步触发视图的数据加载（loadUsage/loadLogs/loadPackages…），而这些
   函数读到的模块级 let/const（usageRateWarmAt、PK_* 等）在文件后半段才初始化——
   直接深链 #usage / #packages 打开页面时会踩 TDZ（"Cannot access 'x' before
   initialization"），表现为该页永远显示"读取失败"，而点导航进去一切正常。
   延迟 0ms 让整份脚本先求值完，是修这一类问题最省事也最不容易再犯的办法。 */
setTimeout(() => {
  const initial = viewFromHash();
  go(initial);
  // 未知 hash 归一化掉；正常 hash 也写入 history.state，后退时能恢复视图。
  history.replaceState({ view: initial }, '', '#' + initial);
}, 0);

/* ── 账号池 ───────────────────────────────────────────────────────── */
/* 账号名消歧：重名昵称追加 uid4；长邮箱保留 @ 前本地段，只缩域名。
   1280/1024 的账号列只有 170px，直接省略号会把 renleihaokun1..5 截成同一个
   "renleihaoku…"，用户无法分辨。备注优先（remarkName 会覆盖这里的显示名）。 */
function acctNameCounts(list) {
  const counts = new Map();
  for (const a of list || []) {
    const raw = String((a && a.nickname) || '').trim().toLowerCase();
    if (raw) counts.set(raw, (counts.get(raw) || 0) + 1);
  }
  return counts;
}
function acctDisplayName(a, counts) {
  const raw = String((a && (a.nickname || a.name)) || '').trim() ||
    String((a && a.uid) || '').slice(0, 8);
  // v5：账号列从「钉死 170px」换成弹性列（1024 下 206px / 1440 下 300px / 宽屏 600+），
  // 所以不再需要无条件把邮箱中间截断——以前连 1920 都显示成 renleihao…n2@…，
  // 而列里明明有 600px 放得下全名。现在按列宽档位给阈值，剩下的交给 CSS 省略号
  // （.nm-text），完整名字始终在 title 里。
  const wide = typeof matchMedia === 'function' && matchMedia('(min-width: 1200px)').matches;
  const emailLimit = wide ? 34 : 20;
  const nameLimit = wide ? 30 : 12;
  let short = raw;
  if (raw.indexOf('@') > 0 && raw.length > emailLimit) {
    const local = raw.split('@')[0];
    short = local.length > 18
      ? local.slice(0, 15) + '…' + local.slice(-2) + '@…'
      : local + '@…';
  } else if (raw.length > nameLimit) {
    // 非邮箱长名：保留头尾，中间省略（尾部通常是区分号的那个数字）
    short = raw.slice(0, nameLimit - 2) + '…' + raw.slice(raw.length > nameLimit + 2 ? -2 : -1);
  }
  const count = counts ? counts.get(raw.toLowerCase()) || 0 : 0;
  if (count > 1 && a && a.uid && short.indexOf(' · ') < 0) {
    short += ' · ' + String(a.uid).slice(0, 4);
  }
  return short;
}
function renderAccounts(list) {
  const tb = $('accBody');
  const nameCounts = typeof acctNameCounts === 'function' ? acctNameCounts(list) : new Map();
  if (!list.length) {
    tb.innerHTML = '<tr><td colspan="9"><div class="empty">' +
      '<div class="ico"><svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4"><circle cx="8" cy="5.5" r="2.6"/><path d="M2.8 13.5c.6-2.6 2.7-4 5.2-4s4.6 1.4 5.2 4"/></svg></div>' +
      '<div class="big">账号池是空的</div>' +
      '添加一个 WorkBuddy 账号后，这里会显示它的可用状态、积分与用量' +
      '<div class="cta"><button class="primary" data-a="add">添加账号</button></div>' +
      '</div></td></tr>';
    return;
  }
  // 有总额度（credits_total）→ 进度条按自身 剩余/总额 百分比；旧数据无总额 → 退回池内最高=100%
  const maxCred = Math.max(1, ...list.map(s => s.credits || 0));
  tb.innerHTML = list.map(s => {
    const dispName = typeof acctDisplayName === 'function'
      ? acctDisplayName(s, nameCounts)
      : (s.nickname || String(s.uid || '').slice(0, 8));
    const bl = (new Date(s.breaker_until || 0) - Date.now()) / 1000;
    const dg = (new Date(s.degrade_until || 0) - Date.now()) / 1000;
    const cool = Math.max(s.cool_remaining_sec || 0, bl > 0 ? bl : 0, dg > 0 ? dg : 0);
    let cls = '', tag;
    if (s.disabled) {
      cls = 'off';
      // needs_relogin：refresh token 已被上游作废，解冻/等时间都救不回，只能重登。
      // 必须与「封禁 / 人工停用」在标签上分开，否则运维会去点解冻、或者干等它自己好。
      tag = s.needs_relogin
        ? '<span class="tag bad" title="refresh token 已被上游作废：revive 救不回，只能重新登录">待重登</span>'
        : '<span class="tag bad">已禁用</span>';
    }
    else if (s.paused) { cls = 'cool'; tag = '<span class="tag warn">已暂停选号</span>'; }
    else if (cool > 0) {
      cls = 'cool';
      const kind = bl > Math.max(s.cool_remaining_sec || 0, dg > 0 ? dg : 0) ? '熔断'
        : (dg > (s.cool_remaining_sec || 0) ? '连败降权' : (s.cool_kind === 'hard_credit' ? '积分冷却' : '限流冷却'));
      tag = '<span class="tag warn">' + kind + ' · ' + dur(cool) + '</span>';
    } else tag = '<span class="tag ok">可用</span>' + (s.in_flight ? '' : '');
    // RT 判死但 AT 还活着：号此刻仍可用（chat 不受影响），黄标提醒趁 AT 没过期赶紧重登——
    // AT 一过期这个号就真的不能用了。与「已禁用」是两个维度，所以单独一枚标签。
    let rtTag = '';
    if (!s.disabled && s.refresh_token_dead) {
      const expMs = parseAPITime(s.access_token_expires_at);
      const left = expMs ? Math.max(0, Math.round((expMs - Date.now()) / 1000)) : 0;
      rtTag = '<span class="tag warn" title="refresh token 已失效（无法续期）；access token 到期后只能重新登录">RT 已失效' +
        (left > 0 ? ' · AT 剩 ' + dur(left) : '') + '</span>';
    }
    const note = s.reason ? '<div class="sub-note">' + esc(s.reason) + '</div>' : '';
    const rateLimits = rateLimitRowsHtml(s.rate_limited_models, Date.now());
    const short = s.uid.length > 16 ? s.uid.slice(0, 16) + '…' : s.uid;
    // 企业版不限量：上游 limitNum == -1，网关以 credits_total=-1 透出（见 upstream
    // enterpriseUnlimitedTotal）。此时剩余额度不参与展示，直接标「不限」。
    const unlimited = s.credits_total === -1;
    const cred = unlimited ? '不限'
      : (s.credits == null ? '—' : (s.credits_total > 0 ? creditNum(s.credits) + '<span class="of">/' + creditNum(s.credits_total) + '</span>' : creditNum(s.credits)));
    const pct = unlimited ? 100
      : (s.credits_total > 0
        ? Math.min(100, Math.round((s.credits || 0) / s.credits_total * 100))
        : Math.round((s.credits || 0) / maxCred * 100));
    // 成本台账 tooltip（model_costs）：每模型实测单价（≤0 = 实测免费），运维据此
    // 看「为什么总选它」——免费号垄断 / 单价排序一眼可见。
    let credTip;
    if (unlimited) credTip = '企业版不限量（上游 limitNum=-1）';
    else if (s.credits_total > 0) {
      credTip = (s.enterprise ? '企业版剩余额度 ' : '剩余 ')
        + s.credits + ' / ' + (s.enterprise ? '分配 ' : '总额 ') + s.credits_total + '（' + pct + '%）';
    } else credTip = '积分（相对池内最高）';
    const costs = (s.model_costs || []).filter(c => c.model);
    if (costs.length) {
      credTip += '\n实测单价（credits/1K）：\n' + costs.map(c =>
        '  ' + c.model + '：' + (c.cost_per_1k <= 0 ? '免费' : c.cost_per_1k)).join('\n');
    }
    const frozen = s.disabled || cool > 0;
    // 操作按钮取舍：needs_relogin / RT 判死时主按钮是「重登」——解冻点了也没用
    // （凭证已经作废），摆在那是误导；其余号维持解冻/暂停的既有语义。
    const reloginBtn = (s.needs_relogin || s.refresh_token_dead)
      ? '<button class="xs primary" data-a="relogin" data-u="' + esc(s.uid) + '" title="重新扫码登录该账号：凭证就地更新，保留 device_token">重登</button>'
      : '';
    const freezeBtn = s.needs_relogin
      ? ''
      : (frozen ? '<button class="xs primary" data-a="revive" data-u="' + esc(s.uid) + '" title="解冻，恢复参与选号">解冻</button>'
                : (s.paused ? '<button class="xs primary" data-a="resume" data-u="' + esc(s.uid) + '" title="恢复选号">恢复</button>'
                            : '<button class="xs ghost" data-a="pause" data-u="' + esc(s.uid) + '" title="' + (s.enterprise ? '退出选号，但照常保活 / 刷新额度' : '暂停选号：退出选号，但照常签到 / 活跃上报 / 保活 / 刷新余额') + '">暂停</button>'));
    const tu = s.token_usage || {};
    const req = tu.request_count || 0;
    const totalTok = fmtTok(tu.total_tokens);
    const totalTokUnit = totalTok === '—' ? '' : '<em>tok</em>';
    const latency = fmtMs(tu.last_latency_ms);
    const rate = fmtRate(tu.last_tokens_per_second);
    const usageTitle = '最近一次：' + req + ' 次 / ' + totalTok + ' / 延迟 ' + latency + ' / ' + rate;
    return '<tr class="' + cls + '" title="uid: ' + esc(s.uid) + '">' +
      '<td class="mark" aria-hidden="true"><i></i></td>' +
      // 名字单独包一层 .nm-text：省略号只作用于名字，realm 徽标固定不收缩——
      // 否则长邮箱名会把徽标一起裁掉（2026-11-06 面板截图反馈）。
      '<td class="who"><div class="nm"><span class="nm-text" title="' + esc(s.nickname || '') + '">' + remarkName(s.uid, esc(dispName)) + '</span>' + (s.realm === 'global' ? '<span class="realm-tag">国际版</span>' : '') + (s.enterprise ? '<span class="realm-tag">企业版</span>' : '') + '</div><div class="id">' + esc(short) + '</div></td>' +
      '<td>' + tag + rtTag + note + rateLimits + '</td>' +
      '<td class="cred" title="' + esc(credTip) + '"><div class="n">' + cred + '</div><div class="bar"><i style="width:' + pct + '%"></i></div></td>' +
      '<td class="num">' + (s.success_count || 0) + ' <span class="sep-dim">/</span> ' + (Number(s.err_total) > 0 ? '<span class="txt-bad">' + s.err_total + '</span>' : '<span class="muted">0</span>') + '</td>' +
      '<td class="num">' + (s.in_flight || 0) + '</td>' +
      '<td class="num usage-cell" title="' + esc(usageTitle) + '"><span class="usage-line" aria-label="' + esc(usageTitle) + '">' +
        '<span class="usage-item usage-count"><b>' + req + '</b><em>次</em></span>' +
        '<span class="usage-item usage-total"><b>' + totalTok + '</b>' + totalTokUnit + '</span>' +
        '<span class="usage-item usage-latency' + (Number(tu.last_latency_ms) > 2000 ? ' slow' : '') + '"><b>' + latency + '</b></span>' +
        '<span class="usage-item usage-rate' + (Number(tu.last_tokens_per_second) > 0 && Number(tu.last_tokens_per_second) < 10 ? ' slow' : '') + '"><b>' + rate + '</b></span>' +
      '</span></td>' +
      '<td class="num muted">' + ago(s.last_success) + '</td>' +
      '<td class="acts">' +
        // 企业版无个人成长体系（签到 400「企业账号不支持该操作」/ 成长任务 403）：
        // 不渲染「签到」「任务」按钮，只留「额度」——点它走 /balance，企业额度由
        // upstream 的 get-enterprise-user-usage 口径填充。
        // v5：行内只留「签到 / 余额 / 暂停·恢复」三个高频动作，「任务」并入 ⋯ 菜单
        // （操作列 219 → 176px，把宽度让给账号名；企业版菜单里同样不出现任务项）。
        (s.enterprise ? '' :
          '<button class="xs ghost" data-a="checkin" data-u="' + esc(s.uid) + '"' + (s.checkin_done ? ' title="今日已签到；点击可重新签到并刷新余额"' : '') + '>' + (s.checkin_done ? '已签' : '签到') + '</button>') +
        '<button class="xs ghost" data-a="balance" data-u="' + esc(s.uid) + '"' + (s.enterprise ? ' title="刷新企业版已分配额度（上游 get-enterprise-user-usage）"' : '') + '>' + (s.enterprise ? '额度' : '余额') + '</button>' +
        reloginBtn + freezeBtn +
        '<button class="xs ghost more-btn" data-a="more" data-u="' + esc(s.uid) + '" aria-haspopup="menu" aria-expanded="false" title="更多操作：' + (s.enterprise ? '禁用 / 移除' : '任务 / 禁用 / 移除') + '">⋯</button>' +
      '</td></tr>';
  }).join('');
}

// renderModelLocks 模型锁池：哪些模型不能用、锁了几个号、还要锁多久。
// 后端 model_locks 已按「整池不可用 → 没号可用 → 部分限流」排好序，这里只做展示。
function renderModelLocks(rows) {
  const tb = $('mlBody');
  if (!tb) return;
  const note = $('mlNote');
  if (!rows || !rows.length) {
    tb.innerHTML = '<tr><td colspan="8"><div class="empty inline">当前没有模型级限流 —— 所有模型均可选</div></td></tr>';
    if (note) note.textContent = '';
    return;
  }
  const STATE = { locked: ['bad', '整池不可用'], starved: ['warn', '没号可用'], partial: ['warn', '部分限流'] };
  const left = iso => {
    const ms = parseAPITime(iso);
    return ms ? dur(Math.max(0, Math.round((ms - Date.now()) / 1000))) : '—';
  };
  tb.innerHTML = rows.map(r => {
    const st = STATE[r.state] || ['mute', r.state || '—'];
    const realm = r.realm === 'global' ? '国际版' : '国内版';
    return '<tr>' +
      '<td>' + esc(r.model) + '</td>' +
      '<td><span class="realm-tag">' + realm + '</span></td>' +
      '<td><span class="tag ' + st[0] + '">' + st[1] + '</span></td>' +
      '<td class="num">' + (r.servable || 0) + ' / ' + (r.total || 0) + '</td>' +
      '<td class="num">' + (r.locked || 0) + '</td>' +
      '<td class="num">' + left(r.unlock_at || r.fully_unlock_at) + '</td>' +
      '<td class="num">' + left(r.fully_unlock_at) + '</td>' +
      '<td>' + (r.reason ? '<div class="note">' + esc(r.reason) + '</div>' : '—') + '</td>' +
      '</tr>';
  }).join('');
  if (note) {
    const bad = rows.filter(r => r.state === 'locked' || r.state === 'starved').length;
    note.textContent = bad ? bad + ' 个模型整池不可用' : rows.length + ' 个模型部分限流';
  }
}

/* setStat 写入统计条数值。值为 0 时加 .zero 把颜色退成中性灰：
   一排彩色的 0（绿 0 / 红 0 / 黄 0）会把真正需要关注的数字淹没，
   而"0 个已禁用"本来就不是需要警觉的信息。 */
function setStat(id, v) {
  const el = $(id);
  if (!el) return;
  // 缺字段 / NaN 一律显示破折号：以前 undefined 会渲染成字面量 "undefined"、
  // NaN 会渲染成 "NaN"，再被当成真实统计读（plan P0-A3）。
  let shown = v;
  if (v == null || v === '') shown = '—';
  else if (typeof v === 'number' && !Number.isFinite(v)) shown = '—';
  el.textContent = shown;
  const head = String(shown).split('/')[0].trim();
  const n = Number(head);
  el.classList.toggle('zero', head !== '—' && head !== '-' && head !== '' && Number.isFinite(n) && n === 0);
  // 零值时连顶部语义色轨一起退成中性，否则数字灰了、轨道还在亮绿/亮红。
  el.closest('.stat, .kpi')?.classList.toggle('zero', head !== '—' && head !== '-' && head !== '' && Number.isFinite(n) && n === 0);
}

async function loadOverview(quiet) {
  try {
    const d = await api('overview');
    overviewData = d;
    setStat('sTotal', d.total);
    setStat('sHealthy', d.healthy);
    setStat('sCooling', d.cooling);
    setStat('sDisabled', d.disabled);
    // 暂停选号单列（issue #125）：它只关选号、照常签到保活，与禁用是两种状态。
    if ($('sPaused')) setStat('sPaused', d.paused);
    // 企业版不限量账号的 credits_total = -1（上游 limitNum=-1），不是真实额度：
    // 算进「总积分」会把分母做小、剩余率算错（plan P0-A3）。
    const accts = d.accounts || [];
    const remSum = accts.reduce((a, s) => a + (Number(s.credits) || 0), 0);
    const totSum = accts.reduce((a, s) => a + (Number(s.credits_total) > 0 ? Number(s.credits_total) : 0), 0);
    setStat('sCredits', totSum > 0 ? creditNum(remSum) + ' / ' + creditNum(totSum) : (accts.length ? creditNum(remSum) : '—'));
    const healthySub = $('sHealthySub');
    if (healthySub) {
      healthySub.textContent = Number(d.total) > 0
        ? '可用率 ' + (Number(d.healthy || 0) / Number(d.total) * 100).toFixed(1) + '%'
        : '可用率 —';
    }
    const creditsSub = $('sCreditsSub');
    if (creditsSub) {
      if (totSum > 0) {
        const pct = Math.max(0, Math.min(100, remSum / totSum * 100));
        creditsSub.textContent = '剩余 ' + pct.toFixed(1) + '% · 已用 ' + creditNum(Math.max(0, totSum - remSum));
      } else {
        creditsSub.textContent = accts.length ? '未返回总额度' : '—';
      }
    }
    setStat('sSticky', d.sticky_sessions);
    $('navSub').textContent = d.version ? 'v' + d.version : 'v—';
    $('navRedis').textContent = d.redis_mode === 'upstash' ? 'Redis 镜像' : '本地内存';
    // 缺 total 说明响应不完整（旧版本/代理截断）：不要当成「待添加账号」去误导用户。
    const known = Number.isFinite(Number(d.total));
    $('navState').textContent = !known ? '数据异常' : (d.healthy > 0 ? '服务正常' : (Number(d.total) > 0 ? '无可用账号' : '待添加账号'));
    const p = $('navPulse');
    p.className = 'pulse' + (!known ? ' bad' : (d.healthy > 0 ? '' : (Number(d.total) > 0 ? ' warn' : ' bad')));
    $('accNote').textContent = [
      d.in_flight_full ? d.in_flight_full + ' 个账号在途占满' : '',
      // 待重登单独提示：这是唯一「程序救不了、必须人工扫码」的状态，混在禁用数里
      // 看不出该做什么。
      Number(d.needs_relogin) > 0 ? d.needs_relogin + ' 个待重登（需重新登录）' : '',
    ].filter(Boolean).join(' · ');
    const up = Number(d.uptime_sec);
    const metaText = Number.isFinite(up)
      ? '运行 ' + (up >= 86400 ? Math.floor(up / 86400) + ' 天 ' : '') + Math.floor(up % 86400 / 3600) + ' 时 ' + Math.floor(up % 3600 / 60) + ' 分'
      : '';
    $('subMeta').textContent = metaText;
    // 没有副标题时把前面的「/」分隔符一起收掉，别在标题右边留一个孤零零的斜杠。
    const sep = $('subMeta').previousElementSibling;
    if (sep && sep.classList.contains('sep')) sep.hidden = !metaText;
    renderAccounts(d.accounts || []);
    renderModelLocks(d.model_locks);
    // 到期卡片跟随首页统计同步刷新（用同一份响应里的按日批次，不额外发请求）。
    // 2 分钟内的实时明细最新且更全，保留；过期后由 state 快照接管，避免点过一次
    // 「检查」之后卡片永远停在那一刻的旧数据。
    const liveFresh = lastPackages && (Date.now() - lastPackagesAt) < EXP_FRESH_MS;
    if ($('expBox') && !liveFresh) renderExpirySnapshot(d);
    markScrollables();
  } catch (e) {
    // 轮询失败不能装没事：顶栏状态切到「连接中断」，下一次成功会自动恢复。
    $('navState').textContent = '连接中断';
    $('navPulse').className = 'pulse warn';
    if (!quiet) toast(e.message, 'err');
  }
}

/* runAccountAction 统一执行单个账号的操作：确认 → busy → 请求 → toast → 刷新。
   行内按钮与「⋯」菜单共用这一条路径，避免两套 confirm / busy 逻辑分叉。 */
async function runAccountAction(a, u, triggerBtn) {
  if (a === 'remove' && !(await askConfirm({
    title: '移除账号', confirmText: '移除', danger: true,
    body: '移除账号将删除池状态与 auths/ 下的凭证文件，且不可恢复。\n确认移除？'
  }))) return;
  if (a === 'disable' && !(await askConfirm({
    title: '禁用账号', confirmText: '禁用', danger: true,
    body: '禁用后该账号不再参与选号（保号任务默认也跳过），需手动解冻才能恢复。\n若只是想临时让位、仍要保号，请改用「暂停选号」。'
  }))) return;
  const done = setBusy(triggerBtn);
  try {
    if (a === 'checkin') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/checkin', { method: 'POST' });
      toast('签到完成' + (r.credits != null ? '，积分 ' + r.credits + (r.credits_total > 0 ? '/' + r.credits_total : '') : '') + (r.checkin_message ? '（' + r.checkin_message + '）' : ''), 'ok');
    } else if (a === 'balance') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/balance', { method: 'POST' });
      toast('余额已更新：' + r.credits + (r.credits_total > 0 ? ' / ' + r.credits_total : ''), 'ok');
    } else if (a === 'revive') {
      await api('accounts/' + encodeURIComponent(u) + '/revive', { method: 'POST' });
      toast('已解冻', 'ok');
    } else if (a === 'disable') {
      await api('accounts/' + encodeURIComponent(u) + '/disable', { method: 'POST' });
      toast('已禁用', 'ok');
    } else if (a === 'pause') {
      await api('accounts/' + encodeURIComponent(u) + '/pause', { method: 'POST' });
      toast('已暂停选号（签到 / 保活照常）', 'ok');
    } else if (a === 'resume') {
      await api('accounts/' + encodeURIComponent(u) + '/resume', { method: 'POST' });
      toast('已恢复选号', 'ok');
    } else if (a === 'tasks') {
      openTasks(u);
    } else if (a === 'refresh') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/refresh', { method: 'POST' });
      toast(r.message || (r.ok ? '刷新成功' : '刷新失败'), r.ok ? 'ok' : 'err');
    } else if (a === 'remove') {
      let r;
      try {
        r = await api('accounts/' + encodeURIComponent(u) + '/remove', { method: 'POST' });
      } catch (e) {
        // 后端对「凭证看起来仍有效」的号要求 force=1（删除不可逆）。这里给一次
        // 显式确认，而不是把 409 文案原样丢给用户。
        if (!/force=1/.test(e.message || '')) throw e;
        if (!(await askConfirm({
          title: '强制移除？', confirmText: '仍然移除', danger: true,
          body: '该账号的凭证看起来仍有效（未禁用、也未判 RT 失效）。移除会永久删除 auths/ 下的凭证文件，只能重新扫码恢复。'
        }))) return;
        r = await api('accounts/' + encodeURIComponent(u) + '/remove?force=1', { method: 'POST' });
      }
      toast(r.file_error ? '已移除（凭证文件删除失败：' + r.file_error + '）' : '已移除', 'ok');
    }
  } catch (e) { toast(e.message, 'err'); }
  finally { done(); loadOverview(true); }
}

/* openAccountMenu 行内「更多」：把禁用/移除从 6 个按钮压成 1 个，账号表在最窄的
   桌面档也能不横滚。controls.js 不可用时退回原生 confirm，功能不丢。 */
function openAccountMenu(btn, uid) {
  const s = ((overviewData && overviewData.accounts) || []).find(x => x.uid === uid) || {};
  if (window.WBControls && typeof WBControls.menu === 'function') {
    const items = [];
    // v5：「任务」从行内按钮收进这里。企业版没有个人成长体系（成长任务 403），
    // 菜单里也不能出现该项，否则点开永远是错误提示。
    if (!s.enterprise) items.push({ label: '成长任务', onPick: () => runAccountAction('tasks', uid, btn) });
    // 刷新令牌：手动问一次上游，立刻知道「还能不能自愈」（回包带 needs_relogin）。
    items.push({ label: '刷新令牌', onPick: () => runAccountAction('refresh', uid, btn) });
    if (s.needs_relogin || s.refresh_token_dead) {
      items.push({ label: '重新登录', onPick: () => openRelogin(uid) });
    }
    if (!s.disabled) items.push({ label: '禁用账号', danger: true, onPick: () => runAccountAction('disable', uid, btn) });
    items.push({ label: '移除账号', danger: true, onPick: () => runAccountAction('remove', uid, btn) });
    WBControls.menu(btn, items);
    return;
  }
  if (window.confirm('移除账号将删除池状态与凭证文件，且不可恢复。确认移除？')) {
    runAccountAction('remove', uid, btn);
  }
}

$('accBody').addEventListener('click', async ev => {
  const b = ev.target.closest('button[data-a]');
  if (!b) return;
  const u = b.dataset.u, a = b.dataset.a;
  if (a === 'add') { openAdd(); return; }
  if (a === 'relogin') { openRelogin(u); return; }
  if (a === 'more') { openAccountMenu(b, u); return; }
  await runAccountAction(a, u, b);
});

$('btnCheckinAll').onclick = async () => {
  try {
    const d = await api('checkin_all', { method: 'POST' });
    const skipped = d && d.started === false;
    toast(skipped ? '上一轮签到仍在执行，本次已跳过' : '全部签到已开始，结果见日志', skipped ? '' : 'ok');
  }
  catch (e) { toast(e.message, 'err'); }
};
$('btnKeepaliveAll').onclick = async () => {
  try {
    const d = await api('keepalive_all', { method: 'POST' });
    const skipped = d && d.started === false;
    toast(skipped ? '上一轮保活仍在执行，本次已跳过' : '全部保活已开始，结果见日志', skipped ? '' : 'ok');
  }
  catch (e) { toast(e.message, 'err'); }
};
$('btnTravelAll').onclick = async () => {
  try {
    const d = await api('travel_all', { method: 'POST' });
    const skipped = d && d.started === false;
    toast(skipped ? '上一轮旅行巡检仍在执行，本次已跳过' : '旅行巡检已开始（含领养链路），结果见日志', skipped ? '' : 'ok');
  }
  catch (e) { toast(e.message, 'err'); }
};
$('btnActivityAll').onclick = async () => {
  try {
    const d = await api('activity_all', { method: 'POST' });
    const skipped = d && d.started === false;
    toast(skipped ? '上一轮活跃上报仍在执行，本次已跳过' : '活跃上报已开始，结果见日志', skipped ? '' : 'ok');
  }
  catch (e) { toast(e.message, 'err'); }
};

/* ── 模型 ─────────────────────────────────────────────────────────── */
/* 实测上限标注：scripts/probe_max_tokens.py --panel-out 写入探测结果，
   /panel/api/model_probes 只读透传。探测键带域前缀（cn:glm-5.2），模型表
   显示裸名，按「精确命中或 :后缀」关联。无数据时本列退回上游声称值。 */
function fmtK(n) { n = Number(n || 0); return n >= 1000 ? Math.round(n / 1000) + 'K' : String(n); }
function probeDays(ts) {
  if (!ts) return null;
  const t = new Date(String(ts).replace(' ', 'T'));
  const d = (Date.now() - t.getTime()) / 86400000;
  return isNaN(d) ? null : Math.floor(d);
}
function outCell(m, pr) {
  if (!pr) return '<td class="num">' + (m.max_output_tokens ? fmtK(m.max_output_tokens) : '—') + '</td>';
  const tip = '声称 ' + (pr.claimed ? fmtK(pr.claimed) : '?') + ' · 实测 ' + (pr.measured ? fmtK(pr.measured) : '?') +
    (pr.note ? ' · ' + pr.note : '') + (pr.tested_at ? ' · 探测于 ' + pr.tested_at : '');
  const days = probeDays(pr.tested_at);
  const stale = days !== null && days > 30 ? ' · ' + days + ' 天前' : '';
  if (pr.verdict === 'clamped' && pr.measured) {
    if (pr.claimed && pr.measured < pr.claimed) {
      const x = pr.claimed / pr.measured;
      const xs = (x >= 10 ? Math.round(x) : Math.round(x * 10) / 10) + '×';
      return '<td class="num" title="' + esc(tip) + '"><span class="txt-warn fw-600">' +
        fmtK(pr.measured) + ' ⚠</span><div class="note">钳制 ' + xs + stale + '</div></td>';
    }
    return '<td class="num" title="' + esc(tip) + '"><span class="txt-ok">' + fmtK(pr.measured) +
      (pr.claimed && pr.measured > pr.claimed ? ' ↑' : ' ✓') + '</span></td>';
  }
  if (pr.verdict === 'at_least' && pr.measured)
    return '<td class="num" title="' + esc(tip) + '"><span class="muted">≥' + fmtK(pr.measured) + '</span></td>';
  return '<td class="num" title="' + esc(tip) + '"><span class="muted">?</span><div class="note">未测出' + stale + '</div></td>';
}

/* rateCell 倍率列：牌价 vs 生效价。上游 credits 是牌价（转正后基准倍率），
   modelPromotions 给当前生效折扣（限时免费 factor=0 / 夜间五折 0.5 等）——
   WorkBuddy 客户端显示的正是生效价。有折扣：生效价大字 + 标签 + 划线牌价，
   悬停带时段说明；无 factor 只有标签（错峰类）：牌价 + 标签。 */
function rateCell(m) {
  const tipText = m.promo_note || (m.credits_reference ? '上游未下发实时倍率，显示 2026-11-05 官方快照参考价' : '');
  const tip = tipText ? ' title="' + esc(tipText) + '"' : '';
  if (m.promo_factor != null && m.promo_credits) {
    const base = m.credits ? ' <s class="price-was">' + esc(m.credits) + '</s>' : '';
    const label = m.promo_label ? ' <span class="tag ok">' + esc(m.promo_label) + '</span>' : '';
    return '<span class="help"' + tip + '><b>' + esc(m.promo_credits) + '</b>' + label + base + '</span>';
  }
  if (m.promo_label) {
    return '<span class="help"' + tip + '>' + (m.credits ? esc(m.credits) : '—') +
      ' <span class="tag warn">' + esc(m.promo_label) + '</span></span>';
  }
  return m.credits ? '<span class="help"' + tip + '>' + esc(m.credits) + '</span>' : '—';
}

async function loadModels() {
  const tb = $('mdBody');
  tb.innerHTML = '<tr><td colspan="7"><div class="empty inline">正在向上游查询…</div></td></tr>';
  try {
    // 探测、模型锁与最近请求都是本地可选增强：任一失败不影响模型目录本身。
    // 可用性标签不额外发推理请求，只用模型锁和最近真实请求结果合成。
    const [d, pr, st, rm] = await Promise.all([
      api('models'),
      api('model_probes').catch(() => ({})),
      // 模型锁取自 overview（/panel/api/status 并不存在，早前这里 404 被 catch 吞掉，
      // 结果"模型与档位"页的限流可用性标签永远是空——账号池页的模型锁池却是好的）。
      api('overview').catch(() => ({})),
      api('request_metrics').catch(() => ({})),
    ]);
    mdAll = d.models || [];
    mdProbes = pr.probes || {};
    mdLocks = Array.isArray(st.model_locks) ? st.model_locks : [];
    mdRecent = Array.isArray(rm.recent) ? rm.recent : [];
    if (!mdAll.length) {
      tb.innerHTML = '<tr><td colspan="7"><div class="empty inline">上游未返回模型</div></td></tr>';
      $('mdCount').textContent = '';
      $('mdNote').textContent = '上游未返回模型';
      return;
    }
    // 探测契约键历史上是 realm:bare（cn:glm-5.2）；models.id_style=slash 后模型
    // id 变成 cn/glm-5.2，统一归一化，避免实测标注凭空消失。
    mdProbeByKey = {};
    for (const [k, v] of Object.entries(mdProbes)) {
      const s = mdSplitId(k);
      mdProbeByKey[s.realm + ':' + s.bare] = v;
    }
    mdProbeOf = m => mdProbeByKey[mdProbeKey(m)] || mdProbes[mdModelBare(m)] || undefined;
    const hit = mdAll.filter(m => mdProbeOf(m)).length;
    // models.hidden 在服务端已过滤（与 /v1/models 同口径），这里只把条数说清楚，
    // 否则"少了一堆模型"会被读成上游目录缩水。
    const hiddenN = Number(d.hidden_count) || 0;
    $('mdNote').textContent = mdAll.length + ' 个模型 · 目录已缓存'
      + (hit ? ' · ' + hit + ' 个有实测上限' : '')
      + (hiddenN ? ' · 已按 models.hidden 隐藏 ' + hiddenN + ' 个' : '');
    mdVisible = 20;
    renderModels();
  } catch (e) {
    mdAll = [];
    tb.innerHTML = '<tr><td colspan="7"><div class="empty inline">' + esc(shortErr(e.message)) + '</div></td></tr>';
    $('mdCount').textContent = '';
    // 头部说明也要改：否则表格里写着失败，标题还挂着"实时查询上游"，读起来像在查。
    $('mdNote').textContent = '查询失败';
  }
}

/* ── 模型筛选（按条件查询）───────────────────────────────────────────
   模型目录一次拉全（几十条），筛选与排序全部在前端完成：改条件零延迟，且不会
   因为调一次筛选就打一次上游——/panel/api/models 是直连上游的实时查询，很贵。
   条件之间是 AND；每个条件为空即不参与判定。 */
// mdRateValue 当前生效的积分倍率数值：优先促销价（限时免费 = 0），无倍率记为
// Infinity 排到最后（排序时"没有价格"不该冒充最便宜）。
function mdRateValue(m) {
  const raw = (m.promo_credits != null && m.promo_credits !== '') ? m.promo_credits : m.credits;
  const n = parseFloat(String(raw == null ? '' : raw).replace(/[^\d.]/g, ''));
  return Number.isFinite(n) ? n : Infinity;
}

// mdSearchText 参与关键字搜索的字段（ID / 展示名 / 厂商 / 描述 / 标签）。
function mdSearchText(m) {
  return [m.id, m.name, m.vendor, m.description, (m.tags || []).join(' ')]
    .filter(Boolean).join(' ').toLowerCase();
}

// mdSplitId 拆出 {realm, bare}：兼容 cn:x / cn/x / 裸名（裸名 realm 为空串）。
// 面板 API 现在同时下发 realm/model 字段，但旧 fixture 与历史数据只带 id，
// 因此这里始终保留按 ID 解析的兜底；两种写法都解析，换 id_style 不受影响。
function mdSplitId(id) {
  const s = String(id || '');
  const m = s.match(/^(cn|global)[:\/]/);
  return m ? { realm: m[1], bare: s.slice(m[0].length) } : { realm: '', bare: s };
}
function mdModelRealm(m) { return (m && m.realm) || mdSplitId(m && m.id).realm; }
function mdModelBare(m) { return (m && m.model) || mdSplitId(m && m.id).bare; }
function mdProbeKey(m) { return mdModelRealm(m) + ':' + mdModelBare(m); }

// mdMatch 单个模型是否满足全部筛选条件。
function mdMatch(m, f) {
  f = f || mdFilter;
  if (f.q) {
    const text = mdSearchText(m);
    // 空格分词后逐个匹配：多关键词是 AND，便于"cn 视觉"这类组合查询。
    for (const kw of f.q.toLowerCase().split(/\s+/).filter(Boolean)) {
      if (!text.includes(kw)) return false;
    }
  }
  if (f.realm && mdModelRealm(m) !== f.realm) return false;
  if (f.cap === 'tool' && !m.supports_tool_call) return false;
  if (f.cap === 'vision' && !m.supports_images) return false;
  if (f.cap === 'reasoning' && !m.supports_reasoning) return false;
  if (f.cap === 'default' && !m.is_default) return false;
  if (f.effort === 'off') {
    if (!m.can_disable_thinking) return false;
  } else if (f.effort && !(m.supported_efforts || []).includes(f.effort)) {
    return false;
  }
  const factor = m.promo_factor == null ? null : Number(m.promo_factor);
  if (f.promo === 'promo' && factor == null && !m.promo_label) return false;
  if (f.promo === 'free' && !(factor === 0)) return false;
  if (f.promo === 'discount' && !(factor != null && factor > 0)) return false;
  return true;
}

// mdSortList 按当前排序条件返回新数组（不改动入参，保持上游原始顺序可回溯）。
function mdSortList(list, f) {
  f = f || mdFilter;
  const out = list.slice();
  const num = v => { const n = Number(v || 0); return Number.isFinite(n) ? n : 0; };
  if (f.sort === 'rate') out.sort((a, b) => mdRateValue(a) - mdRateValue(b));
  else if (f.sort === 'context') out.sort((a, b) => num(b.context_length) - num(a.context_length));
  else if (f.sort === 'output') out.sort((a, b) => num(b.max_output_tokens) - num(a.max_output_tokens));
  else if (f.sort === 'name') out.sort((a, b) => String(a.id || '').localeCompare(String(b.id || '')));
  return out;
}

// mdAvailabilityOf 合成 Global 模型当前可用性：模型锁优先于最近请求结果。
// 只返回有真实本地证据的状态；目录中存在但从未请求过的模型标为“待验证”，
// 避免把“上游列出了模型”误当成“当前账号有权限”。
function mdAvailabilityOf(m, locks, recent) {
  if (mdModelRealm(m) !== 'global') return null;
  const bare = mdModelBare(m);
  locks = Array.isArray(locks) ? locks : mdLocks;
  recent = Array.isArray(recent) ? recent : mdRecent;
  const lock = locks.find(r => r && r.realm === 'global' && r.model === bare);
  if (lock) {
    const reason = String(lock.reason || '').trim();
    const lower = reason.toLowerCase();
    if (lock.state === 'partial') {
      return { key: 'partial', label: '部分可用', cls: 'warn',
        tip: '仍有 ' + Number(lock.servable || 0) + ' 个 global 账号可服务；已锁 ' + Number(lock.locked || 0) + ' 个' + (reason ? '；' + reason : '') };
    }
    if (lock.state === 'starved') {
      return { key: 'starved', label: '账号暂不可用', cls: 'mute',
        tip: '没有账号能服务该模型，但并非该模型自身被锁；请检查账号冷却/在途占用' };
    }
    if (lower.startsWith('11158') || lower.includes('not allowed for user type')) {
      return { key: 'denied', label: '无权限', cls: 'bad',
        tip: '当前 global 账号类型不支持该模型' + (reason ? '；' + reason : '') };
    }
    if (lower.startsWith('11102') || lower.includes('service info not found')) {
      return { key: 'missing', label: '上游无此模型', cls: 'bad',
        tip: '上游目录残留，实际请求返回模型不存在' + (reason ? '；' + reason : '') };
    }
    return { key: 'limited', label: '限流/不可用', cls: 'warn',
      tip: '模型级冷却中' + (reason ? '；' + reason : '') };
  }
  const ev = recent.find(e => {
    if (!e || e.model == null) return false;
    return mdModelRealm({ id: e.model }) === 'global' && mdModelBare({ id: e.model }) === bare;
  });
  if (ev) {
    const status = Number(ev.status || 0);
    const when = ev.time ? new Date(ev.time).toLocaleString('zh-CN', { hour12: false }) : '';
    if (ev.outcome === 'success' || (status >= 200 && status < 300)) {
      return { key: 'ok', label: '可用', cls: 'ok',
        tip: '最近一次真实请求成功' + (when ? ' · ' + when : '') };
    }
    if (status === 429) {
      return { key: 'rate', label: '限流', cls: 'warn',
        tip: '最近一次请求被限流' + (when ? ' · ' + when : '') };
    }
    if (status >= 500) {
      return { key: 'upstream', label: '上游错误', cls: 'bad',
        tip: '最近一次请求上游错误 HTTP ' + status + (when ? ' · ' + when : '') };
    }
    return { key: 'failed', label: '最近失败', cls: 'bad',
      tip: '最近一次请求失败 HTTP ' + (status || '—') + (when ? ' · ' + when : '') };
  }
  return { key: 'unknown', label: '待验证', cls: 'mute',
    tip: '尚无模型锁或近期真实请求记录；目录存在不代表当前账号有权限' };
}

function mdAvailabilityTag(m) {
  const av = mdAvailabilityOf(m);
  if (!av) return '';
  return ' <span class="tag ' + av.cls + ' md-avail" title="' + esc(av.tip) + '">' + esc(av.label) + '</span>';
}

// mdRowHtml 单个模型行（纯渲染，便于独立测试）。
function mdRowHtml(m, pr) {
  const eff = (m.supported_efforts || []).slice();
  if (m.can_disable_thinking && eff.length && !eff.includes('off')) eff.push('off（可关）');
  // 档位胶囊用中性色：50 行 × 每行 2-4 个 amber 会把整张表染成一片黄，
  // 反而看不出重点。真正需要跳出来的是"默认档"（绿）与"思考常开"（琥珀）。
  const effs = eff.length ? eff.map(e => '<span class="tag mute">' + esc(e) + '</span>').join(' ')
    : '<span class="muted">' + (m.supports_reasoning ? '固定档 · 默认 ' + esc(m.default_effort || '?') : '不支持思考') + '</span>';
  // 能力徽标：默认模型 / 工具调用 / 视觉 / 纯推理（上游目录全字段透出，缺失不显示）
  const caps = [];
  if (m.is_default) caps.push('<span class="tag ok">默认</span>');
  if (m.supports_tool_call) caps.push('<span class="tag mute">工具</span>');
  if (m.supports_images) caps.push('<span class="tag mute">视觉</span>');
  if (m.supports_reasoning && !m.can_disable_thinking) caps.push('<span class="tag warn">思考常开</span>');
  const capHtml = caps.length ? caps.join(' ') : '';
  const tip = m.description ? ' title="' + esc(m.description) + '"' : '';
  return '<tr><td class="mark" aria-hidden="true"><i></i></td><td class="who"' + tip + '><div class="nm"><span class="mid">' + esc(m.id) + '</span>' + mdAvailabilityTag(m) + '</div>' +
    '<div class="model-sub"><span class="id">' + esc(m.name || '') + '</span>' + capHtml + '</div></td>' +
    '<td class="num">' + rateCell(m) + '</td>' +
    '<td>' + (m.default_effort ? '<span class="tag ok">' + esc(m.default_effort) + '</span>' : '<span class="muted">—</span>') + '</td>' +
    '<td class="efs">' + effs + '</td>' +
    '<td class="num">' + (m.context_length ? Math.round(m.context_length / 1000) + 'K' : '—') + '</td>' +
    outCell(m, pr) + '</tr>';
}

function renderModels() {
  const tb = $('mdBody');
  const list = mdSortList(mdAll.filter(m => mdMatch(m)));
  const mobile = matchMedia('(max-width: 1000px)').matches;
  const shown = mobile ? list.slice(0, mdVisible) : list;
  if (!list.length) {
    tb.innerHTML = '<tr><td colspan="7"><div class="empty inline">没有符合当前筛选条件的模型</div></td></tr>';
  } else {
    tb.innerHTML = shown.map(m => mdRowHtml(m, mdProbeOf(m))).join('');
  }
  const filtered = list.length !== mdAll.length;
  $('mdCount').textContent = !mdAll.length ? ''
    : filtered ? '命中 ' + list.length + ' / ' + mdAll.length + ' 个模型'
    : mdAll.length + ' 个模型';
  $('mdCount').className = filtered ? 'note src-off' : 'note';
  const more = $('btnMdMore');
  if (more) {
    const left = list.length - shown.length;
    more.hidden = !(mobile && left > 0);
    more.textContent = left > 0 ? '显示更多 20 个（剩余 ' + left + ' 个）' : '显示更多';
  }
}

function resetModelFilter() {
  mdFilter = { q: '', realm: '', cap: '', effort: '', promo: '', sort: 'default' };
  mdVisible = 20;
  $('mdQ').value = ''; $('mdRealm').value = ''; $('mdCap').value = '';
  $('mdEffort').value = ''; $('mdPromo').value = ''; $('mdSort').value = 'default';
  renderModels();
}

// 筛选控件：输入框防抖 120ms（长列表逐字符重排不必每键一次），下拉即时。
let mdQTimer = null;
$('mdQ').oninput = () => {
  clearTimeout(mdQTimer);
  mdQTimer = setTimeout(() => { mdFilter.q = $('mdQ').value.trim(); mdVisible = 20; renderModels(); }, 120);
};
for (const [id, key] of [['mdRealm', 'realm'], ['mdCap', 'cap'], ['mdEffort', 'effort'], ['mdPromo', 'promo'], ['mdSort', 'sort']]) {
  const el = $(id);
  if (!el) continue;
  el.onchange = () => { mdFilter[key] = el.value; mdVisible = 20; renderModels(); };
}
$('mdReset').onclick = resetModelFilter;
$('btnModels').onclick = loadModels;
if ($('btnMdMore')) $('btnMdMore').onclick = () => { mdVisible += 20; renderModels(); };

/* ── 日志（频道：全部/任务/对话/系统） ─────────────────────────────── */
let logCh = 'all';
let logTab = 'req';
const LOG_TAB_MQ = matchMedia('(max-width: 1000px)');
function applyLogTab(tab) {
  if (tab) logTab = tab;
  document.querySelectorAll('#logTabs button').forEach(b => {
    const on = b.dataset.logtab === logTab;
    b.classList.toggle('on', on);
    b.setAttribute('aria-pressed', on ? 'true' : 'false');
  });
  const mobile = LOG_TAB_MQ.matches;
  document.querySelectorAll('#view-logs .log-sec').forEach(sec => {
    sec.hidden = mobile && sec.dataset.logsec !== logTab;
  });
}
if ($('logTabs')) $('logTabs').addEventListener('click', ev => {
  const b = ev.target.closest('button[data-logtab]');
  if (b) applyLogTab(b.dataset.logtab);
});
if (LOG_TAB_MQ.addEventListener) LOG_TAB_MQ.addEventListener('change', () => applyLogTab());
else if (LOG_TAB_MQ.addListener) LOG_TAB_MQ.addListener(() => applyLogTab());
applyLogTab();
$('logChips').addEventListener('click', ev => {
  const b = ev.target.closest('button[data-ch]');
  if (!b) return;
  logCh = b.dataset.ch;
  document.querySelectorAll('#logChips .chip').forEach(c => { const on = c === b; c.classList.toggle('on', on); c.setAttribute('aria-pressed', on ? 'true' : 'false'); });
  loadLogs();
});
async function loadLogs() {
  const box = $('logBox');
  reqVisible = 20;
  const atEnd = box.scrollTop + box.clientHeight >= box.scrollHeight - 24;
  const limit = ($('reqLimit') && $('reqLimit').value) || 100;
  // 时间范围由归档侧过滤（不是前端筛已拉取的条目）：区间落在更早的时间段时，
  // 「最近 N 条」里根本不会有那些记录，必须让服务端按时间取。
  const rq = trangeQuery('reqRange', false);
  rq.set('limit', limit);
  try {
    const [d, metrics, requestRows, credit] = await Promise.all([
      api('logs'),
      api('request_metrics').catch(() => ({})),
      api('request_logs?' + rq.toString()).catch(() => ({ entries: [] })),
      api('credit_history?limit=' + creditLimitValue()).catch(() => null),
    ]);
    // 归档开启时以归档为准——「区间内没有记录」是一个真实结果，不能回落成内存里
    // 的最近 100 条（那会把筛选条件之外、时间范围之外的请求显示出来）。
    // 只有归档关闭时才回落到内存指标，保证没有归档的部署仍能看到最近请求。
    const archiveOn = !!(metrics && metrics.archive && metrics.archive.enabled);
    const recent = archiveOn ? (requestRows.entries || []) : (metrics.recent || []);
    renderRequestMetrics(metrics, recent);
    const entries = (d.entries || []).filter(e => logCh === 'all' || e.ch === logCh);
    box.innerHTML = entries.length
      ? entries.map(e => {
        const lvl = /error|失败|错误/.test(e.text) ? ' e' : /warn|冷却|熔断/.test(e.text) ? ' w' : '';
        const t = e.ts ? fmtStampShort(e.ts) : '';
        const ch = logCh === 'all' ? '<i class="lch c-' + esc(e.ch) + '">' + ({ task: '任务', chat: '对话', sys: '系统' }[e.ch] || e.ch) + '</i>' : '';
        return '<span class="ln' + lvl + '">' + ch +
          '<time>' + esc(t) + '</time><span class="lmsg">' + esc(e.text) + '</span></span>';
      }).join('')
      : '<span class="muted">暂无日志</span>';
    if (logPin && atEnd) box.scrollTop = box.scrollHeight;
    const counts = {};
    for (const e of (d.entries || [])) counts[e.ch] = (counts[e.ch] || 0) + 1;
    $('logNote').textContent = logCh === 'all'
      ? '任务 ' + (counts.task || 0) + ' · 对话 ' + (counts.chat || 0) + ' · 系统 ' + (counts.sys || 0)
      : (logCh === 'task' ? '任务' : logCh === 'chat' ? '对话' : '系统') + ' ' + entries.length + ' 行';
    renderCreditHistory(credit);
  } catch (e) { /* 概览已提示 */ }
  applyLogTab();
}

function renderRequestMetrics(m, entries) {
  m = m || {};
  const a = m.archive || {};
  // 指标是**本次进程启动以来**的内存计数，而下面的请求记录读自 JSONL 归档
  // （含重启前的历史）。不写清楚的话，"已完成 0" 与几十行记录并排出现，
  // 看起来像统计坏了。
  $('reqSummary').textContent =
    '本次启动以来 · 已完成 ' + fmtTok(m.completed) +
    ' · 成功 ' + (m.success_rate == null ? '—' : Number(m.success_rate).toFixed(1) + '%') +
    ' · HTTP ' + (m.http_success_rate == null ? '—' : Number(m.http_success_rate).toFixed(1) + '%') +
    ' · 平均 ' + fmtMs(m.avg_duration_ms) +
    ' · 进行中 ' + String(m.in_flight || 0);
  $('reqNote').textContent = a.enabled
    ? 'JSONL 归档 ' + fmtBytes(a.bytes) + (a.dropped_writes ? ' · 丢弃 ' + a.dropped_writes + ' 条' : '') +
      (a.last_error ? ' · 错误：' + a.last_error : '')
    : '仅内存指标，JSONL 归档已关闭';

  reqEntries = entries || [];
  renderRequestTable();
}

/* ── 积分历史（每次真实查到余额与上次比对，变动即留痕）─────────────────
   纯格式化函数集中在此，便于前端测试按切片断言；变动列 +N / −N（U+2212，
   与请求记录一致），余额用千分位。 */
function creditNum(n) {
  return Number(n || 0).toLocaleString('zh-CN');
}
function creditDeltaText(delta) {
  const d = Number(delta || 0);
  if (d > 0) return '+' + creditNum(d);
  if (d < 0) return '−' + creditNum(-d);
  return '0';
}
function creditTimeText(e) {
  if (!e || !e.time) return '—';
  const d = new Date(e.time);
  // 正常链路 time 由 Go 的 time.Time 序列化，恒为 RFC3339；这里挡的是手工改文件/
  // 将来换格式等脏数据——toLocaleTimeString 遇到非法日期不抛异常，只会渲染出
  // "Invalid Date" 这种没意义的字样。
  return isNaN(d.getTime()) ? '—' : fmtStampShort(e.time);
}
function creditAccountText(e) {
  e = e || {};
  return e.account ? String(e.account) : (e.uid || '—');
}
function creditBalanceText(e) {
  return creditNum(e && e.after);
}
function creditDescText(e) {
  return '余额 ' + creditNum(e && e.before) + ' → ' + creditNum(e && e.after);
}
function creditEntryText(e) {
  return creditTimeText(e) + ' | ' + creditAccountText(e) + ' | ' + creditDeltaText(e && e.delta) +
    ' | ' + creditBalanceText(e) + ' | ' + creditDescText(e);
}
function creditHistoryNote(entries) {
  entries = entries || [];
  if (!entries.length) return '暂无积分变动记录';
  let net = 0;
  for (const e of entries) net += Number(e && e.delta || 0);
  return entries.length + ' 条 · 净 ' + (net > 0 ? '+' : net < 0 ? '−' : '') + creditNum(Math.abs(net));
}
function renderCreditHistory(d) {
  const body = $('creditBody'), note = $('creditNote');
  if (!body) return;
  if (!d) {
    if (note) note.textContent = '积分历史不可用';
    body.innerHTML = '<tr><td colspan="5"><div class="empty inline">积分历史不可用</div></td></tr>';
    return;
  }
  const entries = d.entries || [];
  if (note) note.textContent = creditHistoryNote(entries);
  body.innerHTML = entries.length
    ? entries.map(e => '<tr>' +
      '<td>' + esc(creditTimeText(e)) + '</td>' +
      '<td>' + esc(creditAccountText(e)) + '</td>' +
      '<td class="num">' + esc(creditDeltaText(e && e.delta)) + '</td>' +
      '<td class="num">' + esc(creditBalanceText(e)) + '</td>' +
      '<td>' + esc(creditDescText(e)) + '</td>' +
      '</tr>').join('')
    : '<tr><td colspan="5"><div class="empty inline">暂无积分变动记录</div></td></tr>';
}
function creditLimitValue() {
  const el = $('creditLimit');
  const n = el ? Number(el.value) : 100;
  return n === 300 || n === 1000 ? n : 100;
}
async function loadCreditHistory() {
  let d = null;
  try { d = await api('credit_history?limit=' + creditLimitValue()); } catch (e) { d = null; }
  renderCreditHistory(d);
}

/* reqMatch 请求记录筛选：q 对 IP/UA/模型/账号/请求 ID 做空格分词的 AND 包含匹配，
   outcome 精确匹配。两者都在已拉取的条目上做（最多 1000 条），不发新请求。 */
function reqMatch(e, f) {
  f = f || reqFilter;
  if (f.outcome && String(e && e.outcome || '') !== f.outcome) return false;
  if (f.q) {
    const text = [e && e.client_ip, e && e.user_agent, e && e.model, e && e.account, e && e.request_id]
      .filter(Boolean).join(' ').toLowerCase();
    for (const kw of f.q.toLowerCase().split(/\s+/).filter(Boolean)) {
      if (!text.includes(kw)) return false;
    }
  }
  return true;
}

function reqOutcomeTag(e) {
  const outcome = String(e && e.outcome || '');
  const label = { success: '成功', http_error: 'HTTP 错误', stream_error: '流错误', interrupted: '中断' }[outcome] || outcome || '—';
  const cls = outcome === 'success' ? 'ok'
    : outcome === 'interrupted' ? 'warn'
    : outcome ? 'bad' : 'mute';
  return '<span class="tag ' + cls + '">' + esc(String(e && e.status || '—') + ' ' + label) + '</span>';
}

function reqTokenCell(e) {
  const total = Number(e && e.total_tokens || 0) ||
    (Number(e && e.prompt_tokens || 0) + Number(e && e.completion_tokens || 0));
  return total ? fmtTok(total) : '—';
}

function reqCreditCell(e) {
  if (!e || !e.credit_known) return '<span class="muted">—</span>';
  const v = Number(e.credit);
  return Number.isFinite(v) ? trimFixed(v.toFixed(2)) : '<span class="muted">—</span>';
}

/* renderRequestTable 渲染请求记录表。来源列是这一版的重点：IP 用等宽字体方便扫，
   UA 单行截断（完整值在 title 里，行本身用 requestLogText 作 tooltip）。 */
function renderRequestTable() {
  const filteredList = reqEntries.filter(e => reqMatch(e));
  const mobile = matchMedia('(max-width: 1000px)').matches;
  const list = mobile ? filteredList.slice(0, reqVisible) : filteredList;
  const tb = $('reqBody');
  if (!tb) return;
  tb.innerHTML = list.map(e => {
    const when = e && e.time ? fmtStampShort(e.time) : '—';
    const ip = e && e.client_ip ? e.client_ip : '';
    const ua = e && e.user_agent ? e.user_agent : '';
    const rid = e && e.request_id ? e.request_id : '';
    return '<tr title="' + esc(requestLogText(e)) + '">' +
      '<td class="num">' + esc(when) + '</td>' +
      '<td>' + reqOutcomeTag(e) + '</td>' +
      '<td>' + (e && e.model
        // 模型 ID 里有 `-` 和 `.`，auto 布局下会在连字符处折行（cn/deepseek- 换行
        // v4.1-flash），一行记录被撑成两行、扫表时对不齐。统一走 .clip：单行省略 +
        // 全量 title，与 IP / UA / 请求 ID 三列同一套规则。
        ? '<span class="clip" title="' + esc(e.model) + '">' + esc(e.model) + '</span>'
        : '<span class="muted">—</span>') + '</td>' +
      '<td>' + esc(e && e.account || '—') + '</td>' +
      '<td>' + (ip ? '<span class="clip ip" title="' + esc(ip) + '">' + esc(ip) + '</span>' : '<span class="muted">—</span>') + '</td>' +
      '<td>' + (ua ? '<span class="clip" title="' + esc(ua) + '">' + esc(ua) + '</span>' : '<span class="muted">—</span>') + '</td>' +
      '<td class="num">' + fmtMs(e && e.duration_ms) + '</td>' +
      '<td class="num">' + reqTokenCell(e) + '</td>' +
      '<td class="num">' + reqCreditCell(e) + '</td>' +
      '<td>' + (rid ? '<span class="clip rid" title="' + esc(rid) + '">' + esc(rid) + '</span>' : '<span class="muted">—</span>') + '</td>' +
      '</tr>';
  }).join('') || '<tr><td colspan="10"><div class="empty inline">' +
      (reqEntries.length ? '没有符合当前筛选条件的请求记录' : '暂无请求记录') + '</div></td></tr>';

  const filtered = filteredList.length !== reqEntries.length;
  // 归档里的旧条目没有来源字段（该功能上线前写入）：这时提示开关/历史原因，
  // 而不是让人以为筛选坏了。
  const hasSource = reqEntries.some(e => e && (e.client_ip || e.user_agent));
  $('reqCount').textContent = !reqEntries.length ? ''
    : (filtered ? '命中 ' + filteredList.length + ' / ' + reqEntries.length + ' 条' : reqEntries.length + ' 条') +
      (mobile && filteredList.length > list.length ? ' · 已显示 ' + list.length : '') +
      (hasSource ? '' : ' · 来源未记录');
  $('reqCount').className = (filtered || !hasSource) ? 'note src-off' : 'note';
  const more = $('btnReqMore');
  if (more) {
    const left = filteredList.length - list.length;
    more.hidden = !(mobile && left > 0);
    more.textContent = left > 0 ? '加载更多 20 条（剩余 ' + left + ' 条）' : '加载更多';
  }
}

/* 请求记录筛选控件。搜索框防抖 150ms：最多 1000 行重渲染，不必每键一次。
   这段顶层绑定放在 requestLogText 之前，是为了让"纯函数切片"式前端测试
   （slice requestLogText → fmtBytes）只拿到无副作用的格式化函数。 */
let reqQTimer = null;
if ($('reqQ')) $('reqQ').oninput = () => {
  clearTimeout(reqQTimer);
  reqQTimer = setTimeout(() => { reqFilter.q = $('reqQ').value.trim(); reqVisible = 20; renderRequestTable(); }, 150);
};
if ($('reqOutcome')) $('reqOutcome').onchange = () => {
  reqFilter.outcome = $('reqOutcome').value;
  reqVisible = 20;
  renderRequestTable();
};
if ($('btnReqMore')) $('btnReqMore').onclick = () => {
  reqVisible += 20;
  renderRequestTable();
};
if ($('reqLimit')) $('reqLimit').onchange = loadLogs;
if ($('btnReqReload')) $('btnReqReload').onclick = loadLogs;
// 时间范围：默认「全部历史」——请求记录页的历史行为就是"取最近 N 条"，
// 加一个默认收窄的区间会让打开页面时看到的条数凭空变少。
if ($('reqRange')) trangeBind('reqRange', loadLogs, '0');
if ($('creditLimit')) $('creditLimit').onchange = loadCreditHistory;
if ($('btnCreditReload')) $('btnCreditReload').onclick = loadCreditHistory;

function requestLogText(e) {
  const when = e && e.time ? fmtStampShort(e.time) : '—';
  const outcomeLabel = { success: '成功', http_error: 'HTTP 错误', stream_error: '流错误', interrupted: '中断' };
  const token = Number(e && e.total_tokens || 0) ||
    (Number(e && e.prompt_tokens || 0) + Number(e && e.completion_tokens || 0));
  let credit = 'credit —';
  if (e && e.credit_known) {
    const value = Number(e.credit);
    if (Number.isFinite(value)) credit = String(Number(value.toFixed(2))) + ' credit';
  }
  return [
    when,
    String(e && e.status || '—') + ' ' + (outcomeLabel[e && e.outcome] || (e && e.outcome) || '—'),
    e && e.model || '—',
    e && e.account || '—',
    e && e.client_ip || '—',
    e && e.user_agent || '—',
    fmtMs(e && e.duration_ms),
    fmtTok(token) + ' tok',
    credit,
    cacheRateText(e && e.cache_hit_tokens, e && e.cache_miss_tokens) === '—' ? '' : '命中 ' + cacheRateText(e && e.cache_hit_tokens, e && e.cache_miss_tokens),
    // 服务域 / 跨域回退：只有归档里存在该字段时才追加（旧归档与既有断言输出不变）。
    e && e.realm ? '域 ' + e.realm : '',
    e && e.fallback ? '跨域回退' : '',
    e && e.request_id || '—',
  ].filter(Boolean).join(' | ');
}

/* 缓存命中率纯文本（issue #92）：requestLogText 与积分表/kpi 卡共用。
   自包含（不依赖 trimFixed）：前端纯函数切片测试只截取本段。 */
function cacheRateText(hit, miss) {
  const h = Number(hit || 0), m = Number(miss || 0), total = h + m;
  if (!total) return '—';
  return String(Math.round(h / total * 1000) / 10) + '%';
}

function fmtBytes(bytes) {
  const n = Number(bytes || 0);
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
  return (n / 1024 / 1024).toFixed(1) + ' MB';
}
$('btnLogPin').onclick = () => {
  logPin = !logPin;
  $('btnLogPin').textContent = '自动滚动：' + (logPin ? '开' : '关');
};

/* ── 配置 ─────────────────────────────────────────────────────────── */
const CFG_MAP = {
  strategy_domestic: ['pool', 'strategy', 'domestic'],
  strategy_international: ['pool', 'strategy', 'international'],
  domestic_account_order: ['pool', 'account_order', 'domestic'],
  international_account_order: ['pool', 'account_order', 'international'],
  listen: ['listen'], api_key: ['api_key'],
  package_detail_limit: ['panel', 'package_detail_limit'],
  checkin_hours: ['schedule', 'checkin_hours'], checkin_enabled: ['schedule', 'checkin_enabled'], growth_hours: ['schedule', 'growth_hours'], growth_enabled: ['schedule', 'growth_enabled'],
  travel_hours: ['schedule', 'travel_hours'], travel_enabled: ['schedule', 'travel_enabled'],
  activity_hours: ['schedule', 'activity_hours'], activity_enabled: ['schedule', 'activity_enabled'],
  keepalive_hours: ['schedule', 'keepalive_hours'], keepalive_enabled: ['schedule', 'keepalive_enabled'],
  balance_refresh_enabled: ['schedule', 'balance_refresh_enabled'], balance_refresh_minutes: ['schedule', 'balance_refresh_minutes'],
  task_sweep_enabled: ['schedule', 'task_sweep_enabled'], task_sweep_minutes: ['schedule', 'task_sweep_minutes'],
  include_disabled_in_tasks: ['schedule', 'include_disabled_in_tasks'],
  max_in_flight: ['pool', 'max_in_flight'], max_in_flight_global: ['pool', 'max_in_flight_global'],
  batch_concurrency: ['pool', 'batch_concurrency'],
  breaker_threshold: ['pool', 'breaker_threshold'],
  degrade_threshold: ['pool', 'degrade_threshold'], degrade_cooldown: ['pool', 'degrade_cooldown'],
  degrade_cooldown_max: ['pool', 'degrade_cooldown_max'],
  cost_explore_interval: ['pool', 'cost_explore_interval'],
  credit_floor: ['pool', 'credit_floor'],
  prefer_expiring: ['pool', 'prefer_expiring'], expiring_soon: ['pool', 'expiring_soon'],
  cross_domain_fallback: ['pool', 'cross_domain_fallback'],
  cross_domain_model_fallback: ['pool', 'cross_domain_model_fallback'],
  soft_rate: ['cooldown', 'soft_rate'], soft_rate_max: ['cooldown', 'soft_rate_max'],
  breaker_cooldown: ['pool', 'breaker_cooldown'], breaker_cooldown_max: ['pool', 'breaker_cooldown_max'],
  idle_weight_per_hour: ['pool', 'idle_weight_per_hour'], idle_weight_max: ['pool', 'idle_weight_max'],
  ttl: ['session_sticky', 'ttl'],
  timeout_seconds: ['upstream', 'timeout_seconds'], header_timeout_seconds: ['upstream', 'header_timeout_seconds'],
  idle_timeout_seconds: ['upstream', 'idle_timeout_seconds'], user_agent: ['upstream', 'user_agent'],
  client_version: ['upstream', 'client_version'], cli_version: ['upstream', 'cli_version'],
  identity_mode: ['upstream', 'identity_mode'],
  jitter_minutes: ['schedule', 'jitter_minutes'],
  prompt_mode: ['prompt', 'mode'], prompt_file: ['prompt', 'file'],
  sanitize_blacklist_fingerprints: ['features', 'sanitize_blacklist_fingerprints'],
  system_as_user_fallback: ['features', 'system_as_user_fallback'],
  session_sticky_enabled: ['session_sticky', 'enabled'],
  request_client_info: ['logging', 'request_client_info'],
  id_style: ['models', 'id_style'], realm_name_prefix: ['models', 'realm_name_prefix'],
};
/* 「覆盖型」文本字段：空串本身是有意义的取值（= 回落到内置默认），必须照发。
 *
 * 其余文本字段保持「空 = 不下发」的既有语义——那是防误清空的保护，不是 bug：
 * 表单里某个框没填，通常意味着"没改"，把它当成"请清空"会静默抹掉配置。
 *
 * 但覆盖型字段正好相反：清空 = 明确要求回到默认。漏发它们会让面板显示"已保存"
 * 而值其实没变（issue #102 附带发现 2：user_agent 清空后 config.json 里仍是旧值）。
 *
 * 刻意不含 api_key：清空它 = 关闭整个鉴权，误触代价是网关变成无鉴权公开服务。
 * 该字段（以及提示文案"留空 = 不鉴权"与现状不符的问题）单独处理。
 */
const CLEARABLE_CFG = new Set(['user_agent', 'prompt_file']);

function dig(obj, path) { return path.reduce((o, k) => (o == null ? undefined : o[k]), obj); }
function put(obj, path, val) {
  let o = obj;
  for (let i = 0; i < path.length - 1; i++) { if (typeof o[path[i]] !== 'object' || o[path[i]] === null) o[path[i]] = {}; o = o[path[i]]; }
  o[path[path.length - 1]] = val;
}

/* setConfigReady 切换配置表单的「可编辑/可提交」状态。
   加载期与失败期一律禁用，只留「放弃修改」= 重试入口可用；cfgNote 常驻说明原因。 */
function cfgChangedPathsBetween(before, after) {
  const prev = before || {}, next = after || {};
  const norm = v => Array.isArray(v) ? v.join(',') : (v == null ? '' : String(v));
  const out = [];
  for (const path of Object.values(CFG_MAP)) {
    const a = dig(prev, path), b = dig(next, path);
    if (a === undefined && b === undefined) continue;
    if (norm(a) !== norm(b)) out.push(path.join('.'));
  }
  return out;
}
function syncCfgDirty() {
  if (!cfgReady || !cfgBaseline) return;
  const n = cfgChangedPathsBetween(cfgBaseline, collectConfig()).length;
  cfgDirty = n;
  const bar = $('cfgSavebar'), save = $('btnCfgSave'), note = $('cfgNote');
  if (bar) bar.hidden = n === 0;
  if (save) save.disabled = n === 0;
  if (note) note.textContent = n ? '已修改 ' + n + ' 项' : '';
}
function setConfigReady(ready, note, retryLabel) {
  cfgReady = !!ready;
  const f = $('cfgForm');
  if (f) {
    // 真实表单：DOM 查询；测试里的迷你表单桩只有 elements 映射，两条路径都支持。
    let fields = [];
    if (typeof f.querySelectorAll === 'function') {
      fields = [...f.querySelectorAll('input, select, textarea'),
                ...f.querySelectorAll('button:not(#btnCfgReload)')];
    } else if (f.elements) {
      fields = Object.values(f.elements).filter(Boolean);
    }
    fields.forEach(el => { el.disabled = !ready; });
    if (f.classList && f.classList.toggle) f.classList.toggle('cfg-loading', !ready);
    if (f.setAttribute) f.setAttribute('aria-busy', ready ? 'false' : 'true');
  }
  const save = $('btnCfgSave');
  if (save) save.disabled = !ready || cfgDirty === 0;
  const reload = $('btnCfgReload');
  if (reload) reload.textContent = (ready || !retryLabel) ? '放弃修改' : retryLabel;
  const noteEl = $('cfgNote');
  if (noteEl) noteEl.textContent = note || '';
  const bar = $('cfgSavebar');
  if (!ready) {
    // 加载期不显示操作条；读取失败时保留「重试读取」入口。
    cfgDirty = 0;
    if (bar) bar.hidden = !retryLabel;
  } else {
    syncCfgDirty();
  }
}

async function loadConfig() {
  setConfigReady(false, '正在读取配置…');
  try {
    const d = await api('config');
    // 响应形状校验：只有 200 但没有 config（网关截断/版本不匹配/脚本注入的桩）
    // 时，dig() 会把 undefined 摊平成空值，表单看起来"加载成功"却全是空的。
    // 必须当成失败处理，否则同样会走「空表单保存覆盖线上配置」。
    if (!d || !d.config || typeof d.config !== 'object') throw new Error('配置响应缺少 config 字段');
    cfgLoaded = d.config;
    $('cfgPath').textContent = d.path || '';
    const f = $('cfgForm');
    for (const [name, path] of Object.entries(CFG_MAP)) {
      const el = f.elements[name];
      if (!el) continue;
      const v = dig(cfgLoaded, path);
      if (el.type === 'checkbox') el.checked = !!v;
      else if (Array.isArray(v)) el.value = v.join(', ');
      else el.value = v == null ? (name.startsWith('strategy_') ? 'weighted_random' : '') : v;
    }
    markDurationFields(); // 回填后重置校验态（清掉残留红框；现值来自后端必然合法）
    cfgBaseline = collectConfig();
    setConfigReady(true, '');
  } catch (e) {
    // 读不到配置绝不能保持"可编辑"：此时表单里全是 HTML 默认值，一提交就会把它们
    // 当成用户修改写回服务端。锁死表单，并把「放弃修改」变成重试入口。
    cfgLoaded = null;
    cfgBaseline = null;
    setConfigReady(false, '读取配置失败：' + shortErr(e.message), '重试读取');
    toast('读取配置失败：' + e.message, 'err');
  }
}
/* cfgChangedPaths 对比「表单将要提交的值」与「上次从服务端读到的值」，
   返回真正发生变化的字段路径（"listen" / "upstream.timeout_seconds" 这类）。
   用途：保存后只对**确实改过**的装配期字段提示"需重启"。 */
function cfgChangedPaths(submitted) {
  const prev = cfgLoaded || {};
  // 数组按逗号拼平、其余一律转字符串：表单里 "9, 21" 与 JSON 的 [9,21] 是同一个值。
  const norm = v => Array.isArray(v) ? v.join(',') : (v == null ? '' : String(v));
  const out = [];
  for (const path of Object.values(CFG_MAP)) {
    const after = dig(submitted, path);
    if (after === undefined) continue;   // 空值不下发 = 视为未改动（见 CLEARABLE_CFG 注释）
    if (norm(dig(prev, path)) !== norm(after)) out.push(path.join('.'));
  }
  return out;
}
function collectConfig() {
  const f = $('cfgForm'), out = {};
  for (const [name, path] of Object.entries(CFG_MAP)) {
    const el = f.elements[name];
    if (!el) continue;
    let v;
    if (el.type === 'checkbox') v = el.checked;
    else if (el.type === 'number') { v = el.value.trim() === '' ? undefined : Number(el.value); }
    else {
      const raw = el.value.trim();
      // 覆盖型字段空串照发（见 CLEARABLE_CFG）；其余空 = 不下发。
      if (name.endsWith('_account_order')) v = raw.split(/[,，\s]+/).filter(Boolean);
      else if (raw === '') v = CLEARABLE_CFG.has(name) ? '' : undefined;
      else if (name.endsWith('_hours')) v = raw.split(/[,，\s]+/).filter(Boolean).map(Number);
      else v = raw;
    }
    if (v !== undefined) put(out, path, v);
  }
  return out;
}
/* Go 时长字段即时校验：空 = 沿用现值（collectConfig 跳过发送）；非空必须是
   ParseDuration 语法（30m / 2h / 600s / 1h30m，可组合可带小数）。与后端
   config.go normalize() 的 time.ParseDuration 同口径，脏值在前端就地标红，
   不再等到保存被拒。 */
const DURATION_RE = /^(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$/;
const DURATION_FIELDS = ['soft_rate', 'soft_rate_max', 'breaker_cooldown', 'breaker_cooldown_max',
  'degrade_cooldown', 'degrade_cooldown_max', 'cost_explore_interval', 'expiring_soon', 'ttl'];
const DURATION_TIP = '格式应为 Go 时长：30m / 2h / 600s / 1h30m';
function durationBad(name) {
  const el = $('cfgForm').elements[name];
  if (!el) return false;
  const v = el.value.trim();
  return v !== '' && !DURATION_RE.test(v);
}
function markDurationFields() {
  for (const name of DURATION_FIELDS) {
    const el = $('cfgForm').elements[name];
    if (!el) continue;
    const bad = durationBad(name);
    el.classList.toggle('invalid', bad);
    el.title = bad ? DURATION_TIP : '';
  }
}
$('cfgForm').addEventListener('input', ev => {
  if (DURATION_FIELDS.includes(ev.target.name)) markDurationFields();
  syncCfgDirty();
});
$('cfgForm').addEventListener('change', syncCfgDirty);
$('btnEye').onclick = () => {
  const el = $('cfgKey');
  const show = el.type === 'password';
  el.type = show ? 'text' : 'password';
  $('btnEye').textContent = show ? '隐藏' : '显示';
};
$('btnCfgReload').onclick = async () => {
  if (cfgDirty && !(await askConfirm({
    title: '放弃未保存修改',
    confirmText: '放弃修改',
    body: '当前有 ' + cfgDirty + ' 项修改尚未保存，继续将丢弃这些修改并重新读取配置。'
  }))) return;
  loadConfig();
};
addEventListener('beforeunload', ev => {
  if (!cfgDirty) return;
  ev.preventDefault();
  ev.returnValue = '';
});
$('cfgForm').onsubmit = async ev => {
  ev.preventDefault();
  /* 硬门禁：配置没回填成功时，表单里的值全是 HTML 默认值，提交等于清空线上配置。
     按钮已 disabled，这里再兜一层（回车隐式提交 / 脚本触发都能拦住）。 */
  if (!cfgReady) { toast('配置尚未加载完成，请稍候', 'err'); return; }
  // 时长字段脏值拦截：标红 + toast 点名，不发保存请求（后端同样会拒，这里前置）。
  markDurationFields();
  const firstBad = DURATION_FIELDS.find(durationBad);
  if (firstBad) {
    const el = $('cfgForm').elements[firstBad];
    el.focus();
    toast('「' + (el.closest('.fld')?.querySelector('.lb')?.textContent || firstBad) + '」' + DURATION_TIP, 'err');
    return;
  }
  const btn = $('btnCfgSave');
  // 先算出"这次真正改了哪些字段"：后端返回的 restart_required 是**恒定的完整清单**
  // （它只说明哪些字段属于装配期依赖），拿它的长度当"本次改了几项"会误导——
  // 什么都没改点一下保存也会显示"14 项需重启"。
  const submitted = collectConfig();
  const changed = cfgChangedPaths(submitted);
  const done = setBusy(btn, '保存中…');
  try {
    const r = await api('config', { method: 'POST', body: JSON.stringify(submitted) });
    const restart = r.restart_required || [];
    const needRestart = changed.filter(p => restart.some(k => k === p || p.startsWith(k + '.')));
    if (!changed.length) toast('配置没有改动', '');
    else if (!needRestart.length) toast('配置已保存并立即生效', 'ok');
    else toast('配置已保存，其中 ' + needRestart.length + ' 项需重启进程生效：' + needRestart.join('、'), 'ok');
    // 密钥可能已改：本次会话沿用新值，避免下一次轮询被 401。
    const k = $('cfgKey').value.trim();
    if (k) localStorage.setItem(LS_KEY, k);
    loadConfig();
    loadOverview(true);
  } catch (e) { toast('保存失败：' + e.message, 'err'); }
  finally { done('保存配置'); }
};

/* ── 添加账号 ─────────────────────────────────────────────────────── */
function openAdd() {
  reloginUID = null;
  const t = $('addTitle'); if (t) t.textContent = '添加账号';
  const tabs = $('addTabs'); if (tabs) tabs.hidden = false;
  openVeil('addVeil');
  // 重置到登录标签
  switchAddTab('login');
  $('addPick').hidden = false;
  $('addLoad').hidden = true; $('addReady').hidden = true;
  $('addDone').hidden = true; $('addErr').hidden = true;
  $('importDone').hidden = true; $('importErr').hidden = true;
  $('btnCopyUrl').hidden = true; $('btnOpenUrl').hidden = true;
  $('btnStartLogin').hidden = false; $('btnStartLogin').disabled = false;
  stopPoll();
}
/* openRelogin 单号重登：复用添加账号弹窗的设备授权流程，差别只在「目标已定」——
   realm 由账号决定（选错域会把国际版号写成 cn，下一发请求就 401），并且不允许
   切到导入标签（那条路会绕开 uid 校验）。 */
function openRelogin(uid) {
  openAdd();
  reloginUID = uid;
  const t = $('addTitle'); if (t) t.textContent = '重登账号';
  const tabs = $('addTabs'); if (tabs) tabs.hidden = true;
  // realm 跟账号走：不展示选择器，直接从账号的 realm 发起。
  $('addPick').hidden = true;
  $('addErr').hidden = true;
  startAddLogin();
}
function switchAddTab(tab) {
  document.querySelectorAll('#addTabs .tab').forEach(b => b.classList.toggle('on', b.dataset.tab === tab));
  $('addTabLogin').hidden = tab !== 'login';
  $('addTabImport').hidden = tab !== 'import';
}
document.querySelectorAll('#addTabs .tab').forEach(b => {
  b.onclick = () => switchAddTab(b.dataset.tab);
});
function startAddLogin() {
  const realm = (document.querySelector('input[name="addRealm"]:checked') || {}).value || 'cn';
  const relogin = !!reloginUID;
  const url = relogin
    ? 'accounts/' + encodeURIComponent(reloginUID) + '/relogin/start'
    : 'login/start';
  const body = relogin ? '{}' : JSON.stringify({ realm });
  $('btnStartLogin').disabled = true;
  $('addLoad').hidden = false; $('addErr').hidden = true;
  api(url, { method: 'POST', body }).then(r => {
    loginState = r.state;
    $('addUrl').textContent = r.url;
    $('addPick').hidden = true; // 选域锁定（会话已按该域发起）
    $('addLoad').hidden = true; $('addReady').hidden = false;
    $('btnStartLogin').hidden = true;
    $('btnCopyUrl').hidden = false; $('btnOpenUrl').hidden = false;
    loginTimer = setInterval(pollLogin, 3000);
  }).catch(e => {
    $('addLoad').hidden = true;
    $('btnStartLogin').disabled = false;
    $('addErr').hidden = false;
    $('addErr').textContent = shortErr(e.message);
  });
}
function stopPoll() { if (loginTimer) { clearInterval(loginTimer); loginTimer = null; } }
async function pollLogin() {
  if (!loginState) return;
  try {
    const r = await api('login/poll?state=' + encodeURIComponent(loginState));
    if (r.done) {
      stopPoll();
      $('addReady').hidden = true;
      $('addDone').hidden = false;
      $('addDone').textContent = r.relogin
        ? '已重登 ' + (r.nickname || r.uid) + (r.realm === 'global' ? '（国际版）' : '')
          + (r.credits >= 0 ? ' · 积分 ' + r.credits + (r.credits_total > 0 ? '/' + r.credits_total : '') : '')
          + '，凭证已就地更新，账号已回到池中'
        : '已添加 ' + (r.nickname || r.uid) + (r.realm === 'global' ? '（国际版）' : '') + (r.credits >= 0 ? ' · 积分 ' + r.credits + (r.credits_total > 0 ? '/' + r.credits_total : '') : '') + '，账号已载入池中';
      setTimeout(() => { closeAdd(); loadOverview(true); }, 1600);
    }
  } catch (e) {
    stopPoll();
    $('addReady').hidden = true;
    $('addErr').hidden = false;
    $('addErr').textContent = e.message + '（关闭后重新添加）';
  }
}
function closeAdd() { stopPoll(); loginState = null; reloginUID = null; closeVeil('addVeil'); }
$('btnCloseAdd').onclick = closeAdd;
$('btnStartLogin').onclick = startAddLogin;
$('btnOpenUrl').onclick = () => open($('addUrl').textContent, '_blank');
$('btnCopyUrl').onclick = () => copyText($('addUrl').textContent)
  .then(() => toast('链接已复制', 'ok'), () => toast('复制失败，请手动选择复制', 'err'));
$('importFile').onchange = async () => {
  const file = $('importFile').files[0];
  if (!file) return;
  $('importDone').hidden = true; $('importErr').hidden = true;
  const fd = new FormData();
  fd.append('file', file);
  const h = {};
  const k = localStorage.getItem(LS_KEY);
  if (k) h['Authorization'] = 'Bearer ' + k;
  try {
    const r = await fetch('/panel/api/import/cockpit', { method: 'POST', body: fd, headers: h });
    const d = await r.json();
    if (!r.ok) throw new Error(d.error || ('HTTP ' + r.status));
    $('importDone').hidden = false;
    $('importDone').textContent = '导入完成：成功 ' + d.imported + ' 个' + (d.skipped ? '，跳过 ' + d.skipped + ' 个' : '');
    if (d.errors && d.errors.length) {
      console.warn('import errors:', d.errors);
    }
    loadOverview(true);
  } catch (e) {
    $('importErr').hidden = false;
    $('importErr').textContent = '导入失败：' + shortErr(e.message);
  }
  $('importFile').value = '';
};

/* ── 顶部动作 ─────────────────────────────────────────────────────── */
$('btnAdd').onclick = openAdd;
$('btnRefresh').onclick = async () => {
  const b = $('btnRefresh');
  const done = setBusy(b, '刷新中…');
  try {
    await api('balance_all', { method: 'POST' });
    await loadOverview(true);
    toast('余额已从上游刷新', 'ok');
  } catch (e) { toast('刷新失败：' + e.message, 'err'); await loadOverview(true); }
  finally { done('刷新'); }
  if (view === 'logs') loadLogs();
};

/* ── 官方补偿（issue #189）─────────────────────────────────────────
   「查状态 → 待领才领」。

   取数与呈现刻意分开：
     - 取数只有手动触发（进页面一次 + 「检查」/「领取」后）。补偿状态没有
       overview 快照可吃，必须逐号打上游，挂进 5s 轮询就是每 5 秒轰炸上游。
     - 呈现走 compCache 的本地重渲染（refreshVisible 每轮调一次），只为让
       倒计时走动 —— 不发任何请求。
   之前这里有个「5 分钟新鲜窗口」分支，但它唯一的非强制调用点是 start()，
   而那里 compCache 必为 null，所以该分支永远不会命中：注释宣称「防轰炸」，
   实际是「根本不刷新」。现已删除，别再加回来。

   声明位置在本段（轮询区之前）是必需的：start() 会调用 loadCompensation()，
   若这些 let 写在 start() 之后，顶层求值会在 TDZ 上炸——
   TestAppJSTopLevelSmoke 正是为这类问题设的闸门。 */
let compCache = null;
let compInFlight = null;   // 在途请求的 promise（非布尔标志：布尔标志配 force 刷新会静默空转）

// compPending 挑出「可领未领」的账号。
// 纯函数：判据独立于渲染，node 侧单测单独钉住。
// 判据与后端 upstream.CompensationStatus.Pending() 对齐——两端各留一份实现是
// 有意为之（前端要在渲染前先算总数），但**必须同步**，改一处就要改另一处。
function compPending(rows) {
  return (rows || []).filter(r => r && !r.skip && !r.error && r.status &&
    r.status.active && !r.status.claimed && (r.status.credit_num || 0) > 0);
}

// compErrored 挑出「查询失败」的账号。
// 单独拎出来，是因为它们**必须显形**：上游 5xx / token 过期时所有账号都会落到
// 这里，若和「没有补偿」一起被过滤掉，卡片就会整块消失，用户看到的是「什么都没有」
// ——而真相是「一个都没查成」。这正是本 PR 要消灭的静默失败。
function compErrored(rows) {
  return (rows || []).filter(r => r && r.error);
}

// compTotalPending 待领总额。
function compTotalPending(rows) {
  return compPending(rows).reduce((s, r) => s + (r.status.credit_num || 0), 0);
}

// compDeadlineText 领取截止倒计时文案。
// 上游 end_time 形如 "2026-10-22 23:59:59" 且**不带时区**，按浏览器本地时区解析
// （与后端积分到期的处理同口径）。urgent=true 表示距截止 <=48h，前端据此染色告警。
function compDeadlineText(endTime, now) {
  const raw = String(endTime == null ? '' : endTime).trim();
  if (!raw) return { text: '无截止信息', urgent: false };
  const t = new Date(raw.replace(' ', 'T')).getTime();
  if (!isFinite(t)) return { text: raw, urgent: false };
  const ms = t - (now || Date.now());
  if (ms <= 0) return { text: '已截止', urgent: true };
  const d = Math.floor(ms / 86400000);
  const h = Math.floor(ms % 86400000 / 3600000);
  const text = d > 0 ? (d + ' 天 ' + h + ' 小时') : (Math.max(1, h) + ' 小时');
  return { text: '剩 ' + text, urgent: ms <= 2 * 86400000 };
}

function renderCompensation(d) {
  const rows = (d && d.accounts) || [];
  const box = $('compBox');
  if (!box) return;
  const active = rows.filter(r => r && !r.error && r.status && r.status.active);
  const failed = compErrored(rows);
  // 「有失败就必须显形」——哪怕一条有效数据都没有。
  if (!active.length && !failed.length) { box.hidden = true; return; }
  box.hidden = false;

  const now = Date.now();
  const okRows = active.map(r => {
    const s = r.status || {};
    const done = !!s.claimed;
    const dl = compDeadlineText(s.end_time, now);
    const name = esc(r.nickname || String(r.uid).slice(0, 8));
    return '<div class="comp-row"><span title="' + name + '">' + name + '</span>' +
      (done
        ? '<span class="tag ok">已领取</span>'
        : '<span class="tag warn">待领取</span>' +
          '<span class="tag' + (dl.urgent ? ' bad' : ' mute') + '">' + esc(dl.text) + '</span>') +
      '<span class="tag mute">' + esc(fmtTok(s.credit_num || 0)) + ' 积分</span>' +
      (done ? '' : '<button class="xs" data-comp-uid="' + esc(r.uid) + '">领取</button>') +
      '</div>';
  }).join('');
  // err 文本来自上游响应体，是外部可控字符串，必须 esc。
  const errRows = failed.map(r =>
    '<div class="comp-row"><span>' + esc(r.nickname || String(r.uid).slice(0, 8)) + '</span>' +
    '<span class="tag bad">查询失败</span>' +
    '<span class="tag mute">' + esc(shortErr(r.error, 120)) + '</span></div>'
  ).join('');

  $('compList').innerHTML = okRows + errRows;

  const n = compPending(rows).length;
  let note = n ? (n + ' 个账号待领 · 共 ' + fmtTok(compTotalPending(rows)) + ' 积分') : '当前无待领补偿';
  // 混合态下也要把失败数说出来，否则 note 仍读作「没事」。
  if (failed.length) note += ' · ' + failed.length + ' 个账号查询失败';
  $('compNote').textContent = note;
}

// renderCachedCompensation 从缓存重渲染（不发请求），让倒计时走动。
// 由 refreshVisible 每轮调用；无缓存（还没查过）时什么都不做。
function renderCachedCompensation() {
  if (compCache) renderCompensation(compCache);
}

async function loadCompensation() {
  // 已有在途请求就等它跑完，而不是静默返回——领取后紧接着的刷新必须能看到新状态，
  // 否则用户点了「领取」但列表纹丝不动。
  if (compInFlight) { await compInFlight; return; }
  compInFlight = (async () => {
    try {
      const d = await api('compensation');
      compCache = d;
      renderCompensation(d);
    } catch (e) {
      const box = $('compBox');
      if (box) box.hidden = false;
      if ($('compList')) $('compList').innerHTML =
        '<div class="empty">查询失败：' + esc(shortErr(e.message)) + '</div>';
      if ($('compNote')) $('compNote').textContent = '';
    }
  })();
  try {
    await compInFlight;
  } finally {
    compInFlight = null;   // 必须 finally：否则一次异常就让此后所有刷新永久失效
  }
}

// 领取按钮走事件委托：列表每次重渲染都整块换 innerHTML，绑在具体节点上必丢。
if ($('compList')) $('compList').addEventListener('click', async e => {
  const b = e.target.closest('[data-comp-uid]');
  if (!b) return;
  const done = setBusy(b);
  try {
    const r = await api('accounts/' + encodeURIComponent(b.dataset.compUid) + '/compensation', { method: 'POST' });
    const res = r.result || {};
    if (res.claimed) toast('已领取 ' + fmtTok(res.credit || 0) + ' 积分', 'ok');
    else if (res.error) toast('查询失败，未能确认是否有补偿', 'err');
    else if (res.skip) toast(res.skip_msg || '无需领取', '');
    else toast('领取失败：未知结果', 'err');
    await loadCompensation();
    loadOverview(true);
  } catch (err) {
    toast('领取失败：' + err.message, 'err');
  } finally {
    done();   // 原先没有 finally：请求失败时按钮会一直灰着，直到列表重渲染
  }
});

if ($('btnCompRefresh')) $('btnCompRefresh').onclick = () => loadCompensation();

if ($('btnCompClaimAll')) $('btnCompClaimAll').onclick = async () => {
  const b = $('btnCompClaimAll');
  const done = setBusy(b);
  try {
    const r = await api('compensation_all', { method: 'POST' });
    const s = r.summary || {};
    // 顺序要紧：先看有没有账号没拿到结论，再看有没有领到。
    // 只按 claimed 判定的话，「全部查询失败」会被说成「当前没有待领补偿」，
    // 把故障说成了「没活动」——比不说更糟。
    if (s.errored) {
      toast(s.errored + ' 个账号查询失败，未能确认是否有补偿', 'err');
    } else if (s.claimed) {
      toast('已领取 ' + s.claimed + ' 个账号，共 ' + fmtTok(s.credit || 0) + ' 积分', 'ok');
    } else {
      toast('当前没有待领补偿', '');
    }
    await loadCompensation();
    loadOverview(true);
  } catch (e) {
    toast('领取失败：' + e.message, 'err');
  } finally {
    done();
  }
};

/* ── 轮询 ─────────────────────────────────────────────────────────── */
/* markScrollables 给「内容比容器宽」的表格加一条 sticky 提示：
   1025–1199 这类窄桌面档仍然需要横向滚动，但滚动条在触控板/隐藏滚动条下不可见，
   用户会以为最右边的操作列不存在。提示用 ::before 渲染，不额外造 DOM。 */
function markScrollables() {
  document.querySelectorAll('.tbl-wrap').forEach(w => {
    w.classList.toggle('scroll-x', w.scrollWidth > w.clientWidth + 4);
  });
}
addEventListener('resize', () => {
  clearTimeout(markScrollables._t);
  markScrollables._t = setTimeout(markScrollables, 150);
});

function refreshVisible() {
  if (view === 'accounts') { loadOverview(true); renderCachedCompensation(); }
  else if (view === 'logs') loadLogs();
  else if (view === 'taskscenter') reattachQueueView();
}
function start() {
  loadOverview(true);
  loadCompensation();                       // 补偿状态：首次进页面查一次（之后靠新鲜窗口）
  if (refTimer) clearInterval(refTimer);
  refTimer = setInterval(refreshVisible, 5000);
  checkAuthGate();
}
async function checkAuthGate() {
  try { await api('overview'); }
  catch (e) { if (String(e.message).includes('密钥') || String(e.message).includes('api_key')) return; }
}
start();

/* ── 积分任务 ─────────────────────────────────────────────────────── */
let taskUID = null;

// 可自动完成的任务（与后端 autoActions 表一致）：判据为行为事件、可经网关复现。
// 其余任务需在官方客户端内交互，面板只展示指引（行 title 提示）。
// 注意：键含点号（Model_chat_GLM5.2）必须加引号，否则会被解析成属性访问 + 数字字面量。
const AUTO_TASKS = {
  'chat_5': '上报 5 条对话活跃事件（自动补足差额）',
  'first_buddy': '上报解锁 → 同意协议 → 领取第一只 Buddy',
  'Model_chat_GLM5.2': '接受任务 → glm-5.2 真实对话一次 → 对齐模型上报',
  'RichMeow_Chat': '桌面指纹事件链上报（已验证可点亮）',
  'Buddy_App': '上报「进入 Buddy 应用」事件链（已验证可点亮）',
  'Buddy_App_QQ': '上报「进入企鹅教师助手」事件链（已验证可点亮）',
  'automation_1': '上报「定时任务创建」事件（已验证可点亮）',
  'Library_read': '上报「读资料库介绍」事件（已验证可点亮）',
  'template_5': '上报「使用模板创建任务」事件组 ×5（三账号实测点亮）',
  'playbook_prompt': '上报「灵感案例做同款发送 Prompt」事件组（三账号实测点亮）',
  'create_canvas': '上报「设计创意画布创建」事件组（三账号实测点亮，+300 分）',
  'expert_5': '真实专家召唤+使用链 ×5（专家市场+真实 chat，三账号实测点亮）',
  'Expert_team_use_3': '真实专家团召唤+使用链 ×3（三账号实测点亮）',
  'Hp_Appearance': '设置主题 API + 皮肤生效事件（两账号实测点亮）',
  'black_cat': '夜猫子：23:00–08:00 窗口内 glm-5.2 对话补足（窗口外提示等 23 点排程）',
  'Expert_lighthouse': '真实轻量云专家召唤+使用链（真实对话 requestId，两账号实测点亮）',
  'skill_1': '真实对话 + skill_info 技能加载事件（实测点亮）',
  'school_season': '校园日（小程序口径）：accept → mini 对话+activityId 上报 → 领奖（+100c+5e）',
  'Sequential_Tasks_1': '小程序首对话（小程序口径）：accept → mini 对话上报 → 领奖（+100c+5e）',
  'Sequential_Tasks_2': '小程序选专家对话（小程序口径）：市场专家 id → accept → expert_actual_use 上报 → 领奖（+200c+5e）',
  'Sequential_Tasks_3': '小程序五次对话（小程序口径）：accept → mini 对话上报 ×5（自动补差额）→ 领奖（+300c+5e）',
  'Sequential_Tasks_4': '小程序定时任务（预留，每日零点解锁一环）：accept → 定时任务创建事件 → 领奖（判据待解锁验证）',
  'Sequential_Tasks_5': '小程序使用 GLM5.2（预留）：accept → 带模型字段的 mini 对话上报 → 领奖（判据待解锁验证）',
  'Sequential_Tasks_6': '小程序十次对话（预留）：accept → mini 对话上报 ×target（自动补差额）→ 领奖',
  'Sequential_Tasks_7': '体验灵感功能（预留，疑 PC 口径）：accept → 灵感事件组（PC+mp 双形态）→ 领奖（判据待解锁验证）',
  'expert_use_maidehao': '体验「中国电商运营专家」：真实召唤 + 一次真实对话 + expert_actual_use 上报',
  'expert_use_douyinyin': '体验「抖音策略师」：真实召唤 + 一次真实对话 + expert_actual_use 上报',
  'expert_use_shangjia_xuanchuan': '体验「商家宣传页与海报设计师」：真实召唤 + 一次真实对话 + expert_actual_use 上报',
  'expert_use_bowangwang': '体验「直播电商教练」：真实召唤 + 一次真实对话 + expert_actual_use 上报',
  'WorkBuddy_Valorant': '体验「无畏契约拼豆专家」：按名查专家市场 → 真实召唤 + 对话 + 上报'
};

function openTasks(uid) {
  taskUID = uid;
  $('taskWho').textContent = shortUid(uid);
  $('taskWho').title = uid;
  openVeil('taskVeil');
  $('btnTaskReload').hidden = false;
  loadTasks();
}
function closeTasks() { closeVeil('taskVeil'); taskUID = null; }
$('btnCloseTask').onclick = closeTasks;
$('btnTaskReload').onclick = loadTasks;

// 全部接受：把该账号未接受的任务一次性报名（幂等，跳过已接受/已领取）。
$('btnTaskAcceptAll').onclick = async () => {
  if (!taskUID) return;
  const btn = $('btnTaskAcceptAll');
  const done = setBusy(btn, '接受中…');
  try {
    const r = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks/accept_all', { method: 'POST' });
    const n = r.accepted || 0;
    if (r.failed && r.failed.length) {
      toast(`已接受 ${n} 个，${r.failed.length} 个被上游拒绝（可重试）`, 'err');
    } else {
      toast(n ? `已接受 ${n} 个任务` : (r.message || '所有任务均已接受'), 'ok');
    }
  } catch (e) { toast(e.message, 'err'); }
  finally { done('全部接受'); loadTasks(); }
};

// 一键完成全部可自动任务（耗时较长：含真实对话，逐项回读验证）。
$('btnTaskAutoAll').onclick = async () => {
  if (!taskUID) return;
  const btn = $('btnTaskAutoAll');
  if (!(await askConfirm({
    title: '一键完成可自动任务', confirmText: '开始执行',
    body: '将依次执行：补报对话事件、领取 Buddy、glm-5.2 对话、尝试上报。\n过程约 1–2 分钟（含真实对话），确认继续？'
  }))) return;
  const done = setBusy(btn, '执行中…');
  try {
    const r = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks/auto_all', { method: 'POST' });
    const okN = (r.results || []).filter(x => x.status === 'done').length;
    const skipN = (r.results || []).filter(x => x.status === 'skipped').length;
    const errN = (r.results || []).filter(x => x.status === 'error').length;
    toast(`执行完成：成功 ${okN} 项，跳过 ${skipN} 项${errN ? '，失败 ' + errN + ' 项' : ''}`, errN ? 'err' : 'ok');
    console.log('auto_all results:', r.results);
  } catch (e) { toast(e.message, 'err'); }
  finally { done('一键完成可自动任务'); loadTasks(); }
};

async function loadTasks() {
  if (!taskUID) return;
  const st = $('taskState'), tb = $('taskTable');
  st.hidden = false;
  st.className = 'state';
  st.innerHTML = '<span class="dots">查询中</span>';
  tb.hidden = true;
  try {
    const d = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks');
    const list = d.tasks || [];
    if (!list.length) {
      st.className = 'state';
      st.textContent = '该账号暂无任务';
      return;
    }
    // 有进度或可领取的排前面，已领取沉底——一眼看到"现在该做什么"。
    list.sort((a, b) => (a.claimed - b.claimed) || (b.claimable - a.claimable) || String(a.task_code).localeCompare(String(b.task_code)));
    $('taskBody').innerHTML = list.map(t => {
      // 进度：current 可能缺失（0 或被上游省略）——用 ?? 兜底，避免渲染成 "undefined / N"
      const cur = t.current ?? 0, tgt = t.target ?? 0;
      const prog = tgt ? cur + ' / ' + tgt : (tgt === 0 && cur > 0 ? String(cur) : '—');
      const parts = [];
      // 「+300 分 +8 能 Buddy」这种缩写既看不出单位也看不出对象：补全成
      // 「+300 积分 · +8 能量 · Buddy」，分隔符用中点而不是空格。
      if (t.credit) parts.push('+' + t.credit + ' 积分');
      if (t.energy) parts.push('+' + t.energy + ' 能量');
      if (t.reward_buddy) parts.push('Buddy');
      const reward = parts.length ? parts.join(' · ') : '—';
      const badge = t.claimed ? '<span class="tag ok">已领取</span>'
        : t.claimable ? '<span class="tag warn">可领取</span>'
        : t.locked ? '<span class="tag mute">未解锁</span>'
        : t.accept_status === 'accepted' ? '<span class="tag mute">进行中</span>'
        : '<span class="tag mute">未接受</span>';
      const acted = t.claimed || t.locked ? ''
        : t.claimable ? '<button class="xs primary" data-t="claim" data-c="' + esc(t.task_code) + '">领取</button>'
        : AUTO_TASKS[t.task_code] ? '<button class="xs primary" data-t="auto" data-c="' + esc(t.task_code) + '" title="' + esc(AUTO_TASKS[t.task_code]) + '">一键完成</button>'
        : t.accept_status === 'accepted' ? ''
        : '<button class="xs" data-t="accept" data-c="' + esc(t.task_code) + '">接受</button>';
      // 操作指引（description/task_desc）挂 title 提示：如何完成交给用户看
      const tip = [t.title, t.task_desc || t.description, t.jump_url ? '跳转：' + t.jump_url : ''].filter(Boolean).join('\n');
      return '<tr title="' + esc(tip) + '"><td class="mark" aria-hidden="true"><i></i></td>' +
        '<td class="who"><div class="nm">' + esc(t.title || t.task_code) + '</div><div class="id">' + esc(t.task_code) + (t.tag ? ' · ' + esc(t.tag) : '') + '</div></td>' +
        '<td class="num">' + esc(prog) + '</td>' +
        '<td class="num">' + esc(reward) + '</td>' +
        '<td>' + badge + '</td>' +
        '<td class="acts">' + acted + '</td></tr>';
    }).join('');
    st.hidden = true;
    tb.hidden = false;
  } catch (e) {
    st.className = 'state err';
    st.textContent = e.message;
  }
}

$('taskBody').addEventListener('click', async ev => {
  const b = ev.target.closest('button[data-t]');
  if (!b || !taskUID) return;
  const kind = b.dataset.t, code = b.dataset.c;
  const done = setBusy(b);
  try {
    if (kind === 'auto') {
      // 一键完成：后端执行动作 → 回读进度 → 汇报（耗时可到分钟级，含真实对话）
      b.textContent = '执行中…';
      const r = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks/auto', {
        method: 'POST', body: JSON.stringify({ task_code: code })
      });
      if (r.skipped) {
        toast(r.message || '已跳过', 'ok');
      } else {
        const advanced = r.progress_before !== r.progress_after;
        let msg = r.message || '已执行';
        if (r.progress_after) msg += `（进度 ${r.progress_before} → ${r.progress_after}）`;
        if (r.claimed) msg += '，奖励已自动到账';
        else if (r.claimable) msg += r.claim_error ? '，可点「领取」重试' : '';
        else if (r.attempt && !advanced) msg += '；进度未动，该任务可能需要官方客户端';
        toast(msg, (r.claimed || advanced) ? 'ok' : 'err');
      }
      loadOverview(true);
    } else {
      const path = 'accounts/' + encodeURIComponent(taskUID) + '/tasks/' + (kind === 'claim' ? 'claim' : 'accept');
      const body = kind === 'claim' ? { task_code: code } : { task_codes: [code] };
      await api(path, { method: 'POST', body: JSON.stringify(body) });
      toast(kind === 'claim' ? '已领取奖励' : '已接受任务', 'ok');
      if (kind === 'claim') loadOverview(true);
    }
  } catch (e) { toast(e.message, 'err'); }
  finally { done(); loadTasks(); }
});

/* ── 任务中心：开学季 + 全账号扫描/队列 ──────────────────────────── */
const SCHOOL_TITLES = {
  share_invite: '分享活动 +100c', desktop_chat_1_time: '桌面端体验 +100c（单次）',
  chat_3_times: '和 AI 对话 3 次 +50c', expert_use: '召唤开学季专家 +50c',
  task_student_verify: '学生认证 +100c（需真实认证，不做）',
};

/* ── 精简 QR 编码器（券码二维码用）────────────────────────────────────
   规格子集：byte 模式、ECC L、版本 1-5（全部单纠错块，免块交织）、固定掩码 0。
   完整性：规范允许任选掩码（解码器按格式信息位自行去掩码），固定掩码不影响
   可扫描性；已用 python qrcode 库对多输入多版本做逐像素交叉验证（强制 byte
   模式 + mask 0，5/5 全部 diff=0）。面板 CSP 只允许 self，外链 QR 服务不可用。 */
// qr_gen.js —— 精简 QR 编码器（浏览器用 + node 可跑交叉验证）
// 规格子集：byte 模式、ECC L、版本 1-5（全部单纠错块，免块交织）、固定掩码 0。
// 完整性说明：规范允许编码器任选掩码（解码器按格式信息位自行去掩码），
// 固定掩码不影响可扫描性；券码为短文本，v1-5（26 字节起）绰绰有余。

// GF(256) 对数/指数表（本原多项式 0x11d）
const QR_EXP = new Array(512), QR_LOG = new Array(256);
(() => {
  let x = 1;
  for (let i = 0; i < 255; i++) { QR_EXP[i] = x; QR_LOG[x] = i; x <<= 1; if (x & 0x100) x ^= 0x11d; }
  for (let i = 255; i < 512; i++) QR_EXP[i] = QR_EXP[i - 255];
})();
const gmul = (a, b) => (a && b) ? QR_EXP[QR_LOG[a] + QR_LOG[b]] : 0;

// 各版本参数（下标 = 版本-1）：[数据码字数, 纠错码字数]，ECC L 单块
const QR_V = [[19, 7], [34, 10], [55, 15], [80, 20], [108, 26]];
// 对齐图案中心坐标（v2+；与定位图案重叠的位置在放置时跳过）
const QR_ALIGN = [[], [6, 18], [6, 22], [6, 26], [6, 30]];
const QR_MASK = (r, c) => (r + c) % 2 === 0; // 掩码模式 0

// 生成多项式（最高次系数在前，g[0] 恒为 1）
function qrGenPoly(deg) {
  let g = [1];
  for (let i = 0; i < deg; i++) {
    const a = QR_EXP[i], ng = new Array(g.length + 1).fill(0);
    ng[0] = g[0];
    for (let j = 1; j < g.length; j++) ng[j] = g[j] ^ gmul(a, g[j - 1]);
    ng[g.length] = gmul(a, g[g.length - 1]);
    g = ng;
  }
  return g;
}

// Reed-Solomon 求余（综合除法），返回 deg 个纠错码字
function rsRem(data, deg) {
  const g = qrGenPoly(deg);
  const res = data.concat(new Array(deg).fill(0));
  for (let i = 0; i < data.length; i++) {
    const f = res[i];
    if (f) for (let j = 0; j < g.length; j++) res[i + j] ^= gmul(g[j], f);
  }
  return res.slice(data.length);
}

// 文本 → 码字流（byte 模式：0100 + 8 位计数 + 数据 + 终止符 + 0xEC/0x11 填充）
function qrDataCodewords(text, dataCap) {
  const bytes = Array.from(new TextEncoder().encode(text));
  const bits = [];
  const push = (val, n) => { for (let i = n - 1; i >= 0; i--) bits.push((val >> i) & 1); };
  push(4, 4);            // byte 模式
  push(bytes.length, 8); // v1-9 计数 8 位
  for (const b of bytes) push(b, 8);
  const cap = dataCap * 8;
  push(0, Math.min(4, cap - bits.length));   // 终止符
  while (bits.length % 8) bits.push(0);
  const out = [];
  for (let i = 0; i < bits.length; i += 8) {
    let v = 0; for (const b of bits.slice(i, i + 8)) v = (v << 1) | b;
    out.push(v);
  }
  for (let p = 0; out.length < dataCap; p ^= 1) out.push(p ? 0x11 : 0xEC);
  return out;
}

// 主入口：text → 布尔矩阵（true=深色模块）
function qrMatrix(text) {
  const bytes = Array.from(new TextEncoder().encode(text));
  // 版本选择：需求 ≈ 2 码字头 + 文本长度，取首个放得下的版本
  let ver = 0;
  for (let v = 0; v < QR_V.length; v++) { if (bytes.length + 2 <= QR_V[v][0]) { ver = v + 1; break; } }
  if (!ver) throw new Error('QR: text too long (>' + QR_V[4][0] + ' bytes)');
  const [dataCap, ecCap] = QR_V[ver - 1];
  const n = 17 + 4 * ver;

  const M = Array.from({ length: n }, () => new Array(n).fill(false));
  const F = Array.from({ length: n }, () => new Array(n).fill(false)); // 功能模块占位

  const setF = (r, c, v) => { M[r][c] = v; F[r][c] = true; };
  // 定位图案 + 分隔带
  const finder = (r0, c0) => {
    for (let r = -1; r <= 7; r++) for (let c = -1; c <= 7; c++) {
      const rr = r0 + r, cc = c0 + c;
      if (rr < 0 || cc < 0 || rr >= n || cc >= n) continue;
      const dark = r >= 0 && r <= 6 && c >= 0 && c <= 6 && (r === 0 || r === 6 || c === 0 || c === 6 || (r >= 2 && r <= 4 && c >= 2 && c <= 4));
      setF(rr, cc, dark);
    }
  };
  finder(0, 0); finder(0, n - 7); finder(n - 7, 0);
  // 校正图形（仅贯穿两定位图案之间：8..n-9，不得覆盖定位图案本体）
  for (let r = 8; r <= n - 9; r++) setF(r, 6, r % 2 === 0);
  for (let c = 8; c <= n - 9; c++) setF(6, c, c % 2 === 0);
  // 对齐图案（v2+，跳过与定位重叠处）
  const align = QR_ALIGN[ver - 1] || [];
  for (const ar of align) for (const ac of align) {
    if (F[ar][ac]) continue;
    for (let r = -2; r <= 2; r++) for (let c = -2; c <= 2; c++)
      setF(ar + r, ac + c, Math.max(Math.abs(r), Math.abs(c)) !== 1);
  }
  // 暗模块 + 格式信息（ECC L=01，掩码 0）——BCH(15,5) + 0x5412 异或。
  // 位序遵循规范（与 python qrcode 逐位对齐验证）：bit i 从 LSB 起数，
  // 副本一走左上角 L 形、副本二走右下 L 形。
  let fmt = (1 << 3) | 0; // L<<3 | mask
  let rem = fmt << 10;
  for (let i = 14; i >= 10; i--) if ((rem >> i) & 1) rem ^= 0x537 << (i - 10);
  fmt = ((fmt << 10) | rem) ^ 0x5412; // 15 位
  const fb = i => (fmt >> i) & 1;
  // 副本一（左上）：位 0..5 → (i,8)；6 → (7,8)；7 → (8,8)
  for (let i = 0; i <= 5; i++) setF(i, 8, !!fb(i));
  setF(7, 8, !!fb(6)); setF(8, 8, !!fb(7));
  // 副本一续 + 副本二（右下）：位 8..14 → (n-15+i, 8)；位 0..7 → (8, n-1-i)；8 → (8,7)；9..14 → (8,14-i)
  for (let i = 8; i <= 14; i++) setF(n - 15 + i, 8, !!fb(i));
  for (let i = 0; i <= 7; i++) setF(8, n - 1 - i, !!fb(i));
  setF(8, 7, !!fb(8));
  for (let i = 9; i <= 14; i++) setF(8, 14 - i, !!fb(i));
  // 暗模块（恒为深色，位于副本一垂直段末端）
  setF(n - 8, 8, true);


  // 数据码字 + 纠错码字 → 位流
  const dcw = qrDataCodewords(text, dataCap);
  const cw = dcw.concat(rsRem(dcw, ecCap));
  const bits = [];
  for (const b of cw) for (let i = 7; i >= 0; i--) bits.push((b >> i) & 1);

  // 蛇形放置（成对列，从右向左，跳过第 6 列），写数据时直接异或掩码
  let bi = 0, up = true;
  for (let x = n - 1; x > 0; x -= 2) {
    if (x === 6) x--;
    for (let i = 0; i < n; i++) {
      const r = up ? n - 1 - i : i;
      for (const c of [x, x - 1]) {
        if (F[r][c]) continue;
        const bit = bi < bits.length ? bits[bi++] : 0;
        M[r][c] = bit ? !QR_MASK(r, c) : QR_MASK(r, c);
      }
    }
    up = !up;
  }
  return M;
}

// 矩阵 → SVG（quiet zone 4 模块）
function qrSVG(M, px) {
  const n = M.length, q = 4, total = n + q * 2;
  let s = '<svg viewBox="0 0 ' + total + ' ' + total + '" width="' + px + '" height="' + px + '" shape-rendering="crispEdges" role="img" style="background:#fff">';
  for (let r = 0; r < n; r++) for (let c = 0; c < n; c++)
    if (M[r][c]) s += '<rect x="' + (c + q) + '" y="' + (r + q) + '" width="1" height="1"/>';
  return s + '</svg>';
}

/* ── 开学季券码查询（弹窗，仿活动页 #/prizes?tab=vouchers）──────────── */
/* copyText：clipboard API 只在 secure context（https/localhost）可用，
   远程 http 面板会拿不到 navigator.clipboard → 降级 execCommand。 */
function copyText(text) {
  if (navigator.clipboard && window.isSecureContext) return navigator.clipboard.writeText(text);
  return new Promise((resolve, reject) => {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.style.cssText = 'position:fixed;opacity:0';
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand('copy') ? resolve() : reject(new Error('copy failed')); }
    catch (e) { reject(e); }
    finally { ta.remove(); }
  });
}

function vcCard(v) {
  const expired = v.valid_to && new Date(v.valid_to) < new Date();
  return '<div class="vc' + (expired ? ' expired' : '') + '">' +
    '<div class="hd"><span class="nm">' + esc(v.prize_name || v.sku_code || '券') + '</span>' +
    (expired ? '<span class="tag bad">已过期</span>' : '<span class="tag ok">可使用</span>') + '</div>' +
    '<div class="meta">' +
      (v.valid_to ? '有效期至 ' + esc(v.valid_to) : '长期有效') +
      (v.granted_at ? ' · ' + esc(v.granted_at.slice(0, 10)) + ' 抽中' : '') +
    '</div>' +
    '<div class="sep"></div>' +
    '<div class="ft"><span class="lab">券码</span><code>' + esc(v.code || '-') + '</code>' +
    '<span class="acts">' +
      (v.code ? '<button class="xs ghost" data-qr="' + esc(v.code) + '">二维码</button>' : '') +
      '<button class="xs ghost" data-copy="' + esc(v.code || '') + '">复制</button>' +
    '</span></div>' +
    '</div>';
}

async function loadSchoolVouchers() {
  const body = $('vcBody');
  openVeil('vcVeil');
  body.innerHTML = '<div class="state"><span class="dots">查询中</span></div>';
  $('vcNote').textContent = '';
  try {
    const d = await api('school/vouchers');
    const arr = d.accounts || [];
    const ok = arr.filter(a => !a.error);
    const total = ok.reduce((n, a) => n + (a.vouchers || []).length, 0);
    body.innerHTML = ok.filter(a => (a.vouchers || []).length).map(a =>
      '<div class="vc-acct"><span class="nm">' + esc(a.nickname || a.uid) + '</span>' +
      '<span>' + a.vouchers.length + ' 张</span></div>' +
      a.vouchers.map(vcCard).join('')
    ).join('') || '<div class="empty"><div class="big">🎟️</div>还没有抽到券</div>';
    $('vcNote').textContent = total ? total + ' 张券 · ' + ok.filter(a => !(a.vouchers || []).length).length + ' 个账号未抽中' : '';
    const errs = arr.filter(a => a.error);
    if (errs.length) {
      body.insertAdjacentHTML('beforeend', '<div class="note txt-warn mt-2">查询失败：' +
        errs.map(a => esc(a.nickname || a.uid.slice(0, 8)) + '（' + esc(shortErr(a.error)) + '）').join('、') + '</div>');
    }
    body.querySelectorAll('button[data-copy]').forEach(b => b.onclick = async () => {
      try { await copyText(b.dataset.copy); toast('券码已复制', 'ok'); }
      catch (e) { toast('复制失败，请手动选择券码', 'err'); }
    });
    // 二维码：券码本体编码为 QR（到店出示扫描），点击切换显示/隐藏
    body.querySelectorAll('button[data-qr]').forEach(b => b.onclick = () => {
      const card = b.closest('.vc');
      const old = card.querySelector('.vc-qr');
      if (old) { old.remove(); return; }
      const box = document.createElement('div');
      box.className = 'vc-qr';
      try { box.innerHTML = qrSVG(qrMatrix(b.dataset.qr), 148); }
      catch (e) { box.innerHTML = '<span class="note">二维码生成失败：' + esc(shortErr(e.message)) + '</span>'; }
      card.appendChild(box);
    });
  } catch (e) {
    body.innerHTML = '<div class="state err">' + esc(shortErr(e.message)) + '</div>';
  }
}
$('btnSchoolVouchers').onclick = loadSchoolVouchers;
$('btnVcClose').onclick = () => closeVeil('vcVeil');
$('btnVcRefresh').onclick = loadSchoolVouchers;

/* 成长任务队列。lastQueueSeq 记录本页启动过的队列代次：执行结束后的残留 items
   （running=false 但 seq 停在旧值）不再回写视图——否则扫描结果 3 秒后被上一轮
   队列状态覆盖。 */
let queueTimer = null, lastQueueSeq = 0;
const GROWTH_TITLES = {}; // code → 展示名（扫描时从任务列表带出）
$('btnScanAll').onclick = async () => {
  const b = $('btnScanAll');
  // 停掉队列轮询：显式扫描 = 切到待办视图。否则在途队列的下一 tick 会把扫描
  // 结果冲掉重渲染回队列视图（服务端执行不受影响，只是不再实时回写本视图）。
  if (queueTimer) { clearInterval(queueTimer); queueTimer = null; }
  const done = setBusy(b, '扫描中…');
  try {
    const d = await api('tasks/scan_all', { method: 'POST' });
    const items = groupItems(d);
    if (!items.length && !(d.accounts || []).length) {
      // 账号池是空的：不能沿用"全部任务都已完成"的说法，那会让人以为扫过了。
      renderQueue([], null, '账号池是空的', '先去「账号池」用浏览器登录一个账号，再回来扫描成长任务。');
    } else {
      renderQueue(items, null, '没有待办任务 🎉', '全部账号的成长任务与开学季活动都已完成，明日再来。');
    }
  } catch (e) { toast(e.message, 'err'); }
  finally { done('扫描待办'); }
};
$('btnRunQueue').onclick = async () => {
  const conc = Number($('qcConc').value) || 1;
  if (!(await askConfirm({
    title: '执行全部待办', confirmText: '开始执行',
    body: '扫描全部账号待办并排队执行（账号并发 ' + conc + '，账号内串行）。\n含真实对话的任务耗时较长，确认继续？'
  }))) return;
  const b = $('btnRunQueue');
  const done = setBusy(b, '启动中…');
  try {
    const r = await api('tasks/run_queue', { method: 'POST', body: JSON.stringify({ concurrency: conc }) });
    if (!r.started) { toast(r.message || '没有待办任务', 'ok'); return; }
    lastQueueSeq = r.seq || 0;
    toast('队列已启动：' + r.total + ' 项（并发 ' + conc + '）', 'ok');
    startQueuePolling();
  } catch (e) { toast(e.message, 'err'); }
  finally { done('执行全部待办'); }
};
// 扫描结果 → 分组条目（无执行状态）
function groupItems(d) {
  const groups = [];
  for (const a of (d.accounts || [])) {
    const rows = [];
    for (const t of (a.growth || [])) {
      GROWTH_TITLES[t.task_code] = t.title || t.task_code;
      rows.push({ kind: 'growth', code: t.task_code, prog: t.target ? t.current + '/' + t.target : '—', status: 'scan' });
    }
    if (rows.length) groups.push({ uid: a.uid, nick: a.nickname, rows });
  }
  return groups;
}
const ST_WORDS = { done: '完成', running: '执行中', error: '失败', skipped: '跳过', pending: '排队', scan: '待执行' };
function qrowHTML(it) {
  const isSchool = it.kind === 'school';
  const title = isSchool ? '开学季闭环' : (GROWTH_TITLES[it.code] || it.code);
  const dotCls = it.status === 'scan' ? 'wait' : it.status === 'running' ? 'run' : it.status === 'error' ? 'err' : it.status === 'skipped' ? 'skip' : it.status === 'done' ? 'done' : 'wait';
  const stWord = it.status === 'scan' ? '待执行' : (ST_WORDS[it.status] || it.status);
  return '<div class="qrow" title="' + esc(it.message || '') + '">' +
    '<span class="code">' + esc(it.code) + '</span>' +
    '<span class="name"><span class="t">' + esc(title) + '</span>' + (isSchool ? '<span class="tag mute">开学季</span>' : '') + '</span>' +
    '<span class="prog">' + esc(it.prog || '') + '</span>' +
    '<span class="st"><span class="qdot ' + dotCls + '"></span>' + stWord + '</span>' +
    '<span class="msg">' + esc(it.message || '') + '</span>' +
    '</div>';
}
function renderQueue(groups, progress, emptyTitle, emptyDesc) {
  const empty = $('tcEmpty'), list = $('qcList');
  if (!groups.length) {
    empty.style.display = '';
    // v3 把空态标题从 .t 换成了 .big，这里漏改会让「扫描完没有待办」直接抛
    // TypeError（用户看到错误 toast，而不是「没有待办任务 🎉」）。
    const titleEl = empty.querySelector('.big');
    if (emptyTitle && titleEl) titleEl.textContent = emptyTitle;
    if (emptyDesc) empty.querySelector('.d').textContent = emptyDesc;
    list.innerHTML = '';
    $('qProg').hidden = true; $('qcSummary').textContent = '';
    $('btnRunQueue').disabled = true;
    return;
  }
  $('btnRunQueue').disabled = false;
  empty.style.display = 'none';
  let total = 0;
  list.innerHTML = groups.map(g => {
    total += g.rows.length;
    return '<div class="qgroup"><header><span class="nm">' + esc(g.nick || g.uid.slice(0, 12)) + '</span><span class="cnt">' + g.rows.length + ' 项待办</span></header>' +
      g.rows.map(qrowHTML).join('') + '</div>';
  }).join('');
  $('qcSummary').textContent = total + ' 项';
  updateProgress(progress);
}
function updateProgress(q) {
  if (!q || !q.items) { $('qProg').hidden = true; return; }
  const total = q.items.length;
  const done = q.items.filter(it => it.status === 'done' || it.status === 'error' || it.status === 'skipped').length;
  $('qProg').hidden = false;
  $('qBarFill').style.width = (total ? Math.round(done / total * 100) : 0) + '%';
  $('qProgText').textContent = (q.running ? '执行中 ' : '已结束 ') + done + ' / ' + total;
}
// 队列状态 → 分组（执行时轮询）
function groupsFromQueue(items) {
  const by = new Map();
  for (const it of items) {
    if (!by.has(it.uid)) by.set(it.uid, { uid: it.uid, nick: it.nickname, rows: [] });
    by.get(it.uid).rows.push({
      kind: it.kind, code: it.code,
      prog: it.kind === 'school' ? '—' : '',
      status: it.status, message: it.message,
    });
  }
  return Array.from(by.values());
}
function startQueuePolling() {
  if (queueTimer) clearInterval(queueTimer);
  queueTimer = setInterval(async () => {
    let q;
    try { q = await api('tasks/queue'); } catch (e) { return; }
    if (!q.started) return;
    // 只渲染本页启动过的那轮队列（刷新页面后不再接管旧队列）。
    if (lastQueueSeq && q.seq !== lastQueueSeq) return;
    if (q.running) {
      renderQueue(groupsFromQueue(q.items || []), q);
      return;
    }
    // 结束：终态只渲染这一次，随即停表。此后残留的 items（running=false）不再
    // 回写视图——曾把用户刚点开的「扫描待办」结果在下一个 tick 冲掉。
    renderQueue(groupsFromQueue(q.items || []), q);
    clearInterval(queueTimer); queueTimer = null;
    toast('任务队列执行结束', 'ok');
  }, 3000);
}
// reattachQueueView 切回任务中心视图时恢复队列进度：仅当本页启动的队列仍在
// 执行才重新开轮询（残留态/别页队列不接管——视图不被旧结果冲掉）。
function reattachQueueView() {
  // 全程异步：go() 在顶层（app.js ~143 行）被调用时，本文件下方 let/const
  //（queueTimer/lastQueueSeq 等）尚未初始化——同步读取即 TDZ ReferenceError
  // 使整个脚本中断。await 之后才碰它们（旧 pollQueueOnce 正是靠开头的 await
  // 侥幸安全）。queueTimer 的"已在跑"判定也挪到 await 后，语义不变。
  (async () => {
    try {
      const q = await api('tasks/queue');
      if (queueTimer) return; // 轮询已在跑（跨视图不中断）
      if (q.started && q.running && (!lastQueueSeq || q.seq === lastQueueSeq)) startQueuePolling();
    } catch (e) { /* 静默 */ }
  })();
}

/* ── 用量 ─────────────────────────────────────────────────────────── */
/* 图表用原生 SVG 手绘：面板是 go:embed 单文件、无构建步骤，引入图表库
   就得带上打包器，得不偿失。这里只需要堆叠柱状图，二十行足够。 */

function fmtTok(n) {
  // 缺失值统一 —：以前 null → 0，账号表里看起来像「这个号真的用了 0 token」。
  if (n == null || n === '') return '—';
  n = Number(n);
  if (!Number.isFinite(n)) return '—';
  if (n >= 1e9) return (n / 1e9).toFixed(2) + 'B';
  if (n >= 1e6) return (n / 1e6).toFixed(2) + 'M';
  if (n >= 1e3) return (n / 1e3).toFixed(1) + 'k';
  return String(n);
}
function fmtMs(ms) {
  ms = Number(ms || 0);
  if (!ms) return '—';
  if (ms >= 1000) return (ms / 1000).toFixed(2) + 's';
  return Math.round(ms) + 'ms';
}
function fmtRate(r) { return r ? Number(r).toFixed(1) + ' tok/s' : '—'; }
function trimFixed(s) {
  if (!String(s).includes('.')) return String(s);
  return String(s).replace(/0+$/, '').replace(/\.$/, '');
}
function fmtCredit(n) {
  const v = Number(n || 0);
  if (!Number.isFinite(v)) return '—';
  return trimFixed(v.toFixed(2));
}
function fmtCreditRatio(v, samples, tokens) {
  if (!samples || !tokens) return '—';
  const n = Number(v || 0);
  if (!Number.isFinite(n)) return '—';
  return trimFixed(n.toFixed(4)) + ' / 1M';
}
function fmtModelRate(rate) {
  const s = String(rate || '').trim();
  return s ? 'x' + s : '—';
}
/* fmtMoney 官方价折算的金额格式：￥ + 两位小数 + 千分位。
   手写分组而不是 toLocaleString：面板要在 node 冒烟测试的极简沙箱里跑，
   Intl 行为随宿主 ICU 版本漂移，输出不稳定。 */
function fmtMoney(v) {
  const n = Number(v || 0);
  if (!Number.isFinite(n)) return '—';
  const neg = n < 0;
  const parts = Math.abs(n).toFixed(2).split('.');
  const int = parts[0].replace(/\B(?=(\d{3})+(?!\d))/g, ',');
  return (neg ? '-' : '') + '￥' + int + '.' + parts[1];
}
/* fmtCoverage 牌价覆盖率：正常一位小数；但有未定价 token 时不许四舍五入成 100.0%
   （那会和旁边的「未定价 611.4k」自相矛盾），压到 99.99%。 */
function fmtCoverage(cov, unpriced) {
  const c = Math.max(0, Math.min(100, Number(cov) || 0));
  const s = c.toFixed(1) + '%';
  // 一位小数会把 99.95+ 进位成 100.0：还有未定价 token 时不能这么说。
  if (Number(unpriced || 0) > 0 && s === '100.0%') return '99.99%';
  return s;
}

function usStat(v, k, cls) {
  // v3：与 usKpi 同序（标签在上、数值在下）。当前没有调用点，但保留函数名——
  // 前端测试按它切分 app.js，删掉会让 TestAppJSSaveRows 找不到边界。
  return '<div class="stat ' + (cls || '') + '"><div class="k">' + esc(k) +
         '</div><div class="v">' + esc(v) + '</div></div>';
}

/* usKpi 用量页的指标卡。比账号池的 .stat 多两样：语义色轨（cls）与副标题（sub，
   放"占比 / 均速率"这类解释性数字）；bar 是卡片内的构成条 HTML，只有需要时才传。
   sub 传数组时每个元素单独包一层 nowrap 的 .s-pair：断行只发生在片段之间，
   不会再出现「completion」和「0.3%」被拆到两行的断法。 */
function usKpi(v, k, cls, sub, bar) {
  const subHtml = Array.isArray(sub)
    ? sub.filter(Boolean).map(s => '<span class="s-pair">' + esc(s) + '</span>').join('')
    : (sub ? esc(sub) : '');
  return '<div class="kpi ' + (cls || '') + '">' +
    '<div class="k">' + esc(k) + '</div>' +
    '<div class="v">' + esc(v) + '</div>' +
    (bar || '') +
    (subHtml ? '<div class="s">' + subHtml + '</div>' : '') +
    '</div>';
}

/* usMixBar prompt/completion 占比条。宽度按百分比而不是固定像素：表列宽随窗口变化，
   像素宽度在窄屏会溢出、宽屏又显得没信息。 */
function usMixBar(prompt, completion, total) {
  const t = Number(total || 0);
  if (!t) return '';
  const pp = Math.max(0, Math.min(100, Number(prompt || 0) / t * 100));
  const pc = Math.max(0, Math.min(100, Number(completion || 0) / t * 100));
  return '<span class="us-mix" title="prompt ' + pp.toFixed(1) + '% · completion ' + pc.toFixed(1) + '%">' +
    '<i class="p" style="width:' + pp.toFixed(2) + '%"></i>' +
    '<i class="c" style="width:' + pc.toFixed(2) + '%"></i>' +
    '</span>';
}

/* usPct 占比文案（0 值不显示 "0.0%"，直接 —，避免一行全是零）。 */
function usPct(part, total) {
  const t = Number(total || 0);
  if (!t) return '—';
  return (Number(part || 0) / t * 100).toFixed(1) + '%';
}

/* usCostCell「官方价」单元格：只有确实计过价的 token 才显示金额；
   未定价行给 —（显示 0.00 会被读成"这个模型不要钱"）。 */
function usCostCell(a) {
  if (!Number(a && a.priced_tokens ? a.priced_tokens : 0)) return '<span class="muted">—</span>';
  return '<span title="按官方 API 牌价折算（元）">' + esc(fmtMoney(a.saved_yuan)) + '</span>';
}

/* ── 已省：一张账单 ─────────────────────────────────────────────────
   左边大字金额 + 覆盖率细条，右边按模型列出「请求 / token / 折算 / 占比」。
   未定价模型永远排在末尾（不计金额，但要让人看见），有价行默认只显示前 SAVE_ROWS 个。 */
const SAVE_ROWS = 6;
let usSaveAll = false;

/* saveRowHtml 一行：模型名 + 单价摘要 / 请求 / Token / 金额 / 占比条。
   priced 看 priced_tokens：0 元也可能是"缓存全命中"，不能按金额判断有没有定价。 */
function saveRowHtml(r, total) {
  const priced = Number(r.priced_tokens || 0) > 0;
  const amount = Number(r.saved_yuan || 0);
  const pct = priced && total > 0 ? Math.max(0, Math.min(100, amount / total * 100)) : 0;
  const note = String(r.price_note || '').trim();
  return '<tr>' +
    '<td class="who"><div class="sv-name" title="' + esc(r.key || '') + '">' + esc(r.key || '—') +
      (priced ? '' : ' <span class="tag mute">未定价</span>') + '</div>' +
      (note ? '<div class="sv-price">' + esc(note) + '</div>' : '') + '</td>' +
    '<td class="num">' + fmtTok(r.requests) + '</td>' +
    '<td class="num">' + fmtTok(r.total_tokens) + '</td>' +
    '<td class="num sv-amt' + (priced ? '' : ' zero') + '">' + (priced ? fmtMoney(amount) : '—') + '</td>' +
    '<td><div class="sv-share">' + (priced
      ? '<span class="bar"><i style="width:' + pct.toFixed(2) + '%"></i></span>' +
        '<span class="pct">' + pct.toFixed(1) + '%</span>'
      : '<span class="pct">—</span>') + '</div></td>' +
    '</tr>';
}

/* saveRowsHtml 行集合：有价行按金额降序（同额按 token），未定价行按 token 降序垫底。
   all=false 时只截断**有价行**，未定价行始终全显 —— 覆盖率缺口不能靠折叠藏起来。 */
/* isEmptyUsageRow 只有请求、没有任何 token 的模型行（多为失败尝试）：
   进表只占行没有信息，改为在页脚记数量。 */
function isEmptyUsageRow(r) {
  return !Number(r.total_tokens || 0) && !Number(r.saved_yuan || 0) && !Number(r.priced_tokens || 0);
}

function saveRowsHtml(rows, total, all) {
  const list = (Array.isArray(rows) ? rows.slice() : []).filter(r => !isEmptyUsageRow(r));
  const priced = list.filter(r => Number(r.priced_tokens || 0) > 0)
    .sort((a, b) => (Number(b.saved_yuan || 0) - Number(a.saved_yuan || 0)) ||
                    (Number(b.total_tokens || 0) - Number(a.total_tokens || 0)));
  const unpriced = list.filter(r => !(Number(r.priced_tokens || 0) > 0))
    .sort((a, b) => Number(b.total_tokens || 0) - Number(a.total_tokens || 0));
  const shown = all ? priced : priced.slice(0, SAVE_ROWS);
  const out = shown.concat(unpriced);
  if (!out.length) return '<tr><td colspan="5"><div class="empty inline">暂无用量数据</div></td></tr>';
  return out.map(r => saveRowHtml(r, total)).join('');
}

/* renderSaveStats 把整份用量快照画成「已省」区块。入参是 usageData 本身：
   金额与明细来自 by_model，牌价元信息来自 pricing。 */
function renderSaveStats(d) {
  d = d || {};
  const p = d.pricing || {};
  const t = d.totals || {};
  const on = !!p.enabled;
  const saved = Number(p.saved_yuan != null ? p.saved_yuan : (t.saved_yuan || 0));
  const life = Number(p.lifetime_saved_yuan || 0);
  const cov = Number(p.coverage_pct || 0);
  const unpriced = Number(t.unpriced_tokens || 0);
  const rows = d.by_model || [];
  const range = trangeLabel('usRange');

  const num = $('usSaveNum');
  num.textContent = on ? fmtMoney(saved) : '—';
  num.classList.toggle('off', !on);
  const reqs = Number(t.requests || 0);
  $('usSaveSub').textContent = on
    ? (range ? range + ' · ' : '') + (reqs > 0 ? fmtTok(reqs) + ' 次请求 · ' : '') + '累计 ' + fmtMoney(life)
    : '牌价表未启用，本区不折算金额';
  $('usSaveCovBar').style.width = (on ? Math.max(0, Math.min(100, cov)) : 0).toFixed(1) + '%';
  $('usSaveCovTxt').textContent = on
    ? '牌价覆盖 ' + fmtCoverage(cov, unpriced) + (unpriced > 0 ? ' · 未定价 ' + fmtTok(unpriced) : '')
    : '—';

  const peak = p.peak_models || [];
  const meta = on
    ? ['牌价 ' + (p.updated || '—'), String(p.models || 0) + ' 个模型',
       p.source === 'seed+file' ? '种子+本地覆盖' : '内置种子']
    : ['未启用'];
  $('usSaveMeta').textContent = meta.join(' · ');
  const tip = [];
  if (on) {
    tip.push('来源：' + (p.source === 'seed+file' ? '内置种子 + data/prices.json' : '内置种子（没读到 data/prices.json）'));
    if (peak.length) tip.push('峰谷价模型：' + peak.join('、'));
    const un = p.unpriced_models || [];
    if (un.length) {
      tip.push('未定价：' + un.join('、') +
        (Number(p.unpriced_model_count || 0) > un.length ? ' 等 ' + p.unpriced_model_count + ' 个' : ''));
    }
    if (p.error) tip.push('价格文件读取失败：' + p.error + '（沿用上一版）');
  }
  $('usSaveMeta').title = tip.join('\n');

  const pricedCount = rows.filter(r => Number(r.priced_tokens || 0) > 0).length;
  const btn = $('usSaveMore');
  if (btn) {
    btn.hidden = pricedCount <= SAVE_ROWS;
    if (!btn.hidden) btn.textContent = usSaveAll ? '收起' : '展开全部 ' + pricedCount + ' 个';
  }
  $('usSaveRows').innerHTML = saveRowsHtml(rows, saved, usSaveAll);

  const bits = [];
  if (!on) bits.push('牌价表未接线，本区不折算金额');
  else {
    bits.push('按厂商官网牌价（元/百万 tokens）折算，只统计不参与计费');
    if (peak.length) bits.push('峰谷价：工作日 9–12、14–18 高峰原价，其余时段与周末半价');
    bits.push('未上报缓存命中按未命中价计（偏高）');
    const empties = rows.filter(isEmptyUsageRow).length;
    if (empties > 0) bits.push('另有 ' + empties + ' 个模型无 token 记录（多为失败请求）');
  }
  $('usSaveFoot').textContent = bits.join(' · ');
  $('usSaveFoot').title = bits.join(' · ');
}

/* usRow 生成一行。mid 是插在「名称」之后、请求数之前的额外单元格（如「域」列）。
   withPerf 控制延迟/速率两列；列开关显式传入，避免调用方改动后与表头错列。 */
/* nameHtml 非空时替代 esc(name)（已转义的富文本，见 remarkName）：账号维度的第一列
   需要把备注渲染成主标识、uid8 作为浅色虚体跟随，其余维度仍走纯文本转义。 */
function usRow(name, sub, a, mid, withPerf, nameHtml) {
  return '<tr>' +
    '<td class="mark" aria-hidden="true"></td>' +
    '<td>' + (nameHtml || esc(name)) + (sub ? '<div class="note">' + esc(sub) + '</div>' : '') + '</td>' +
    (mid || '') +
    '<td class="num">' + fmtTok(a.requests) + '</td>' +
    '<td class="num">' + (a.errors ? '<span class="txt-warn">' + fmtTok(a.errors) + '</span>' : '—') + '</td>' +
    '<td class="num">' + fmtTok(a.prompt_tokens) + '</td>' +
    '<td class="num">' + fmtTok(a.completion_tokens) + '</td>' +
    '<td class="num">' + fmtTok(a.total_tokens) +
      usMixBar(a.prompt_tokens, a.completion_tokens, a.total_tokens) + '</td>' +
    (withPerf
      ? '<td class="num">' + fmtMs(a.avg_latency_ms) + '</td>' +
        '<td class="num">' + fmtRate(a.avg_tokens_per_second) + '</td>'
      : '') +
    '<td class="num">' + usCostCell(a) + '</td>' +
    '</tr>';
}

/* ── 用量明细：三个维度共用一张表 + 页内切换 ──────────────────────────
   账号 / 模型 / 域三张表此前各自占一个 box，页面纵向拉得很长且表头结构几乎一样。
   现在合成一个 box：表头由维度定义生成，行渲染复用 usRow，切换零请求。 */
const US_DIMS = {
  // span = 该维度表格的列数（含新增的「官方价」列），空行 colspan 用它。
  account: { key: 'by_account', title: '账号', withRealm: true, withPerf: true, span: 11 },
  model: { key: 'by_model', title: '模型', withRealm: false, withPerf: false, span: 8 },
  realm: { key: 'by_realm', title: 'realm', withRealm: false, withPerf: false, span: 8 },
};

// usSortRows 按当前排序字段降序（默认合计 Token，最大者最相关）。
function usSortRows(rows, sort) {
  const out = rows.slice();
  const num = v => { const n = Number(v || 0); return Number.isFinite(n) ? n : 0; };
  const val = a => sort === 'requests' ? num(a.requests)
    : sort === 'errors' ? num(a.errors)
    : sort === 'latency' ? num(a.avg_latency_ms)
    : num(a.total_tokens);
  out.sort((a, b) => val(b) - val(a));
  return out;
}

function usDimHead(dim) {
  const m = US_DIMS[dim] || US_DIMS.account;
  return '<tr><th class="mark" aria-hidden="true"></th><th>' + esc(m.title) + '</th>' +
    (m.withRealm ? '<th>域</th>' : '') +
    '<th class="num">请求</th><th class="num">失败</th>' +
    '<th class="num">Prompt</th><th class="num">Completion</th><th class="num">合计</th>' +
    (m.withPerf ? '<th class="num">均延迟</th><th class="num">均速率</th>' : '') +
    '<th class="num" title="按官方 API 牌价折算（元）">官方价</th>' +
    '</tr>';
}

/* usTabsHtml 维度切换按钮（带条数徽标）。整段 innerHTML 重写而不是逐个改 class：
   容器的 click 监听是委托式的，换掉子节点不会丢事件，代码也更短。 */
function usTabsHtml(dims, active, counts) {
  return dims.map(([k, label]) => {
    const n = counts ? counts[k] : null;
    return '<button data-dim="' + k + '"' + (k === active ? ' class="on"' : '') + '>' +
      esc(label) + (n == null ? '' : '<span class="cnt">' + n + '</span>') + '</button>';
  }).join('');
}

const US_DIM_TABS = [['account', '按账号'], ['model', '按模型'], ['realm', '按域']];

function renderUsageDim() {
  const d = usageData || {};
  const m = US_DIMS[usDim] || US_DIMS.account;
  const rows = usSortRows(d[m.key] || [], usSort);
  $('usDimTabs').innerHTML = usTabsHtml(US_DIM_TABS, usDim, {
    account: (d.by_account || []).length,
    model: (d.by_model || []).length,
    realm: (d.by_realm || []).length,
  });
  $('usDimHead').innerHTML = usDimHead(usDim);
  $('usDimBody').innerHTML = rows.map(x => {
    const isAcct = usDim === 'account';
    // 账号行以「昵称」为主标识、uid8 为副标识——与账号池那张表保持一致。
    // 旧实现反过来（把 uid 截 8 位当主标识、昵称塞进副行），uid 前缀相同的账号
    // 在表里会全部显示成同一个 "u_demo_0"，根本分不出谁是谁。
    const uidFull = isAcct ? String(x.key || '') : '';
    const nick = isAcct ? String(x.extra || '').trim() : '';
    const primary = isAcct ? (nick || uidFull) : x.key;
    return usRow(primary,
      isAcct && nick ? uidFull : '',
      x,
      isAcct ? '<td class="num">' + esc(x.realm || '') + '</td>' : '',
      m.withPerf,
      isAcct ? remarkName(x.key, esc(primary)) : '');
  }).join('') || '<tr><td colspan="' + m.span + '"><div class="empty inline">暂无数据</div></td></tr>';
  $('usDimNote').textContent = rows.length + ' 行 · 请求数含失败尝试 · 官方价按牌价折算';
}

/* 积分扣除：按账号 / 按模型两个维度共用一张表（同上，两个 box 合成一个）。 */
function usCreditHead(dim) {
  return dim === 'model'
    ? '<tr><th class="mark" aria-hidden="true"></th><th>模型</th><th>积分倍率</th>' +
      '<th class="num">请求</th><th class="num">扣除积分</th>' +
      '<th class="num">有效样本 Token</th><th class="num">积分 / 1M Token</th><th class="num">缓存命中率</th></tr>'
    : '<tr><th class="mark" aria-hidden="true"></th><th>账号</th>' +
      '<th class="num">请求</th><th class="num">扣除积分</th>' +
      '<th class="num">有效样本 Token</th><th class="num">积分 / 1M Token</th><th class="num">缓存命中率</th></tr>';
}

/* 缓存命中率（issue #92）：颜色即健康度——≥90% 绿 / 80–90% 黄 / <80% 红，
   样本不足灰。title 带命中/未命中绝对量，供逐项核对。 */
function cacheRateCell(hit, miss) {
  const h = Number(hit || 0), m = Number(miss || 0), total = h + m;
  if (!total) return '<span class="muted">—</span>';
  const pct = h / total * 100;
  const cls = pct >= 90 ? 'cache-hi' : (pct >= 80 ? 'cache-mid' : 'cache-lo');
  const txt = trimFixed(pct.toFixed(1)) + '%';
  return '<span class="' + cls + '" title="命中 ' + fmtTok(h) + ' / 未命中 ' + fmtTok(m) + ' tok">' + txt + '</span>';
}

function renderCreditDim() {
  const d = usageData || {};
  $('usCreditHead').innerHTML = usCreditHead(usCreditDim);
  const empty = '暂无积分扣除记录；升级前仅含 Token 的历史不会伪造积分。';
  if (usCreditDim === 'model') {
    const models = d.credit_by_model || [];
    $('usCreditBody').innerHTML = models.map(row =>
      '<tr>' +
        '<td class="mark" aria-hidden="true"></td>' +
        '<td>' + esc(row.key || '—') + '</td>' +
        '<td>' + esc(fmtModelRate(row.rate)) + '</td>' +
        '<td class="num">' + fmtTok(row.requests) + '</td>' +
        '<td class="num">' + fmtCredit(row.credits) + '</td>' +
        '<td class="num">' + fmtTok(row.credit_tokens) + '</td>' +
        '<td class="num">' + fmtCreditRatio(row.credits_per_1m_tokens, row.credit_samples, row.credit_tokens) + '</td>' +
        '<td class="num">' + cacheRateCell(row.cache_hit_tokens, row.cache_miss_tokens) + '</td>' +
      '</tr>'
    ).join('') || '<tr><td colspan="8"><div class="empty inline">' + empty + '</div></td></tr>';
  } else {
    const accounts = d.credit_by_account || [];
    $('usCreditBody').innerHTML = accounts.map(row => {
      const uid = String(row.key || '');
      const account = row.nickname || uid.slice(0, 8) || '—';
      return '<tr>' +
        '<td class="mark" aria-hidden="true"></td>' +
        '<td>' + remarkName(uid, esc(account)) + '<div class="note">' + esc(row.realm || '') + ' · ' + esc(uid.slice(0, 8)) + '</div></td>' +
        '<td class="num">' + fmtTok(row.requests) + '</td>' +
        '<td class="num">' + fmtCredit(row.credits) + '</td>' +
        '<td class="num">' + fmtTok(row.credit_tokens) + '</td>' +
        '<td class="num">' + fmtCreditRatio(row.credits_per_1m_tokens, row.credit_samples, row.credit_tokens) + '</td>' +
        '<td class="num">' + cacheRateCell(row.cache_hit_tokens, row.cache_miss_tokens) + '</td>' +
        '</tr>';
    }).join('') || '<tr><td colspan="7"><div class="empty inline">' + empty + '</div></td></tr>';
  }
  $('usCreditTabs').innerHTML = usTabsHtml([['account', '按账号'], ['model', '按模型']], usCreditDim, {
    account: (d.credit_by_account || []).length,
    model: (d.credit_by_model || []).length,
  });
}

function renderUsage(d) {
  usageData = d || {};
  const t = usageData.totals || {};
  const total = Number(t.total_tokens || 0);
  const pt = Number(t.prompt_tokens || 0);
  const ct = Number(t.completion_tokens || 0);
  const reqs = Number(t.requests || 0);
  const errs = Number(t.errors || 0);
  const okRate = reqs ? (reqs - errs) / reqs * 100 : null;
  // 构成条要的是合法 CSS 宽度，usPct 在无样本时返回 "—"，不能直接拼进 style。
  const pctW = (part) => total ? Math.max(0, Math.min(100, Number(part || 0) / total * 100)).toFixed(2) + '%' : '0%';
  // 六张卡：主指标用强调色，completion 用成功色（与图表里的绿柱呼应），
  // 失败/延迟只在有值时上语义色——全绿全黄的仪表盘等于没有重点。
  $('usStats').innerHTML =
    usKpi(fmtTok(reqs), '请求数', 'c-accent',
      errs ? '其中失败 ' + errs + ' 次' : '全部成功') +
    usKpi(fmtTok(total), '总 token', 'c-accent',
      ['prompt ' + usPct(pt, total), 'completion ' + usPct(ct, total)],
      '<div class="kbar"><i class="p" style="width:' + pctW(pt) + '"></i>' +
      '<i class="c" style="width:' + pctW(ct) + '"></i></div>') +
    usKpi(fmtTok(pt), 'prompt', 'c-soft', '占比 ' + usPct(pt, total)) +
    usKpi(fmtTok(ct), 'completion', 'c-ok', '占比 ' + usPct(ct, total)) +
    usKpi(String(errs), '失败尝试', errs ? 'c-warn' : 'c-mute',
      okRate == null ? '—' : (errs ? '成功率 ' + okRate.toFixed(1) + '%' : '成功率 100%')) +
    usKpi(fmtMs(t.avg_latency_ms), '平均延迟', 'c-soft',
      t.avg_tokens_per_second ? '吐字 ' + fmtRate(t.avg_tokens_per_second) : '无速率样本');

  // 卡片、明细表与时序图全部按所选窗口统计（切窗口数字随之变化）；
  // 「全部历史」含 90 天前折叠出的日桶。这里标注当前口径与数据起点。
  const winLabel = trangeLabel('usRange');
  // 服务端回显的实际区间优先（自定义区间下它就是权威口径）；滚动窗口没有回显，
  // 用控件自己的标签。
  const rangeEcho = usageData.window_from
    ? String(usageData.window_from).replace('T', ' ').slice(0, 16) +
      (usageData.window_to ? ' → ' + String(usageData.window_to).replace('T', ' ').slice(0, 16) : ' → 现在')
    : '';
  const note = (rangeEcho || winLabel ? (rangeEcho || winLabel) + ' · ' : '') +
    (usageData.buckets || 0) + ' 个分桶' +
    (usageData.since ? ' · 数据自 ' + (String(usageData.since).length === 13
      ? usageData.since.replace('T', ' ') + ':00'
      : usageData.since.replace('T', ' ')) : '') +
    (usageData.file_bytes ? ' · 文件 ' + (usageData.file_bytes / 1024).toFixed(1) + ' KB' : '');
  $('usNote').textContent = note;
  $('usNote').title = note; // 窄屏单行截断时靠悬停看全

  // 积分扣除的四张卡片与说明。
  $('usCreditStats').innerHTML =
    usKpi(fmtCredit(t.credits), '扣除积分', 'c-accent', '按上游 usage.credit 累计') +
    usKpi(fmtTok(t.credit_tokens), '匹配 Token', 'c-mute', '与积分同时观测到的 Token') +
    usKpi(fmtCreditRatio(t.credits_per_1m_tokens, t.credit_samples, t.credit_tokens),
      '平均积分 / 1M Token', 'c-ok', '越低越划算') +
    usKpi(String(t.credit_samples || 0), '有效积分样本', 'c-mute', '缺字段的历史不参与折算') +
    usKpi(cacheRateText(t.cache_hit_tokens, t.cache_miss_tokens), '缓存命中率', 'c-mute',
      '上游前缀缓存命中 / (命中+未命中)；低命中意味着费用数倍放大');
  $('usCreditNote').textContent =
    (usageData.credit_by_account || []).length + ' 个账号 · ' +
    (usageData.credit_by_model || []).length + ' 个模型倍率分组 · 仅统计与积分同时观测到的 Token';

  renderSaveStats(usageData);
  renderUsageDim();
  renderCreditDim();
  renderUsageChart(usageData.series || []);
}

// 维度切换 / 排序控件。
if ($('usDimTabs')) $('usDimTabs').addEventListener('click', ev => {
  const b = ev.target.closest('button[data-dim]');
  if (!b) return;
  usDim = b.dataset.dim;
  renderUsageDim();
});
if ($('usCreditTabs')) $('usCreditTabs').addEventListener('click', ev => {
  const b = ev.target.closest('button[data-dim]');
  if (!b) return;
  usCreditDim = b.dataset.dim;
  renderCreditDim();
});
if ($('usSort')) $('usSort').onchange = () => {
  usSort = $('usSort').value;
  renderUsageDim();
};
if ($('usSaveMore')) $('usSaveMore').onclick = () => {
  usSaveAll = !usSaveAll;
  renderSaveStats(usageData || {}); // 只重渲染行，不重新请求接口
};

/* renderUsageChart 画堆叠柱状图。
 *
 * x 轴是**真实时间轴**，不是按序号等距。这一点很重要：数据里存在 1 小时的
 * 间隔，也存在 6~8 小时的断档（没请求的时段不产生桶），等距排布会把 8 小时
 * 画得和 1 小时一样宽，让「什么时候用的」完全失真。
 *
 * 另外不再用 preserveAspectRatio="none"：那会把 viewBox 横向拉伸到容器宽度，
 * 柱子和文字都变形。改为固定比例、按容器宽度自适应高度。
 *
 * viewBox 取 1200×200（原 760×180）：SVG 以 width:100% 渲染，高宽比决定实际
 * 高度——旧比例在 1500px 宽的主区里会撑到 ~355px，只有一两根柱子时整块几乎是
 * 空白。宽 viewBox 把同宽度下的高度压到 ~250px，与下方表格的视觉重量相当。
 *
 * 时间轴用本地时间解析（后端返回的就是本地时区），day 点按当天 00:00 参与定位，
 * 与 hour 点在同一个连续轴上——日桶本来就是他那天所有小时的聚合。
 */

/* parsePointTime 把后端的 t 解析成毫秒时间戳。 */
function parsePointTime(p) {
  // hour: "2026-09-16T13"  day: "2026-09-16"
  const s = p.t.length === 13 ? p.t + ':00:00' : p.t + 'T00:00:00';
  const d = new Date(s);
  return isNaN(d.getTime()) ? null : d.getTime();
}

/* fmtTokTimeLabel 时间桶的短标签，与 x 轴刻度同一口径（日桶 MM-DD，小时桶 HH:00）。 */
function fmtTokTimeLabel(p) {
  const d = new Date(p.t);
  return p.scope === 'day'
    ? (d.getMonth() + 1) + '-' + String(d.getDate()).padStart(2, '0')
    : String(d.getHours()).padStart(2, '0') + ':00';
}

/* 图表公用：宽度跟随容器，避免 1200 宽的桌面图在 1024 缩成 0.6 倍、
   或 440 宽的手机图在 768 被放大 1.6 倍。隐藏视图 clientWidth 为 0 时回退。 */
function chartWidth(host, fallback, min, max) {
  const w = Number(host && host.clientWidth) || 0;
  return Math.max(min || 320, Math.min(max || 1400, w || fallback || 1200));
}
function niceStep(max, ticks) {
  const raw = Math.max(1, Number(max) || 1) / Math.max(1, Number(ticks) || 4);
  const pow = Math.pow(10, Math.floor(Math.log10(raw)));
  const norm = raw / pow;
  const step = norm <= 1 ? 1 : norm <= 2 ? 2 : norm <= 2.5 ? 2.5 : norm <= 5 ? 5 : 10;
  return step * pow;
}
function fmtTokTick(v) {
  const n = Number(v) || 0;
  if (n >= 1e9) return (n / 1e9).toFixed(n >= 10e9 ? 0 : 1).replace(/\.0$/, '') + 'B';
  if (n >= 1e6) return Math.round(n / 1e6) + 'M';
  if (n >= 1e3) return Math.round(n / 1e3) + 'k';
  return String(Math.round(n));
}
/* 跨天窗口只写 HH:00 会看起来像同一天内倒序（16:00 → 15:00 → …）。
   跨度跨自然日或超过 20h 时把 MM-DD 带上；日桶本身只显示 MM-DD。 */
function fmtAxisTimeLabel(ms, t0, t1, scope) {
  const d = new Date(ms);
  const p2 = n => String(n).padStart(2, '0');
  const md = p2(d.getMonth() + 1) + '-' + p2(d.getDate());
  if (scope === 'day') return md;
  const sameDay = new Date(t0).toDateString() === new Date(t1).toDateString();
  return sameDay && Math.abs(t1 - t0) <= 20 * 3600000
    ? p2(d.getHours()) + ':00'
    : md + ' ' + p2(d.getHours()) + ':00';
}

function renderUsageChart(series) {
  const host = $('usChart');

  // 丢掉时间解析不出来的点，而不是让 NaN 传染整张图。
  const pts = [];
  for (const p of series) {
    const t = parsePointTime(p);
    if (t === null) continue;
    const pt = Number(p.prompt_tokens || 0);
    const ct = Number(p.completion_tokens || 0);
    pts.push({ t, scope: p.scope, raw: p.t, pt, ct, tt: Number(p.total_tokens || 0) || (pt + ct),
               req: p.requests || 0 });
  }
  if (!pts.length) {
    host.innerHTML = '<div class="us-empty">暂无用量数据。发起一次对话后再刷新。</div>';
    $('usChartNote').textContent = '—';
    return;
  }

  const W = chartWidth(host, 1200, 760, 1400), H = 200, PL = 58, PR = 14, PT = 18, PB = 30;
  const iw = W - PL - PR, ih = H - PT - PB;

  const t0 = pts[0].t;
  const t1 = pts[pts.length - 1].t;
  const span = Math.max(1, t1 - t0);

  const max = Math.max(1, ...pts.map(p => p.tt));
  const step = niceStep(max, 4);
  const axisMax = step * 4;
  const peak = pts.reduce((a, b) => (b.tt > a.tt ? b : a), pts[0]);
  const avg = pts.reduce((s, p) => s + p.tt, 0) / pts.length;
  const chartNote = pts.length + ' 个点 · 峰值 ' + fmtTok(peak.tt) + ' @ ' +
    fmtAxisTimeLabel(peak.t, t0, t1, peak.scope) + ' · 均值 ' + fmtTok(avg);
  // completion 通常只占 prompt 的千分之几：与 prompt 同轴时柱高接近 0，图例却并列
  // 展示两个系列，读者会以为图没画出来。占比 <1% 时在 note 里点明「不是没数据，
  // 是量级差」，避免误判成图表故障。
  const ctSum = pts.reduce((s, p) => s + p.ct, 0);
  const ttSum = pts.reduce((s, p) => s + p.tt, 0);
  const ctPct = ttSum > 0 ? ctSum / ttSum * 100 : 0;
  const mixNote = ctPct > 0 && ctPct < 1
    ? ' · completion 占 ' + ctPct.toFixed(1) + '%（同轴几乎不可见）'
    : '';
  $('usChartNote').textContent = chartNote + mixNote;

  // 柱宽取「最小真实间隔」的 70%，并夹在合理区间内——窗口拉到 30 天时柱子会
  // 变细，但不会细到看不见。
  let minGap = Infinity;
  for (let i = 1; i < pts.length; i++) minGap = Math.min(minGap, pts[i].t - pts[i - 1].t);
  if (!isFinite(minGap) || minGap <= 0) minGap = span;
  const slot = iw * (minGap / span);
  const bw = Math.max(2, Math.min(30, slot * 0.7));

  // 首尾各让出半个柱宽：否则第一个点和最后一个点的柱子会各有一半跑到绘图区外
  // （末点柱子贴着卡片右边缘被切掉），刻度仍用同一个 xOf，标签与柱子始终对齐。
  const xOf = t => PL + bw / 2 + (t - t0) / span * Math.max(1, iw - bw);
  const yOf = v => PT + ih - ih * (v / axisMax);

  let out = '<svg viewBox="0 0 ' + W + ' ' + H + '" role="img" aria-label="Token 时序图：' +
            pts.length + ' 个数据点" preserveAspectRatio="xMidYMid meet">';

  // 柱体渐变：顶部实、底部略透，堆叠时两段仍能一眼分清（纯色块并排会糊成一片）。
  // 注意 stop-color 必须走 style 而不是 presentation 属性——Blink/WebKit 不解析
  // 属性里的 var()，写成 stop-color="var(--accent)" 会整条渐变失效（柱子全透明）。
  out += '<defs>' +
    '<linearGradient id="usGradP" x1="0" y1="0" x2="0" y2="1">' +
    '<stop offset="0" style="stop-color:var(--accent);stop-opacity:1"/>' +
    '<stop offset="1" style="stop-color:var(--accent);stop-opacity:.6"/></linearGradient>' +
    '<linearGradient id="usGradC" x1="0" y1="0" x2="0" y2="1">' +
    '<stop offset="0" style="stop-color:var(--ok);stop-opacity:1"/>' +
    '<stop offset="1" style="stop-color:var(--ok);stop-opacity:.6"/></linearGradient>' +
    '</defs>';

  // y 轴网格 + 刻度
  for (let i = 0; i <= 4; i++) {
    const y = PT + ih - (ih * i / 4);
    out += '<line class="gl" x1="' + PL + '" y1="' + y.toFixed(1) + '" x2="' + (W - PR) +
           '" y2="' + y.toFixed(1) + '"/>';
    out += '<text class="tk" x="' + (PL - 6) + '" y="' + (y + 3.5).toFixed(1) +
           '" text-anchor="end">' + fmtTokTick(axisMax * i / 4) + '</text>';
  }

  // 跨天日期分隔线画在柱子之前：画在之后会横穿柱体，像把柱子切成两半。
  let prevDay = null;
  for (const p of pts) {
    const d = new Date(p.t).getDate();
    if (prevDay !== null && d !== prevDay) {
      const x = xOf(p.t).toFixed(1);
      out += '<line class="gl" x1="' + x + '" y1="' + PT + '" x2="' + x + '" y2="' +
             (PT + ih) + '" style="opacity:.28;pointer-events:none"/>';
    }
    prevDay = d;
  }

  // 均值参考线挪到柱子之后绘制（见下方），否则柱高的时段会把虚线完全盖住。

  // 柱子
  const yBase = PT + ih;
  for (const p of pts) {
    const x = xOf(p.t) - bw / 2;
    const hTot = ih * (p.tt / axisMax);
    const hP = p.tt ? hTot * (p.pt / p.tt) : 0;
    const hC = Math.max(p.tt && p.ct ? 1 : 0, hTot - hP);
    // 每根柱子包一个 <g>，把 <title> 放进去。
    //
    // 为什么必须包裹：SVG 里 <title> 描述的是它的**父元素**。此前 <title> 是
    // <rect> 的兄弟节点（rect 自闭合，无法包含子节点），于是全部平铺在 <svg>
    // 根下 —— 整张图只有一个 tooltip（浏览器取第一个），悬停任何柱子都显示同一
    // 份数据（issue #128）。包进 <g> 后 tooltip 跟随该柱，且堆叠的两段
    //（prompt + completion）共用同一个提示。
    out += '<g><title>' + esc(p.raw) + '  ' + fmtTok(p.pt) + ' prompt / ' +
           fmtTok(p.ct) + ' completion / ' + p.req + ' 次</title>';
    // 圆角只给堆叠顶端（贴轴的底边保持方角，柱子才像"立"在基线上）。
    // 类名用 usbar 而不是 bar：账号池的积分条是 .bar{height:3px}，而 SVG2 里
    // height 是 rect 的 CSS 几何属性，同名类会把每根柱子压成 3px 高（踩过）。
    if (hP > 0) out += '<rect class="usbar" x="' + x.toFixed(2) + '" y="' + (yBase - hP).toFixed(2) +
      '" width="' + bw.toFixed(2) + '" height="' + hP.toFixed(2) +
      '" fill="url(#usGradP)"' + (hC > 0 ? '' : ' rx="1.5"') + '/>';
    if (hC > 0) out += '<rect class="usbar" x="' + x.toFixed(2) + '" y="' + (yBase - hP - hC).toFixed(2) +
      '" width="' + bw.toFixed(2) + '" height="' + hC.toFixed(2) +
      '" fill="url(#usGradC)" rx="1.5"/>';
    out += '</g>';
  }

  // 均值参考线画在柱子之后：先画会被高柱完全盖住，虚线等于不存在。
  // v3：图内不再写"均值/峰值"文字——usChartNote 已经给出两者。
  if (avg > 0 && avg < axisMax) {
    const y = yOf(avg);
    out += '<line class="avg" x1="' + PL + '" y1="' + y.toFixed(1) + '" x2="' + (W - PR) +
           '" y2="' + y.toFixed(1) + '" style="pointer-events:none"/>';
  }

  // x 轴基线画在柱子之后，避免压在柱底
  out += '<line class="ax" x1="' + PL + '" y1="' + yBase + '" x2="' + (W - PR) +
         '" y2="' + yBase + '"/>';

  // x 轴刻度：按真实时间等距取 6 个位置，取该位置**最近的实际柱子**做标签，
  // 所以标签永远落在有数据的点上，不会指到空档里。
  const TICKS = Math.min(W < 420 ? 2 : (W < 700 ? 3 : (W < 900 ? 5 : 6)), pts.length);
  const usedLabel = new Set();
  for (let k = 0; k < TICKS; k++) {
    const target = t0 + span * (TICKS === 1 ? 0.5 : k / (TICKS - 1));
    let bi = 0, best = Infinity;
    for (let i = 0; i < pts.length; i++) {
      const d = Math.abs(pts[i].t - target);
      if (d < best) { best = d; bi = i; }
    }
    if (usedLabel.has(bi)) continue;
    usedLabel.add(bi);
    const p = pts[bi];
    // 首尾标签靠边对齐，避免被裁掉
    const cx = xOf(p.t);
    const anchor = cx < PL + 14 ? 'start' : (cx > W - PR - 14 ? 'end' : 'middle');
    out += '<text class="tk" x="' + Math.max(PL, Math.min(W - PR, cx)).toFixed(1) +
           '" y="' + (PT + ih + 15) + '" text-anchor="' + anchor + '">' +
           esc(fmtAxisTimeLabel(p.t, t0, t1, p.scope)) + '</text>';
  }

  out += '</svg>';
  host.innerHTML = out;
}

function fmtTokTip(v) { return fmtTok(v); }

let usageRateWarmAt = 0;
async function warmUsageModelRates() {
  if (Date.now() - usageRateWarmAt < 10 * 60 * 1000) return;
  try {
    await api('models');
  } catch (e) {
    // 倍率回填是可选增强；失败不阻塞用量统计，10 分钟后再试。
  }
  usageRateWarmAt = Date.now();
}

async function loadUsage() {
  const q = trangeQuery('usRange', true);
  try {
    await warmUsageModelRates();
    const d = await api('usage?' + q.toString());
    renderUsage(d);
  } catch (e) {
    // 失败时三块都要清干净：只改图表会留下上一次窗口的数字，看起来像"刷新成功"。
    usageData = null;
    $('usChart').innerHTML = '<div class="us-empty">读取用量失败：' + esc(shortErr(e.message)) + '</div>';
    $('usChartNote').textContent = '—';
    $('usStats').innerHTML = '';
    $('usCreditStats').innerHTML = '';
    $('usNote').textContent = '—';
    $('usCreditNote').textContent = '—';
    $('usDimNote').textContent = '—';
    $('usDimHead').innerHTML = '';
    $('usCreditHead').innerHTML = '';
    $('usDimTabs').innerHTML = usTabsHtml(US_DIM_TABS, usDim, null);
    $('usCreditTabs').innerHTML = usTabsHtml([['account', '按账号'], ['model', '按模型']], usCreditDim, null);
    $('usDimBody').innerHTML = '<tr><td colspan="10"><div class="empty inline">读取用量失败</div></td></tr>';
    $('usCreditBody').innerHTML = '<tr><td colspan="7"><div class="empty inline">读取用量失败</div></td></tr>';
  }
}

if ($('btnUsage')) $('btnUsage').onclick = loadUsage;
// 时间范围控件绑定：任何改动（预设切换 / 自定义起止）都重新拉一次用量。
if ($('usRange')) trangeBind('usRange', loadUsage);

/* ── 积分构成 ─────────────────────────────────────────────────────── */
/* 一个账号的余额是若干积分包之和。包按来源命名（「国内运营裂变包」「拉新权益包」
   「个人体验版」…），面额从 6 到 1500 不等，且**按次发放**。所以两个任务完成度
   完全一致的账号，余额可能差上千——差别只在包里。这里把逐包明细摊开，并给每个
   包名一个稳定配色，跨账号对比时同色即同类。 */

/* v3：色板走 CSS 令牌，深/浅主题各一套（浅色在白底上对比更足）。
   返回 var(...) 字符串，可直接塞进 background / --seg-color 等内联属性。 */
const PK_COLORS = ['var(--chart-1)', 'var(--chart-2)', 'var(--chart-3)', 'var(--chart-4)',
                   'var(--chart-5)', 'var(--chart-6)', 'var(--chart-7)', 'var(--chart-8)',
                   'var(--chart-9)', 'var(--chart-10)'];
const PK_ACCOUNT_COLORS = ['var(--chart-1)', 'var(--chart-2)', 'var(--chart-3)', 'var(--chart-4)',
                           'var(--chart-5)', 'var(--chart-11)', 'var(--chart-7)', 'var(--chart-10)',
                           'var(--chart-12)', 'var(--chart-8)', 'var(--chart-9)', 'var(--chart-6)'];

function pkColor(i) { return PK_COLORS[i % PK_COLORS.length]; }

// pkAccountColorMap 按 UID 稳定分配颜色：排序后分配，账号刷新/重排不会换色。
function pkAccountColorMap(list) {
  const uids = (list || [])
    .filter(a => a && !a.error && a.uid)
    .map(a => String(a.uid))
    .sort();
  const colors = new Map();
  uids.forEach((uid, i) => colors.set(uid, PK_ACCOUNT_COLORS[i % PK_ACCOUNT_COLORS.length]));
  return colors;
}

/* pkBySource 把包按名称归并，得到「来源 → 面额/余额/个数」。这是对比的关键视图：
   两个号的差异一定体现在某几个来源的面额上。 */
function pkBySource(packs) {
  const m = new Map();
  for (const p of packs) {
    // 分组键用 code + name，而不是只 name：上游给「首登赠送」和普通活动包用了
    // **同一个 PackageName 和同一个 PackageCode**，只按 name 会把两类混成一类，
    // 那正是当初「两个号为何差 1500」看不出来的原因。这里至少把 code 带进键里，
    // 并在卡片上显示最早的发放时间。
    const k = (p.package_code || '') + '|' + (p.name || '(未命名)');
    const e = m.get(k) || {
      key: k, name: p.name || '(未命名)', code: p.package_code || '',
      n: 0, remain: 0, size: 0, used: 0, minEnd: '', minCreated: '',
    };
    e.n += 1;
    e.remain += Number(p.remain || 0);
    e.size += Number(p.size || 0);
    e.used += Number(p.used || 0);
    const t = (p.end_time || '').slice(0, 10);
    if (t && (!e.minEnd || t < e.minEnd)) e.minEnd = t;
    const c = (p.created_at || '').slice(0, 10);
    if (c && (!e.minCreated || c < e.minCreated)) e.minCreated = c;
    m.set(k, e);
  }
  return [...m.values()].sort((a, b) => b.size - a.size);
}

const PK_DEFAULT_DETAIL_LIMIT = 5;

function pkDetailLimitValue(raw) {
  const n = Number(raw);
  return Number.isFinite(n) && n > 0 ? Math.floor(n) : PK_DEFAULT_DETAIL_LIMIT;
}

function pkDetailLimit(cfg) {
  return pkDetailLimitValue(cfg && cfg.panel && cfg.panel.package_detail_limit);
}

const PK_DAY_MS = 24 * 3600 * 1000;

function pkExpiryMs(p) {
  const raw = Number(p && p.expires_at);
  if (Number.isFinite(raw) && raw > 0) return raw;
  const text = String((p && p.end_time) || '').trim();
  if (!text) return null;
  let iso = text.includes('T') ? text : text.replace(' ', 'T');
  if (!/(?:Z|[+-]\d\d:\d\d)$/.test(iso)) iso += '+08:00';
  const parsed = Date.parse(iso);
  return Number.isFinite(parsed) ? parsed : null;
}

// pkDetailCompare 只服务单账号逐包明细：正余额包先按到期时间挑选默认展示项，
// 其余正余额包与已用完包分别折叠；同一到期时间按面额降序。主键跟随视图排序
// 模式（pkSortMode，声明在本区块末尾的绑定块）：end_asc 到期升序、size_desc
// 面额降序（同面额按到期升序）。typeof 守卫：前端 harness 的区域切片求值里
// 没有该全局，回落 end_asc（= 上游原有语义，切片测试的期望序不受影响）。
function pkDetailCompare(a, b) {
  const sizeOf = p => {
    const n = Number(p && p.size);
    return Number.isFinite(n) ? n : 0;
  };
  const mode = (typeof pkSortMode === 'string' && pkSortMode) || 'end_asc';
  if (mode === 'size_desc') {
    const d = sizeOf(b) - sizeOf(a);
    if (d !== 0) return d;
  }
  const ea = pkExpiryMs(a), eb = pkExpiryMs(b);
  if (ea == null && eb != null) return 1;
  if (ea != null && eb == null) return -1;
  if (ea != null && eb != null && ea !== eb) return ea - eb;
  return sizeOf(b) - sizeOf(a);
}

function pkDetailGroups(packs, limit) {
  const active = [], used = [];
  let usedSize = 0, restSize = 0, restRemain = 0;
  for (const p of packs || []) {
    const remain = Number(p && p.remain);
    if (remain > 0) {
      active.push(p);
      continue;
    }
    used.push(p);
    const size = Number(p && p.size);
    if (Number.isFinite(size)) usedSize += size;
  }
  active.sort(pkDetailCompare);
  used.sort(pkDetailCompare);
  const visible = active.slice(0, pkDetailLimitValue(limit));
  const rest = active.slice(visible.length);
  for (const p of rest) {
    const size = Number(p && p.size);
    if (Number.isFinite(size)) restSize += size;
    const remain = Number(p && p.remain);
    if (Number.isFinite(remain)) restRemain += remain;
  }
  return { visible, rest, used, restSize, restRemain, usedSize };
}

function pkCreditOpacity(days) {
  if (days == null || !Number.isFinite(Number(days))) return 1;
  return 0.25 + 0.75 * Math.max(0, Math.min(29, Number(days) - 1)) / 29;
}

function pkExpiryText(expiresAt) {
  if (!expiresAt) return '无到期时间';
  const diff = expiresAt - Date.now();
  if (diff <= 0) return '已到期';
  const minutes = Math.max(1, Math.ceil(diff / 60000));
  if (minutes < 60) return '剩余 ' + minutes + ' 分钟';
  const hours = Math.ceil(diff / 3600000);
  if (hours < 24) return '剩余 ' + hours + ' 小时';
  return '剩余 ' + Math.ceil(diff / PK_DAY_MS) + ' 天';
}

function pkExpiryDateTime(expiresAt) {
  if (!expiresAt) return '—';
  return new Date(expiresAt).toLocaleString('zh-CN', {
    timeZone: 'Asia/Shanghai', hour12: false,
    year: 'numeric', month: '2-digit', day: '2-digit',
    hour: '2-digit', minute: '2-digit', second: '2-digit',
  });
}

function pkAccountSegments(a, now) {
  let balance = Math.max(0, Number(a.remain || 0));
  const out = [];
  for (const p of a.packages || []) {
    const remain = Number(p.remain || 0);
    if (!Number.isFinite(remain) || remain <= 0 || balance <= 0) continue;
    const amount = Math.min(balance, remain);
    const expiresAt = pkExpiryMs(p);
    out.push({
      amount,
      expiresAt,
      days: expiresAt == null ? null : Math.max(0, Math.ceil((expiresAt - now) / PK_DAY_MS)),
      source: p.name || '积分',
      uid: String(a.uid || ''),
      accountName: a.nickname || String(a.uid || '').slice(0, 8) || '未命名账号',
    });
    balance -= amount;
  }
  return out.sort((x, y) => {
    if (x.expiresAt == null && y.expiresAt != null) return 1;
    if (x.expiresAt != null && y.expiresAt == null) return -1;
    return (x.expiresAt || 0) - (y.expiresAt || 0);
  });
}

// summarizeCreditDays 对齐 WorkDaddy：按精确剩余天数逐行聚合，无有效到期时间的余额
// 不进入图表，也不猜测到期日。账号内先按总余额约束逐包金额，避免上游重复记录膨胀。
function summarizeCreditDays(list, now) {
  const buckets = new Map();
  let unavailable = 0;
  for (const a of list || []) {
    if (a.error || !Number.isFinite(Number(a.remain))) {
      unavailable++;
      continue;
    }
    for (const segment of pkAccountSegments(a, now)) {
      if (segment.days == null) continue;
      let row = buckets.get(segment.days);
      if (!row) {
        row = { days: segment.days, credits: 0, segments: [] };
        buckets.set(segment.days, row);
      }
      row.credits += segment.amount;
      row.segments.push(segment);
    }
  }
  const rows = [...buckets.values()].sort((a, b) => a.days - b.days);
  for (const row of rows) {
    row.segments.sort((a, b) =>
      (a.expiresAt || Infinity) - (b.expiresAt || Infinity) ||
      a.accountName.localeCompare(b.accountName) ||
      a.source.localeCompare(b.source));
  }
  return { rows, accountCount: (list || []).length, unavailable };
}

function renderExpiryDistribution(list, now) {
  const summary = summarizeCreditDays(list, now);
  const accounts = (list || []).filter(a => a && !a.error && a.uid);
  const byUid = new Map(accounts.map(a => [String(a.uid), a]));
  const nameCounts = typeof acctNameCounts === 'function' ? acctNameCounts(accounts) : new Map();
  // 配色只保留前 8 个账号：19 个账号塞进 12 色令牌必然重复，重复色比灰色更误导。
  const totals = new Map();
  for (const row of summary.rows) for (const seg of row.segments) {
    totals.set(seg.uid, (totals.get(seg.uid) || 0) + seg.amount);
  }
  const topUids = [...totals.entries()].sort((a, b) => b[1] - a[1]).slice(0, 8)
    .map(([uid]) => uid);
  const chartColors = new Map(topUids.map((uid, i) => [uid, PK_ACCOUNT_COLORS[i]]));
  const near = summary.rows.filter(r => r.days <= 90);
  const long = summary.rows.filter(r => r.days > 90);
  const displayRows = near.length ? near : summary.rows;
  const maxCredits = Math.max(1, ...displayRows.map(r => r.credits || 0));
  const rows = displayRows.map(row => {
    const total = row.credits || 1;
    const width = Math.max(1.5, Math.min(100, row.credits / maxCredits * 100));
    const nodes = row.segments.map(segment => {
      const color = chartColors.get(segment.uid) || 'var(--ink-3)';
      const title = segment.source + '\n' + fmtTok(segment.amount) + ' 积分\n到期时间 ' +
        pkExpiryDateTime(segment.expiresAt) + '（' + pkExpiryText(segment.expiresAt) + '）\n' +
        segment.accountName;
      return '<span class="pk-expiry-seg" style="--seg-color:' + color +
        ';opacity:' + pkCreditOpacity(segment.days).toFixed(5) +
        ';flex:' + Math.max(0.008, segment.amount / total).toFixed(4) +
        ' 1 0" title="' + esc(title) + '" aria-label="' + esc(title) + '"></span>';
    }).join('');
    return '<div class="pk-expiry-row" title="' + esc(fmtTok(row.credits) + ' 积分 · 该档 ' + row.segments.length + ' 段') +
      '"><span>' + esc(row.days === 0 ? '已到期' : row.days + ' 天') +
      '</span><div class="pk-expiry-lane"><div class="pk-expiry-track" style="width:' +
      width.toFixed(2) + '%">' + nodes + '</div></div><b>' + esc(fmtTok(row.credits)) +
      '</b></div>';
  }).join('');
  const longCredits = long.reduce((s, r) => s + r.credits, 0);
  const foot = summary.accountCount + ' 个账号' +
    (summary.unavailable ? ' · ' + summary.unavailable + ' 个未获取余额' : '') +
    (long.length && near.length
      ? ' · 90 天以上 ' + long.length + ' 档 / ' + fmtTok(longCredits) + ' 积分（已折叠）'
      : '') + ' · 条长 = 该档积分合计';
  const legend = topUids.map(uid => '<span><i style="background:' + chartColors.get(uid) +
    '"></i>' + esc(typeof acctDisplayName === 'function'
      ? acctDisplayName(byUid.get(uid) || {}, nameCounts)
      : ((byUid.get(uid) || {}).nickname || uid)) + '</span>').join('') +
    (totals.size > topUids.length
      ? '<span><i style="background:var(--ink-3)"></i>其他 ' + (totals.size - topUids.length) + ' 个账号</span>'
      : '');
  const hdr = '<div class="pk-expiry-hdr"><span>剩余天数</span><span class="pk-expiry-mid">各账号构成 · 条长=该档总量</span><b>剩余积分</b></div>';
  $('pkExpiry').innerHTML = (rows
    ? hdr + '<div class="pk-expiry-chart">' + rows + '</div>'
    : '<div class="pk-expiry-empty">暂无可汇总积分</div>') +
    (legend ? '<div class="pk-expiry-legend">' + legend + '</div>' : '') +
    '<div class="pk-expiry-foot">' + esc(foot) + '</div>';
}

function renderPackages(d, detailLimit) {
  const list = (d.accounts || []);
  const nameCounts = typeof acctNameCounts === 'function' ? acctNameCounts(list) : new Map();
  const disp = a => typeof acctDisplayName === 'function'
    ? acctDisplayName(a, nameCounts)
    : ((a && (a.nickname || a.uid)) || '');
  const now = Date.now();
  const expiryColors = pkAccountColorMap(list);
  renderExpiryDistribution(list, now);
  if (!list.length) {
    $('pkSummary').innerHTML = '<div class="empty">没有账号</div>';
    $('pkDetail').innerHTML = '';
    if ($('pkAcctSel')) $('pkAcctSel').innerHTML = '';
    return;
  }

  // 包名 → 稳定色号（跨账号一致，方便肉眼对齐）
  const names = [];
  for (const a of list) for (const s of pkBySource(a.packages || [])) {
    if (!names.includes(s.key)) names.push(s.key);
  }
  names.sort((x, y) => {
    const sz = n => Math.max(...list.map(a => {
      const f = pkBySource(a.packages || []).find(s => s.key === n);
      return f ? f.size : 0;
    }));
    return sz(y) - sz(x);
  });
  const colorOf = n => pkColor(names.indexOf(n));
  // 键 → 展示名，供卡片与明细表共用（同一来源必然同色同名）。
  const labelOf = {};
  for (const a of list) for (const s of pkBySource(a.packages || [])) labelOf[s.key] = s;

  const maxRemain = Math.max(1, ...list.map(a => Number(a.remain || 0)));
  const selectable = list.filter(a => !a.error && a.uid);
  if (!selectable.some(a => String(a.uid) === String(pkSelectedUid))) {
    pkSelectedUid = selectable[0] ? String(selectable[0].uid) : '';
  }
  const pkSel = $('pkAcctSel');
  if (pkSel) {
    pkSel.innerHTML = selectable.map(a => '<option value="' + esc(String(a.uid)) + '"' +
      (String(a.uid) === String(pkSelectedUid) ? ' selected' : '') + '>' +
      esc(disp(a)) + '</option>').join('');
    pkSel.hidden = !selectable.length;
  }

  $('pkSummary').innerHTML = list.map(a => {
    if (a.error) {
      return '<div class="pk-card"><div class="who"><span class="nm">' +
        esc(disp(a)) + '</span>' +
        '<span class="realm">' + esc(a.realm || '') + '</span></div>' +
        '<div class="err">查询失败：' + esc(shortErr(a.error)) + '</div></div>';
    }
    const srcs = pkBySource(a.packages || []);
    const total = Math.max(1, Number(a.size || 0));
    const bar = srcs.map(s =>
      '<i style="width:' + (s.size / total * 100).toFixed(2) + '%;background:' +
      colorOf(s.key) + '" title="' + esc(s.name) + ' ' + fmtTok(s.size) + '"></i>'
    ).join('');
    const legend = srcs.map(s =>
      '<span><i style="background:' + colorOf(s.key) + '"></i>' +
      esc(s.name.replace(/^CodeBuddy/, '')) + ' x' + s.n + ' · ' + fmtTok(s.size) +
      (s.minCreated ? ' · 首发 ' + esc(s.minCreated.slice(5)) : '') + '</span>'
    ).join('');
    const expiry = pkAccountSegments(a, now);
    const expiryTotal = Math.max(1, expiry.reduce((sum, s) => sum + s.amount, 0));
    const expiryColor = expiryColors.get(String(a.uid)) || 'var(--accent)';
    const expiryBar = expiry.length ? '<div class="expirybar" role="img" aria-label="积分到期分布">' +
      expiry.map(s => {
        const title = s.source + '\n' + fmtTok(s.amount) + ' 积分\n到期时间 ' +
          pkExpiryDateTime(s.expiresAt) + '（' + pkExpiryText(s.expiresAt) + '）';
        return '<i style="background:' + expiryColor +
          ';opacity:' + pkCreditOpacity(s.days).toFixed(5) +
          ';flex:' + Math.max(0.008, s.amount / expiryTotal).toFixed(4) +
          ' 1 0" title="' + esc(title) + '"></i>';
      }).join('') + '</div>' : '';
    const isSel = String(a.uid) === String(pkSelectedUid);
    return '<div class="pk-card' + (isSel ? ' on' : '') + '" data-pk-uid="' + esc(String(a.uid)) +
      '" role="button" tabindex="0" aria-pressed="' + (isSel ? 'true' : 'false') + '">' +
      '<div class="who"><span class="nm">' + esc(disp(a)) + '</span>' +
      '<span class="realm">' + esc(a.realm || '') + '</span></div>' +
      '<div class="big">' + fmtTok(a.remain) + '</div>' +
      '<div class="sub">共 ' + fmtTok(a.size) + ' · ' + (a.packages || []).length +
      ' 个包 · 占最高 ' + (Number(a.remain || 0) / maxRemain * 100).toFixed(0) + '%</div>' +
      '<div class="mixbar">' + bar + '</div>' +
      expiryBar +
      '<div class="pk-legend">' + legend + '</div>' +
      '</div>';
  }).join('');

  $('pkNote').textContent = list.length + ' 个账号 · 点击账号看逐包明细';

  // 逐包明细：只渲染当前选中账号。19 个账号全量铺开会让页面到 11k–19k px，
  // 而用户同一时刻只会看一个账号的包；筛选/排序由视图顶部选择器控制。
  $('pkDetail').innerHTML = selectable.filter(a => String(a.uid) === String(pkSelectedUid)).map(a => {
    const groups = pkDetailGroups(a.packages || [], detailLimit);
    const rowOf = (p, rowGroup) => {
      const k = (p.package_code || '') + '|' + (p.name || '(未命名)');
      const sub = (p.sub_product_code || '').replace(/^sp_tcaca_codebuddyide_?/, '') ||
                  (p.package_code || '').replace(/^TCACA_/, '');
      return '<tr' + (rowGroup ? ' class="pk-hidden-row pk-' + rowGroup +
        '-row" data-pk-row="' + rowGroup + '" hidden' : '') +
        '><td class="mark" aria-hidden="true"><i style="background:' +
        colorOf(k) + '"></i></td>' +
      '<td>' + esc(p.name || '(未命名)') +
        (sub ? '<div class="note">' + esc(sub) + '</div>' : '') + '</td>' +
      '<td class="num">' + fmtTok(p.size) + '</td>' +
      '<td class="num">' + fmtTok(p.remain) + '</td>' +
      '<td class="num">' + fmtTok(p.used) + '</td>' +
      '<td class="num">' + esc((p.created_at || '').slice(0, 16).replace('T', ' ') || '—') + '</td>' +
      '<td class="num">' + esc((p.end_time || '').slice(0, 10) || '—') + '</td>' +
      '</tr>';
    };
    const groupSummary = (group, label, count, size, remain) =>
      '<tr class="pk-group-summary"><td colspan="7"><button type="button" class="pk-group-toggle"' +
      ' data-pk-group="' + group + '" data-count="' + count + '" data-size="' + size +
      '" data-remain="' + remain + '" aria-expanded="false">' + label + '，展开</button></td></tr>';
    const rows = groups.visible.map(p => rowOf(p, '')).join('');
    const restSummary = groups.rest.length
      ? groupSummary('rest', '其余未用完 ' + groups.rest.length + ' 个包（面额合计 ' +
          fmtTok(groups.restSize) + ' · 剩余 ' + fmtTok(groups.restRemain) + '）',
          groups.rest.length, groups.restSize, groups.restRemain) +
        groups.rest.map(p => rowOf(p, 'rest')).join('')
      : '';
    const usedSummary = groups.used.length
      ? groupSummary('used', '已用完 ' + groups.used.length + ' 个包（面额合计 ' +
          fmtTok(groups.usedSize) + '）', groups.used.length, groups.usedSize, 0) +
        groups.used.map(p => rowOf(p, 'used')).join('')
      : '';
    return '<div class="box"><header><h3>' +
      esc(disp(a)) + ' · ' + esc(a.realm || '') +
      '</h3><span class="grow"></span><span class="note">余额 ' + fmtTok(a.remain) +
      ' / 总额 ' + fmtTok(a.size) + ' · 可用 ' + (groups.visible.length + groups.rest.length) + ' 个包' +
      (groups.used.length ? ' / 已用完 ' + groups.used.length + ' 个' : '') +
      ' · 最早到期前 ' + pkDetailLimitValue(detailLimit) + ' 条</span>' +
      '</header><div class="tbl-wrap"><table class="acc"><thead><tr>' +
      '<th class="mark" aria-hidden="true"></th><th>包名 / 来源</th>' +
      '<th class="num">面额</th><th class="num">剩余</th><th class="num">已用</th>' +
      '<th class="num">发放</th><th class="num">到期</th>' +
      '</tr></thead><tbody>' + rows + restSummary + usedSummary + '</tbody></table></div></div>';
  }).join('') || '<div class="box"><div class="empty">选择账号后查看逐包明细</div></div>';
}

if ($('pkDetail')) $('pkDetail').addEventListener('click', ev => {
  const btn = ev.target.closest('button[data-pk-group]');
  if (!btn) return;
  const body = btn.closest('tbody');
  if (!body) return;
  const group = btn.dataset.pkGroup;
  const expanded = btn.getAttribute('aria-expanded') === 'true';
  body.querySelectorAll('tr[data-pk-row="' + group + '"]').forEach(row => { row.hidden = expanded; });
  const count = btn.dataset.count || '0';
  const size = btn.dataset.size || '0';
  const remain = btn.dataset.remain || '0';
  btn.setAttribute('aria-expanded', String(!expanded));
  if (group === 'rest') {
    btn.textContent = expanded
      ? '其余未用完 ' + count + ' 个包（面额合计 ' + fmtTok(size) + ' · 剩余 ' +
        fmtTok(remain) + '），展开'
      : '收起其余未用完 ' + count + ' 个包';
  } else {
    btn.textContent = expanded
      ? '已用完 ' + count + ' 个包（面额合计 ' + fmtTok(size) + '），展开'
      : '收起已用完 ' + count + ' 个包';
  }
});

/* 账号摘要卡 = 明细选择器：点击/回车切换 #pkDetail 里唯一的一张表。 */
function pickPkAccount(uid) {
  if (uid == null) return;
  pkSelectedUid = String(uid);
  if (lastPackages) renderPackages(lastPackages, lastDetailLimit);
}
if ($('pkSummary')) {
  $('pkSummary').addEventListener('click', ev => {
    const card = ev.target.closest('[data-pk-uid]');
    if (!card) return;
    ev.preventDefault();
    pickPkAccount(card.dataset.pkUid);
  });
  $('pkSummary').addEventListener('keydown', ev => {
    if (ev.key !== 'Enter' && ev.key !== ' ') return;
    const card = ev.target.closest('[data-pk-uid]');
    if (!card) return;
    ev.preventDefault();
    pickPkAccount(card.dataset.pkUid);
  });
}
if ($('pkAcctSel')) $('pkAcctSel').onchange = () => pickPkAccount($('pkAcctSel').value);

/* ── 逐包明细排序模式 ─────────────────────────────────────────────── */
/* 逐包明细的排序规则（选择持久化在 localStorage，跨会话记住）：
   end_asc   到期升序（默认）——快过期的包排最前，提醒优先消耗；无到期时间的
             包（上游没下发 end_time）没有可比的日期，统一垫底，不掺进日期序里；
   size_desc 面额降序——原来的展示口径，看「钱从哪来」。
   行序统一由 pkDetailCompare 实现（明细折叠分组共用同一比较器，切换模式时
   折叠组内行序同步跟随）。本块整体放在 renderPackages / renderExpiryDistribution
   之后：前端 harness 按区域切片求值（[PK_ACCOUNT_COLORS, renderExpiryDistribution)
   与 [PK_DEFAULT_DETAIL_LIMIT, renderPackages)），顶层 localStorage/$ 语句落进
   切片区会让无 DOM 桩的求值环境 ReferenceError——上游测试的切片边界不动。 */
const LS_PK_SORT = 'pkSortMode';
let pkSortMode = localStorage.getItem(LS_PK_SORT) || 'end_asc';
const PK_SORT_LABELS = { end_asc: '按到期升序 · 近的在前', size_desc: '按面额降序' };
// 排序切换时重排明细需要 detailLimit（上游折叠配置），loadPackages 拉到后缓存。
let lastDetailLimit = PK_DEFAULT_DETAIL_LIMIT;

// 排序规则控件：恢复上次选择并绑定切换。
if ($('pkSort')) {
  $('pkSort').value = pkSortMode;
  if ($('pkSort').value !== pkSortMode) {        // localStorage 里存了废弃值：回落默认
    pkSortMode = 'end_asc';
    localStorage.setItem(LS_PK_SORT, pkSortMode);   // 不用 removeItem：harness 桩无此方法
  }
  $('pkSort').onchange = () => {
    pkSortMode = $('pkSort').value;
    localStorage.setItem(LS_PK_SORT, pkSortMode);
    if (lastPackages) renderPackages(lastPackages, lastDetailLimit);   // 数据在内存，直接重排
  };
}

// loadPackages 按需拉取：有缓存（上次任意入口拉到的 packages 数据）先直接渲染，
// 不再「切进积分构成页就打一次上游」——逐账号实时查询在账号多时既慢又压上游。
// 无缓存（首次进入）才自动查一次；显式刷新（force=true，点「刷新」按钮）始终实时查。
async function loadPackages(force) {
  if (!force && lastPackages) {
    renderPackages(lastPackages, lastDetailLimit);
    const ageMin = Math.floor((Date.now() - lastPackagesAt) / 60000);
    if (ageMin > 0) $('pkNote').textContent = lastPackages.accounts.length + ' 个账号 · ' + ageMin + ' 分钟前的数据，点「刷新」更新';
    return;
  }
  $('pkSummary').innerHTML = '<div class="empty">查询中…（逐账号向上游实时查询）</div>';
  $('pkDetail').innerHTML = '';
  $('pkExpiry').innerHTML = '<div class="pk-expiry-empty">查询中…</div>';
  try {
    const [d, c] = await Promise.all([
      api('packages'),
      api('config').catch(() => null),
    ]);
    lastPackages = d;                            // 缓存供首页到期卡片与排序切换复用
    lastPackagesAt = Date.now();
    lastDetailLimit = pkDetailLimit(c && c.config);
    renderPackages(d, lastDetailLimit);
  } catch (e) {
    $('pkSummary').innerHTML = '<div class="empty">读取失败：' + esc(shortErr(e.message)) + '</div>';
    $('pkExpiry').innerHTML = '<div class="pk-expiry-empty">读取失败：' + esc(shortErr(e.message)) + '</div>';
  }
}

/* ── 积分到期提醒（首页卡片）────────────────────────────────────────── */
/* 积分不是永久的：签到/任务发的裂变包约一个月失效。只看「剩余积分 ÷ 日消耗」
   会系统性偏乐观——用不完的部分到期直接蒸发。这里把「最近要过期的是哪批、
   有多少、到期前每天要至少消耗多少」顶到首页，数据源与「积分构成」共用
   （lastPackages 缓存，EXP_FRESH_MS 内复用，不重复打上游）。 */

// expBatches 把某账号的包聚合成「到期日 → 该日作废积分」升序列表。
// 只统计 remain>0 且有到期时间的包——没余额/长期包到期没有任何影响。
function expBatches(packs) {
  const byDay = new Map();
  for (const p of packs || []) {
    const r = Number(p.remain || 0);
    const t = (p.end_time || '').slice(0, 10);
    if (r <= 0 || !t) continue;
    byDay.set(t, (byDay.get(t) || 0) + r);
  }
  return [...byDay.entries()]
    .map(([date, remain]) => ({ date, remain }))
    .sort((a, b) => (a.date < b.date ? -1 : 1));
}

function expDaysLeft(dateStr, today) {
  return Math.round((new Date(dateStr + 'T00:00:00') - today) / 86400000);
}

// expiryRowsHtml 到期提醒的逐账号行渲染。**「检查」与首页快照共用这一个函数**，
// 保证两条数据路径（逐包明细 / state 里的按日批次）输出完全一致：排序、今天/明天
// 文案、日均需耗、7 天内合计、随后批次都只有一份实现。
// list 每项形态：{ uid, nickname, packages:[{end_time, remain}], error? }。
function expiryRowsHtml(list) {
  const today = new Date(); today.setHours(0, 0, 0, 0);
  const nameCounts = typeof acctNameCounts === 'function' ? acctNameCounts(list) : new Map();
  // 按「最近到期」升序排（issue #125）。
  //
  // 后端 /panel/api/packages 是按**余额降序**返回的，恰好把 CN 账号都排在前面、
  // global 排在末尾，看起来像"按域分组"，其实只是余额顺序。这里再排一次，让
  // "最快过期的排最前"这个唯一重要的顺序成立，且不分域。
  //
  // 排序键用最早到期批次的日期（expBatches 已按日期升序，故 [0] 即最早）。
  // 查询失败的账号、以及 7 天内无到期的账号没有可比较的到期时间，统一排在最后，
  // 保持它们原本的相对顺序（稳定排序）。
  const keyed = list.map(a => {
    let key = null;
    if (!a.error) {
      const bs = expBatches(a.packages).filter(b => expDaysLeft(b.date, today) >= 0);
      if (bs.length) key = bs[0].date;
    }
    return { a, key };
  });
  keyed.sort((x, y) => {
    if (x.key !== y.key) {
      if (x.key === null) return 1;
      if (y.key === null) return -1;
      return x.key < y.key ? -1 : 1;
    }
    if (x.key === null) return 0;
    // 同日到期的账号按昵称/uid 兜底排序：两条数据路径（检查的余额降序、
    // overview 的 uid 升序）输入顺序不同，加固定 tie-break 才能保证输出一致。
    const ax = String(x.a.nickname || x.a.uid || '');
    const ay = String(y.a.nickname || y.a.uid || '');
    return ax < ay ? -1 : ax > ay ? 1 : 0;
  });
  return keyed.map(({ a }) => {
    const name = typeof acctDisplayName === 'function'
      ? acctDisplayName(a, nameCounts)
      : (a.nickname || String(a.uid || '').slice(0, 8));
    if (a.error) {
      return '<div class="exp-row"><span class="exp-dot mute"></span>' +
        '<span class="exp-nm">' + esc(name) + '</span>' +
        '<span class="exp-main err">查询失败：' + esc(shortErr(a.error)) + '</span></div>';
    }
    const bs = expBatches(a.packages).filter(b => expDaysLeft(b.date, today) >= 0);
    if (!bs.length) {
      return '<div class="exp-row"><span class="exp-dot ok"></span>' +
        '<span class="exp-nm">' + esc(name) + '</span>' +
        '<span class="exp-main">7 天内无到期积分</span></div>';
    }
    const first = bs[0];
    const days = expDaysLeft(first.date, today);
    const daily = Math.ceil(first.remain / Math.max(1, days));
    const week = bs.filter(b => expDaysLeft(b.date, today) <= 7)
      .reduce((s, b) => s + b.remain, 0);
    // 危险度：≤3 天红（不抓紧就真没了）、≤7 天琥珀、更远绿。
    // 上游扣包是 FEFO（按失效时刻升序，实测两号口径一致）：这些快过期批次正是
    // 被消耗得最快的，日均需耗给的是「哪怕单靠这个账号的自然流量也能对齐」的参照。
    const cls = days <= 3 ? 'bad' : days <= 7 ? 'warn' : 'ok';
    const dayWord = days === 0 ? '今天到期' : days === 1 ? '明天到期' : days + ' 天后到期';
    const more = bs.length > 4 ? '　等 ' + bs.length + ' 批' : '';
    const rest = bs.slice(1, 4).map(b =>
      '随后 ' + esc(b.date.slice(5)) + ' · ' + fmtTok(b.remain)).join('　') + more;
    return '<div class="exp-row"><span class="exp-dot ' + cls + '"></span>' +
      '<span class="exp-nm" title="' + esc(name) + '">' + esc(name) + '</span>' +
      '<span class="exp-main">最近到期 <b>' + esc(first.date) + '</b>（' + dayWord +
      '）· 该批 <b>' + fmtTok(first.remain) + '</b> 积分 · 到期前日均需耗 ≥<b>' +
      fmtTok(daily) + '</b> 积分/天' +
      (week > first.remain ? ' · 7 天内合计 ' + fmtTok(week) : '') +
      (rest ? '<div class="note">' + rest + '</div>' : '') +
      '</span></div>';
  }).join('');
}

function renderExpiry(d) {
  const list = (d.accounts || []);
  if (!list.length) { $('expBox').hidden = true; return; }
  const rows = expiryRowsHtml(list);
  $('expList').innerHTML = rows || '<div class="empty">没有账号</div>';
  // 数据新鲜度透明化：走缓存时标注年龄，免得把旧数据误当实时。
  const ageMin = lastPackages ? Math.floor((Date.now() - lastPackagesAt) / 60000) : 0;
  $('expNote').textContent = (lastPackagesAt && ageMin > 0)
    ? list.length + ' 个账号 · ' + ageMin + ' 分钟前的数据'
    : list.length + ' 个账号 · 实时查询上游';
  $('expNote').title = '逐个账号实时查询上游的积分包明细；点「刷新明细」刷新。';
  const expNoteEl = $('expNote');
  if (expNoteEl && expNoteEl.dataset) expNoteEl.dataset.full = expNoteEl.textContent;
  $('expBox').hidden = false;
  if (typeof applyExpFold === 'function') applyExpFold();
}

/* 快照版到期提醒：不查上游，直接用 /overview 里的 credits_expiry_buckets。
   后端每次余额刷新（默认每 5 分钟）按自然日聚合并写入 state.json，口径与
   「检查」的逐包按日汇总完全相同；这里把批次还原成 packages 形态后直接复用
   expiryRowsHtml，因此排序/今天明天/日均需耗/7 天内合计/随后批次全部一致。

   旧 state（升级后尚未跑过一轮余额刷新，或 Redis 快照来自旧版本）没有 buckets，
   退回只显示最早一批，保证卡片不空白；下一轮刷新后自动变成完整视图。 */
function renderExpirySnapshot(d) {
  const list = (d && d.accounts) || [];
  // 空账号池不该出现一张「积分到期提醒 / 没有账号」的卡；账号池空态已经说清楚了。
  if (!list.length) { $('expBox').hidden = true; return; }
  const hasBuckets = list.some(a => Array.isArray(a.credits_expiry_buckets) && a.credits_expiry_buckets.length);
  if (hasBuckets) {
    const isoDay = v => {
      const dt = new Date(v);
      if (isNaN(dt.getTime())) return '';
      const p2 = n => String(n).padStart(2, '0');
      return dt.getFullYear() + '-' + p2(dt.getMonth() + 1) + '-' + p2(dt.getDate());
    };
    // 逐账号取批次；升级过渡期/单号刷新失败的账号可能还没有 buckets，用旧的
    // 「最早一批」字段兜底，避免在同一次渲染里被误报成「无到期」。
    const packagesOf = a => {
      const bs = a.credits_expiry_buckets || [];
      if (bs.length) {
        return bs.map(b => ({ end_time: (b.date || '') + 'T00:00:00', remain: b.remain }));
      }
      const day = isoDay(a.credits_earliest_expiry);
      return (day && a.credits_earliest_remaining)
        ? [{ end_time: day + 'T00:00:00', remain: a.credits_earliest_remaining }]
        : [];
    };
    renderExpiry({
      accounts: list.map(a => ({
        uid: a.uid,
        nickname: a.nickname,
        realm: a.realm,
        packages: packagesOf(a),
      })),
    });
    $('expNote').textContent = snapshotNoteText(list.length, d);
    const expNoteEl = $('expNote');
    if (expNoteEl && expNoteEl.dataset) expNoteEl.dataset.full = expNoteEl.textContent;
    if (typeof applyExpFold === 'function') applyExpFold();
    return;
  }
  // 旧 state 兜底：只有最早一批的日期与数量。
  const today = new Date(); today.setHours(0, 0, 0, 0);
  const keyed = list.map(a => {
    // 后端给的是 RFC3339 带时区；无到期信息（长期包/未刷新）的账号排在最后。
    let key = null, ms = 0;
    if (a.credits_earliest_expiry) {
      const t = new Date(a.credits_earliest_expiry);
      if (!isNaN(t.getTime())) { ms = t.getTime(); key = ms; }
    }
    return { a, key, ms };
  });
  keyed.sort((x, y) => {
    if (x.key === null && y.key === null) return 0;
    if (x.key === null) return 1;
    if (y.key === null) return -1;
    return x.key - y.key;
  });
  const rows = keyed.map(({ a, ms }) => {
    const nm = esc(a.nickname || String(a.uid || '').slice(0, 8));
    if (!ms || !a.credits_earliest_remaining) {
      return '<div class="exp-row"><span class="exp-dot ok"></span>' +
        '<span class="exp-nm" title="' + nm + '">' + nm + '</span>' +
        '<span class="exp-main">无近期到期批次</span></div>';
    }
    // 与 expDaysLeft 同口径：按本地自然日算，而不是对精确时间戳 ceil（后者会把
    // 明天/今天各多算一天，日均需耗也随之少一半）。
    const dt = new Date(ms);
    const p2 = n => String(n).padStart(2, '0');
    const when = dt.getFullYear() + '-' + p2(dt.getMonth() + 1) + '-' + p2(dt.getDate());
    const days = Math.max(0, Math.round((new Date(when + 'T00:00:00') - today) / 86400000));
    const remain = Number(a.credits_earliest_remaining || 0);
    const daily = Math.ceil(remain / Math.max(1, days));
    const cls = days <= 3 ? 'bad' : days <= 7 ? 'warn' : 'ok';
    const dayWord = days === 0 ? '今天到期' : days === 1 ? '明天到期' : days + ' 天后到期';
    return '<div class="exp-row"><span class="exp-dot ' + cls + '"></span>' +
      '<span class="exp-nm" title="' + nm + '">' + nm + '</span>' +
      '<span class="exp-main">最近到期 <b>' + esc(when) + '</b>（' + dayWord +
      '）· 该批 <b>' + fmtTok(remain) + '</b> 积分 · 到期前日均需耗 ≥<b>' +
      fmtTok(daily) + '</b> 积分/天' +
      '<div class="note">仅最早一批，点「刷新明细」看全部到期批次</div>' +
      '</span></div>';
  }).join('');
  $('expList').innerHTML = rows || '<div class="empty">没有账号</div>';
  $('expNote').textContent = snapshotNoteText(list.length, d);
  const expNoteEl = $('expNote');
  if (expNoteEl && expNoteEl.dataset) expNoteEl.dataset.full = expNoteEl.textContent;
  $('expNote').title = '快照来自本地状态，随后台余额刷新更新；点「刷新明细」拉取逐包实时明细。';
  $('expBox').hidden = false;
  if (typeof applyExpFold === 'function') applyExpFold();
}

// snapshotNoteText 快照卡片的数据新鲜度标注。界面本身 5 秒轮询一次，但卡片数据
// 只随余额后台刷新更新（默认每 5 分钟）——不标出来的话，5 秒级的界面刷新会让人
// 误以为数据也是 5 秒级新鲜。放在 renderExpirySnapshot 之后：前端 harness 按
// 「renderExpirySnapshot 起」切片求值，函数声明提升让它在同一切片内可用。
function snapshotNoteText(count, d) {
  const iv = Number(d && d.balance_refresh_sec) || 0;
  const at = d && d.balance_refreshed_at ? new Date(d.balance_refreshed_at).getTime() : 0;
  if (iv <= 0) {
    return count + ' 个账号 · 本地快照（未开启后台刷新）';
  }
  const every = iv % 60 === 0 ? iv / 60 + ' 分钟' : iv + ' 秒';
  let tail = '每 ' + every + '后台刷新';
  if (at && !isNaN(at)) {
    const min = Math.floor((Date.now() - at) / 60000);
    tail += '，' + (min < 1 ? '刚刚更新' : min + ' 分钟前更新');
  }
  return count + ' 个账号 · ' + tail;
}

async function loadExpiry(force) {
  if (!$('expBox')) return;
  if (expFetching) return;
  const fresh = lastPackages && (Date.now() - lastPackagesAt) < EXP_FRESH_MS;
  if (fresh && !force) { renderExpiry(lastPackages); return; }
  expFetching = true;
  $('expBox').hidden = false;
  if (!$('expList').children.length) $('expList').innerHTML = '<div class="empty">查询中…（逐账号向上游实时查询）</div>';
  $('expNote').textContent = '查询中…';
  try {
    const d = await api('packages');
    lastPackages = d;                            // 与「积分构成」视图共用同一份缓存
    lastPackagesAt = Date.now();
    renderExpiry(d);
  } catch (e) {
    // textContent 赋值不能再 esc：那样会把 &lt;html&gt; 原样显示出来（双重转义）。
    $('expNote').textContent = '查询失败：' + shortErr(e.message);
  }
  expFetching = false;
}

if ($('btnExp')) $('btnExp').onclick = () => loadExpiry(true);

// 「刷新」始终实时查（显式 force，不依赖事件对象真值）
if ($('btnPk')) $('btnPk').onclick = () => { loadPackages(true); loadCreditTrend(); };

/* ══ 账号备注（只写本机 localStorage；账号池 / 用量两个视图共用同一份映射）══════
   键取 uid 前 8 位——与 renderUsageDim 账号维度的 key 和积分扣除表的 uid 前缀同源，
   三个表格因此都能直接 join。展示层只替换文本并挂 .rm-ghost 虚体，账号标识本身
   仍由服务端下发，不写回后端；备注是纯展示层信息，不参与选号 / 计费 / 鉴权。 */
function remarksAll() {
  if (remarksCache) return remarksCache;
  try { remarksCache = JSON.parse(localStorage.getItem(LS_REMARKS)) || {}; }
  catch (e) { remarksCache = {}; }
  return remarksCache;
}
function remarkKey(uid) { return String(uid == null ? '' : uid).slice(0, 8); }
function remarkOf(uid) {
  const k = remarkKey(uid);
  return k ? String(remarksAll()[k] || '') : '';
}
/* nameHtml 必须是已转义的 HTML；有备注时返回「备注 + 原标识（浅色虚体）」 */
function remarkName(uid, nameHtml) {
  const r = remarkOf(uid);
  return r ? esc(r) + '<span class="rm-ghost">' + nameHtml + '</span>' : nameHtml;
}
function storeRemarks(map) {
  remarksCache = map;
  try { localStorage.setItem(LS_REMARKS, JSON.stringify(map)); } catch (e) {}
}

/* 编辑器的行来自 overview 缓存（账号池加载过就有），另外补上「已有备注但当前列表里
   没有」的键，避免账号被移除后备注看起来凭空消失。 */
function remarkFormRows() {
  const rows = ((overviewData && overviewData.accounts) || [])
    .map(s => ({ uid: remarkKey(s.uid), who: s.nickname || remarkKey(s.uid) }))
    .filter(r => r.uid);
  for (const k of Object.keys(remarksAll())) {
    if (!rows.some(r => r.uid === k)) rows.push({ uid: k, who: k });
  }
  return rows;
}
function renderRemarkBody() {
  const box = $('remarkBody');
  if (!box) return;
  const rows = remarkFormRows();
  box.innerHTML = rows.length
    ? rows.map(r => '<div class="rm-row"><span class="rm-who" title="' + esc(r.who) + '">' + esc(r.who) + '</span>' +
        '<input type="text" data-uid="' + esc(r.uid) + '" value="' + esc(remarksAll()[r.uid] || '') + '" placeholder="备注（留空即清除）" maxlength="24"></div>').join('')
    : '<div class="empty">暂无账号：先在「账号池」加载一次账号列表。</div>';
}
function openRemarks() {
  renderRemarkBody();
  if ($('remarkVeil')) openVeil('remarkVeil');
}
function closeRemarks() { if ($('remarkVeil')) closeVeil('remarkVeil'); }
function saveRemarkForm() {
  const map = {};
  document.querySelectorAll('#remarkBody input[data-uid]').forEach(inp => {
    const v = inp.value.trim();
    if (v) map[inp.getAttribute('data-uid')] = v;
  });
  storeRemarks(map);
  closeRemarks();
  // 备注只影响本机展示：重渲染当前视图即可，无需后端参与。
  if (view === 'accounts') loadOverview(true);
  else if (view === 'usage' && usageData) renderUsage(usageData);
  toast('备注已保存（仅本机浏览器）');
}
if ($('btnRemarks')) $('btnRemarks').onclick = openRemarks;
if ($('btnRemarkClose')) $('btnRemarkClose').onclick = closeRemarks;
if ($('btnRemarkClear')) $('btnRemarkClear').onclick = async () => {
  const n = Object.keys(remarksAll()).length;
  if (!n) { toast('没有可清空的备注'); return; }
  if (!(await askConfirm({
    title: '清空全部备注', confirmText: '清空', danger: true,
    body: '将清空本机 ' + n + ' 条备注（仅影响当前浏览器），不可恢复。'
  }))) return;
  storeRemarks({}); renderRemarkBody(); toast('已清空本机备注');
};
if ($('btnRemarkSave')) $('btnRemarkSave').onclick = saveRemarkForm;

/* ══ 首页「积分到期提醒」折叠（选择记在本机；账号多时默认折叠）════════════
   issue #137 的教训是「入口藏在自己的隐藏卡片里」——所以卡片头、摘要、展开按钮、
   刷新明细始终可见。默认展开只适合少量账号；19 个账号时它会把账号池推到第二屏，
   因此没有显式选择且账号数 >8 时自动折叠，摘要仍给出最近到期与 7 天内合计。 */
const EXP_AUTO_FOLD_OVER = 8;
function expFolded() {
  // 注意：前端 harness 会从本函数起单独切片求值，不能依赖切片外的 helper；
  // 这里把「本地偏好 + 账号数」都收进函数体内，并为无 DOM/无 overview 的测试桩兜底。
  let pref = '';
  try {
    const v = localStorage.getItem('wb2api.expfold.v1');
    pref = v === '1' ? 'fold' : (v === '0' ? 'open' : '');
  } catch (e) { /* localStorage 不可用时走自动折叠 */ }
  if (pref) return pref === 'fold';
  let count = 0;
  if (typeof overviewData !== 'undefined' && overviewData && Array.isArray(overviewData.accounts)) {
    count = overviewData.accounts.length;
  } else {
    const list = $('expList');
    count = list && list.querySelectorAll ? list.querySelectorAll('.exp-row').length : 0;
  }
  const limit = typeof EXP_AUTO_FOLD_OVER === 'number' ? EXP_AUTO_FOLD_OVER : 8;
  return count > limit;
}
function expSummaryText() {
  const list = (overviewData && overviewData.accounts) || [];
  if (!list.length) return '暂无账号';
  const today = new Date(); today.setHours(0, 0, 0, 0);
  let earliest = null, week = 0;
  const urgentUids = new Set();
  const consider = (dateStr, remain, uid) => {
    if (!dateStr || !(Number(remain) > 0)) return;
    const dt = new Date(dateStr.length === 10 ? dateStr + 'T00:00:00' : dateStr);
    if (isNaN(dt.getTime())) return;
    const days = Math.max(0, Math.round((dt.getTime() - today.getTime()) / 86400000));
    if (days <= 7) week += Number(remain);
    if (days <= 3) urgentUids.add(String(uid || ''));
    if (!earliest || dt < earliest.dt) earliest = { dt, days, remain: Number(remain) };
  };
  for (const a of list) {
    const buckets = Array.isArray(a.credits_expiry_buckets) ? a.credits_expiry_buckets : [];
    if (buckets.length) buckets.forEach(b => consider(b.date, b.remain, a.uid));
    else consider(a.credits_earliest_expiry, a.credits_earliest_remaining, a.uid);
  }
  const urgent = urgentUids.size;
  if (!earliest) return list.length + ' 个账号 · 暂无近期到期';
  const p2 = n => String(n).padStart(2, '0');
  const when = earliest.dt.getFullYear() + '-' + p2(earliest.dt.getMonth() + 1) + '-' + p2(earliest.dt.getDate());
  const dayWord = earliest.days === 0 ? '今天' : earliest.days + ' 天后';
  return '最近到期 ' + when + '（' + dayWord + '）' +
    (week > 0 ? ' · 7 天内合计 ' + fmtTok(week) + ' 积分' : '') +
    ' · ' + list.length + ' 个账号' + (urgent ? ' · ' + urgent + ' 个 3 天内到期' : '');
}
function applyExpFold() {
  const pad = $('expPad'), btn = $('btnExpFold'), note = $('expNote');
  if (!pad || !btn) return;
  const folded = expFolded();
  // 折叠时把摘要放进卡片头的 note；展开时恢复渲染函数写入的新鲜度原文。
  if (note && note.dataset) {
    if (folded) {
      if (!note.dataset.full) note.dataset.full = note.textContent || '';
      note.textContent = expSummaryText();
    } else if (note.dataset.full) {
      note.textContent = note.dataset.full;
      delete note.dataset.full;
    }
  }
  pad.hidden = folded;
  btn.textContent = folded ? '展开' : '收起明细';
  btn.setAttribute('aria-expanded', folded ? 'false' : 'true');
}
if ($('btnExpFold')) {
  $('btnExpFold').onclick = () => {
    try { localStorage.setItem(LS_EXP_FOLD, expFolded() ? '0' : '1'); } catch (e) {}
    applyExpFold();
  };
}
applyExpFold();

/* ══ 通道健康（反代劣化）看板 ═══════════════════════════════════════════
   看的是「上游怎么对待我们」+「我们为了让请求过通道改了多少东西」。

   两条曲线分工：
     - 拦截事件：channel_refused / 限流 / 流中断 / 其他，按请求计（可切每千请求）；
     - 出站改写：role_normalized / sanitize，按消息条数计，独立 y 轴。
   11-128 被 system→user 搬移救回后终态是 200，用终态成功率永远看不出劣化；
   这里用网关决策点上的事件计数（channel_health 的 events 段）。

   口径与容错：
     - x 轴用窗口边界定域（不是数据首末点），停机断档不会被画成连续曲线；
     - 窗口起点到 events.since 之间画覆盖带，明确「这段时间没有事件数据」；
     - 桶时间按服务端时区（API 的 tz）解析，标签按浏览器本地时区显示；
     - 每桶一个透明命中区，tooltip 汇总全部系列 + 请求/失败 + 改写量。 */

let channelData = null;
let chMeasure = 'intercept';       // intercept | rewrite
let chMode = 'abs';                // abs | per1k（仅拦截视图）
let chSortKey = '';                // '' = 注册表顺序
let chSortDir = 1;
let chAutoSec = 60;
let chTimer = null;

try {
  chMeasure = localStorage.getItem('wb2api.ch.measure') || 'intercept';
  chMode = localStorage.getItem('wb2api.ch.mode') || 'abs';
  chAutoSec = Number(localStorage.getItem('wb2api.ch.auto') || 60);
} catch (e) { /* 隐私模式：用默认值 */ }
if (chMeasure !== 'intercept' && chMeasure !== 'rewrite') chMeasure = 'intercept';
if (chMode !== 'abs' && chMode !== 'per1k') chMode = 'abs';
if ([0, 30, 60, 300].indexOf(chAutoSec) < 0) chAutoSec = 60;

/* CH_GROUP_COLOR 进图组配色（颜色是展示层，留在前端；后端只下发 key/label）。
   v5：原先「流中断·超时」用 --chart-2（青绿），跟全站 --ok「正常/成功」撞语义——
   同一页里绿色一会儿代表流中断一会儿代表成功。现在故障类统一走红/琥珀/暖橙，
   绿色只留给"正常"；限流沿用 --chart-4（紫），它是中性色，不跟任何好坏语义冲突。 */
const CH_GROUP_COLOR = {
  refusal: 'var(--bad)',        // 通道拒绝：红
  throttle: 'var(--chart-4)',   // 限流 / 容量：紫（中性）
  stream: 'var(--chart-10)',    // 流中断·超时：暖橙（原来是绿色）
  other: 'var(--ink-3)',        // 其他：中性灰
};
function chGroupColor(key) { return CH_GROUP_COLOR[key] || 'var(--ink-3)'; }

/* chGroupCount 取某个桶里某组的数量；groups 由后端算好（other 已倒推并 clamp）。 */
function chGroupCount(pt, key) {
  return Number(((pt && pt.groups) || {})[key] || 0);
}

/* chChartTotal 桶内「进图事件数」合计（不含出站改写）。 */
function chChartTotal(pt) {
  const g = (pt && pt.groups) || {};
  let n = 0;
  for (const k in g) n += Number(g[k] || 0);
  return n;
}

function chNum(v) {
  const n = Number(v || 0);
  return n > 0 ? fmtTok(n) : '0';
}

function chPct(part, total) {
  const t = Number(total || 0);
  if (!t) return '—';
  return (Number(part || 0) / t * 100).toFixed(1) + '%';
}

/* chNarrow 窄屏判定：440 宽 viewBox + 更少刻度（与手机/平板断点同口径）。 */
function chNarrow() {
  return typeof matchMedia === 'function' && matchMedia('(max-width: 1000px)').matches;
}

/* chTZOffsetMin 把 "+08:00"/"-05:00" 解析成分钟；非法返回 null。 */
function chTZOffsetMin(tz) {
  const m = /^([+-])(\d{2}):(\d{2})$/.exec(String(tz || ''));
  if (!m) return null;
  return (m[1] === '-' ? -1 : 1) * (Number(m[2]) * 60 + Number(m[3]));
}

function chLocalOffsetMin() { return -new Date().getTimezoneOffset(); }

/* chPointTime 把桶键按**服务端时区**解析成绝对毫秒。
   桶键是服务端本地时间（不带偏移），直接 new Date 会按浏览器时区解释；
   生产宿主是 UTC、浏览器是 UTC+8 时会整体差 8 小时。tz 缺失/非法时回落旧行为。 */
function chPointTime(p, tz) {
  const off = String(tz || '');
  const s = p.t.length === 13 ? p.t + ':00:00' + off : p.t + 'T00:00:00' + off;
  const ms = Date.parse(s);
  if (Number.isFinite(ms)) return ms;
  return parsePointTime(p);
}

/* chBucketTitle 桶的完整时间（日桶按服务端日历日标注）。 */
function chBucketTitle(p, tz) {
  const server = chTZOffsetMin(tz);
  if (p.scope === 'day') {
    return p.t + (server !== null && server !== chLocalOffsetMin() ? '（服务端日）' : '');
  }
  const d = new Date(chPointTime(p, tz));
  return fmtLocalDateTime(d.getTime());
}

/* chAxisLabel x 轴刻度：日桶按**服务端日历日**（MM-DD），小时桶按浏览器本地时区
   （MM-DD HH:00）。日桶是一个服务端日历日，用浏览器时区取日期会在 UTC-5 之类
   的时区把「11-21 日桶」标成 11-20，和 tooltip/柱子的语义对不上。 */
function chAxisLabel(ms, bucket, tz) {
  const d = new Date(ms);
  const p = n => String(n).padStart(2, '0');
  if (bucket === 'day') {
    const off = chTZOffsetMin(tz);
    if (off !== null) {
      const sd = new Date(ms + off * 60000);
      return p(sd.getUTCMonth() + 1) + '-' + p(sd.getUTCDate());
    }
    return p(d.getMonth() + 1) + '-' + p(d.getDate());
  }
  return p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' + p(d.getHours()) + ':00';
}

/* chNiceMax 把最大值抬到 1/2/5×10^n 的「好看刻度」，避免 6→0,2,3,5,6 这种重复刻度。
   4 格网格：小整数向上取到 4 的倍数，保证每格都是整数（0/2/4/6/8 而不是 0/3/5/8）。 */
function chNiceMax(max) {
  const m = Math.max(1, Math.ceil(Number(max) || 1));
  if (m <= 40) return Math.ceil(m / 4) * 4;
  if (m <= 100) return Math.ceil(m / 20) * 20;
  const pow = Math.pow(10, Math.floor(Math.log10(m)));
  for (const k of [1, 2, 5, 10]) {
    if (m <= k * pow) return k * pow;
  }
  return 10 * pow;
}

/* chNiceMax1k 每千请求的 y 上限：值可能 <1（低拦截率），按 1/2/5×10^n 取小数刻度。 */
function chNiceMax1k(max) {
  const m = Math.max(0.1, Number(max) || 0);
  const pow = Math.pow(10, Math.floor(Math.log10(m)));
  for (const k of [1, 2, 5, 10]) {
    if (m <= k * pow) return k * pow;
  }
  return 10 * pow;
}

/* chNicePct 失败率 y 轴上限：2/5/10/20/25/30/40/50/100，超出按 100 的倍数抬。 */
function chNicePct(max) {
  const m = Math.max(0.1, Number(max) || 0);
  for (const k of [2, 5, 10, 20, 25, 30, 40, 50, 100]) {
    if (m <= k) return k;
  }
  return Math.ceil(m / 100) * 100;
}

/* chTickText 趋势图 y 轴刻度文案：拦截视图按次数/千分比，改写视图按条数。 */
function chTickText(v, per1k) {
  if (per1k) {
    if (v >= 10) return String(Math.round(v));
    if (v >= 1) return v.toFixed(1);
    return v.toFixed(2);
  }
  return fmtTok(Math.round(v));
}

/* chSegments 把折线按断档切段：相邻点间隔 > 1.8 个桶就断开，不连成假曲线。 */
function chSegments(pts, valueOf, bucket) {
  const step = bucket === 'day' ? 24 * 3600 * 1000 : 3600 * 1000;
  const segs = [];
  let cur = [];
  for (const pt of pts) {
    const v = valueOf(pt);
    if (v === null || v === undefined) {
      if (cur.length) segs.push(cur);
      cur = [];
      continue;
    }
    if (cur.length && pt.t - cur[cur.length - 1].t > step * 1.8) {
      segs.push(cur);
      cur = [];
    }
    cur.push({ t: pt.t, v: v });
  }
  if (cur.length) segs.push(cur);
  return segs;
}

/* chReqMap 把 usage 的请求序列按桶键索引，供每千请求/失败率 join。 */
function chReqMap(d) {
  const m = new Map();
  for (const p of (d && d.requests) || []) m.set(p.t, p);
  return m;
}

/* chWinLabel 窗口粒度标签。 */
function chWinLabel(d) {
  return (d && d.bucket === 'day') ? '按日' : '按小时';
}

/* chCoverText 覆盖条：窗口 + 三源起点 + 服务端分桶时区提示。 */
function chCoverText(d) {
  const s = (d && d.sources) || {};
  const parts = [];
  const wf = parseAPITime(d && d.window_from), wt = parseAPITime(d && d.window_to);
  if (wf && wt) parts.push('窗口 ' + fmtLocalDateTime(wf) + ' → ' + fmtLocalDateTime(wt) + ' · ' + chWinLabel(d));
  const ev = s.events || {};
  parts.push('事件 ' + (ev.since ? '自 ' + fmtStampShort(parseAPITime(ev.since)) : '无数据'));
  const us = s.usage || {};
  parts.push('用量 ' + (us.since ? '自 ' + fmtStampShort(parseAPITime(us.since)) : '未启用'));
  const ar = s.archive || {};
  parts.push('归档 ' + (ar.since ? '自 ' + fmtStampShort(parseAPITime(ar.since)) : '未启用'));
  let out = parts.map(esc).join('<span class="ch-sep">·</span>');
  const server = chTZOffsetMin(d && d.tz);
  if (server !== null && server !== chLocalOffsetMin()) {
    const sign = server < 0 ? '-' : '+';
    const abs = Math.abs(server);
    out += '<span class="ch-sep">·</span>服务端分桶 UTC' + sign + String(Math.floor(abs / 60)).padStart(2, '0') + ':' + String(abs % 60).padStart(2, '0');
  }
  return out;
}

/* chStateChips 状态条：一个语义徽标 + 4 个带解释的事实。
   st 为 null = handler 未接线，显示「状态不可用」而不是编造「从未触发」。 */
function chStateChips(st, fallbacks) {
  if (!st) {
    return '<span class="ch-badge mute">状态不可用</span>' +
      '<span class="ch-fact">看板状态源未接线</span>';
  }
  const MODE = { custom: '替换（custom）', append: '追加（append）', passthrough: '透传（passthrough）' };
  let out = '';
  if (st.active) {
    const until = parseAPITime(st.until);
    out += '<span class="ch-badge warn">提示词降级中' + (until ? ' · 至 ' + esc(fmtStampShort(until)) : '') + '</span>';
  } else {
    out += '<span class="ch-badge ok">通道正常</span>';
  }
  out += '<span class="ch-fact">提示词 <b>' + esc(MODE[st.mode] || st.mode || '—') + '</b></span>';
  const last = parseAPITime(st.degrade_last_at);
  out += '<span class="ch-fact">上次降级 <b>' +
    (last ? esc(fmtStampShort(last)) + ' · ' + esc(ago(st.degrade_last_at)) : '从未') + '</b></span>';
  const learnedNow = Number(st.learned_now || 0);
  const learnedTotal = Number(st.learned_total || 0);
  out += '<span class="ch-fact" title="本进程生效条数（重启清零）；历史累计 ' + learnedTotal +
    ' 次">已学习指纹 <b>' + learnedNow + ' 条</b>' +
    (learnedTotal > learnedNow ? ' · 历史 ' + learnedTotal : '') + '</span>';
  out += '<span class="ch-fact">跨域回退 <b>' + Number(fallbacks || 0) + ' 次</b></span>';
  return out;
}

/* chThrottleBreakdown 限流·容量卡片的细分文案（只列非零项）。 */
function chThrottleBreakdown(kinds) {
  const names = {
    soft_rate: '软限流', hard_credit: '余额', model_busy: '模型忙',
    upstream_busy: '渠道满载', waf_block: 'WAF', model_blocked: '模型不可用',
  };
  const parts = [];
  for (const k in names) {
    const n = Number((kinds || {})[k] || 0);
    if (n > 0) parts.push(names[k] + ' ' + fmtTok(n));
  }
  return parts.length ? parts.join(' · ') : '本窗口无';
}

/* renderChannelKpis 6 张 KPI 卡：单位/口径/数据源覆盖都写在副标题里。 */
function renderChannelKpis(d) {
  const t = d.totals || {};
  const kinds = t.kinds || {};
  const groups = t.groups || {};
  const reqs = Number(t.requests || 0);
  const errs = Number(t.errors || 0);
  const refused = Number(kinds.channel_refused || 0);
  const moved = Number(kinds.sys_as_user || 0);
  const rewrites = Number(kinds.role_normalized || 0) + Number(kinds.sanitize || 0);
  const throttled = Number(groups.throttle || 0);
  const per1k = reqs > 0 ? refused / reqs * 1000 : 0;
  const rate = Number(t.failure_rate || 0);
  const rateCls = !reqs ? 'c-mute' : rate >= 10 ? 'c-bad' : rate >= 2 ? 'c-warn' : 'c-ok';
  const ev = (d.sources || {}).events || {};
  const evCover = ev.since ? '事件自 ' + fmtStampShort(parseAPITime(ev.since)) + ' 起' : '事件无数据';
  let out = '';
  out += usKpi(chNum(reqs), '请求（尝试）', 'c-accent', errs + ' 次失败 · 用量账本 · ' + chWinLabel(d));
  out += usKpi(rate.toFixed(1) + '%', '失败率', rateCls, '终态失败 / 全部尝试');
  out += usKpi(chNum(refused), '通道拦截（11-128）', refused > 0 ? 'c-warn' : 'c-ok',
    '每千请求 ' + per1k.toFixed(2) + ' · ' + evCover);
  out += usKpi(chNum(moved), 'system→user 搬移', moved > 0 ? 'c-warn' : 'c-ok', '每次拦截都会尝试一次');
  out += usKpi(chNum(rewrites), '出站改写', rewrites > 0 ? 'c-accent' : 'c-mute', '按消息条数计，不是请求');
  out += usKpi(chNum(throttled), '限流·容量', throttled > 0 ? 'c-warn' : 'c-ok', chThrottleBreakdown(kinds));
  $('dgStats').innerHTML = out;
}

/* chTrendSeries 当前度量的系列定义。 */
function chTrendSeries(d) {
  if (chMeasure === 'rewrite') {
    return [
      { key: 'role_normalized', label: '归一（developer→system）', color: 'var(--chart-2)' },
      { key: 'sanitize', label: '指纹清洗', color: 'var(--chart-3)' },
    ];
  }
  return ((d && d.groups) || []).map(g => ({ key: g.key, label: g.label, color: chGroupColor(g.key) }));
}

/* chTrendValue 某个桶某个系列的值；每千请求模式下无请求的桶返回 null（留空）。 */
function chTrendValue(pt, s, reqs) {
  if (chMeasure === 'rewrite') {
    return Number(((pt.pt.kinds || {})[s.key]) || 0);
  }
  const v = chGroupCount(pt.pt, s.key);
  if (chMode === 'per1k') {
    const r = reqs.get(pt.raw);
    const n = r ? Number(r.req || 0) : 0;
    // 分母太小时 1 个事件就能放大成 500‰（2 请求 1 拦截），失去可比性：
    // 桶内请求数 <10 不画，宁可留空也不画假峰值。
    if (n < 10) return null;
    return v / n * 1000;
  }
  return v;
}

/* chTipForBucket 每桶 tooltip：时间 + 各系列 + 请求/失败 + 改写另计。 */
function chTipForBucket(pt, d, series, reqs) {
  const lines = [chBucketTitle({ t: pt.raw, scope: pt.scope }, d.tz)];
  if (chMeasure === 'rewrite') {
    for (const s of series) lines.push(s.label + ' ' + fmtTok(Number(((pt.pt.kinds || {})[s.key]) || 0)) + ' 条');
  } else {
    const parts = [];
    for (const s of series) {
      const v = chGroupCount(pt.pt, s.key);
      if (v > 0) parts.push(s.label + ' ' + v);
    }
    lines.push('拦截：' + (parts.length ? parts.join(' · ') : '0'));
    const r = reqs.get(pt.raw);
    if (chMode === 'per1k' && r && Number(r.req || 0) > 0) {
      lines.push('每千请求：' + (chChartTotal(pt.pt) / Number(r.req) * 1000).toFixed(2) + '‰');
    }
  }
  const r = reqs.get(pt.raw);
  if (r) {
    const n = Number(r.req || 0), e = Number(r.err || 0);
    lines.push('请求 ' + fmtTok(n) + ' · 失败 ' + fmtTok(e) + '（' + (n ? (e / n * 100).toFixed(1) : '0.0') + '%）');
  }
  const rw = Number((pt.pt.kinds || {}).role_normalized || 0) + Number((pt.pt.kinds || {}).sanitize || 0);
  if (chMeasure !== 'rewrite' && rw > 0) lines.push('另：出站改写 ' + fmtTok(rw) + ' 条');
  return lines.join('\n');
}

/* renderChannelTrend 主图：拦截事件（堆叠柱 / 每千请求）或出站改写（双折线）。
   x 轴用窗口边界定域；窗口起点到 events.since 之间画覆盖带。 */
function renderChannelTrend(d) {
  const host = $('dgTrendChart');
  if (!host) return;
  const tz = d.tz || '';
  const pts = [];
  for (const p of d.events || []) {
    const t = chPointTime(p, tz);
    if (t === null) continue;
    pts.push({ t: t, raw: p.t, scope: p.scope, pt: p });
  }
  const series = chTrendSeries(d);
  const reqs = chReqMap(d);
  $('dgTrendLegend').innerHTML = series.map(s =>
    '<span class="legend"><i class="sw" style="background:' + s.color + '"></i>' + esc(s.label) + '</span>').join('');
  const modeBox = $('dgMode');
  if (modeBox) modeBox.hidden = chMeasure !== 'intercept';

  if (!pts.length) {
    host.innerHTML = '<div class="us-empty">该窗口内没有事件数据。事件计数器启用后才会产生数据。</div>';
    $('dgTrendNote').textContent = '—';
    return;
  }

  // x 域：优先窗口边界；窗口缺失/非法时回落数据首末点。
  let t0 = parseAPITime(d.window_from), t1 = parseAPITime(d.window_to);
  if (!t0 || !t1 || t1 <= t0) {
    t0 = pts[0].t;
    t1 = pts[pts.length - 1].t;
  }
  if (t1 <= t0) t1 = t0 + 1;

  // y 轴上限：堆叠柱按桶内合计（每千请求也是各系列之和），折线按单系列峰值。
  let max = 0;
  for (const pt of pts) {
    if (chMeasure === 'rewrite') {
      for (const s of series) max = Math.max(max, chTrendValue(pt, s, reqs) || 0);
    } else if (chMode === 'abs') {
      max = Math.max(max, chChartTotal(pt.pt));
    } else {
      let sum = 0;
      for (const s of series) {
        const v = chTrendValue(pt, s, reqs);
        if (v !== null) sum += v;
      }
      max = Math.max(max, sum);
    }
  }
  const nice = chMeasure === 'intercept' && chMode === 'per1k' ? chNiceMax1k(max) : chNiceMax(max);

  const W = typeof chartWidth === 'function'
    ? chartWidth(host, 1200, 320, 1400)
    : (typeof chNarrow === 'function' && chNarrow() ? 440 : 1200);
  const narrow = W < 700;
  const H = narrow ? 220 : 260;
  const PL = narrow ? 46 : 56, PR = 16, PT = 16, PB = 30;
  const iw = W - PL - PR, ih = H - PT - PB, yBase = PT + ih;
  const xOf = t => PL + (t - t0) / (t1 - t0) * iw;
  const yOf = v => PT + ih - ih * (v / nice);

  let out = '<svg viewBox="0 0 ' + W + ' ' + H + '" role="img" aria-label="' +
    (chMeasure === 'rewrite' ? '出站改写趋势' : '拦截事件趋势') + '" preserveAspectRatio="xMidYMid meet">';

  // 覆盖带：窗口起点 → 事件数据起点，说明这段空白不是「一切正常」。
  const since = parseAPITime(((d.sources || {}).events || {}).since);
  if (since && since > t0 && since < t1) {
    const x = xOf(since);
    if (x > PL + 2) {
      out += '<rect class="chgap" x="' + PL.toFixed(2) + '" y="' + PT.toFixed(2) + '" width="' + (x - PL).toFixed(2) + '" height="' + ih.toFixed(2) + '"/>';
      out += '<line class="chmark" x1="' + x.toFixed(2) + '" y1="' + PT.toFixed(2) + '" x2="' + x.toFixed(2) + '" y2="' + yBase.toFixed(2) + '"/>';
      // 数据起点贴近右边缘时标签放线的左侧，否则会被 viewBox 裁掉。
      const labelLeft = x > W - PR - 70;
      out += '<text class="tk" x="' + (labelLeft ? x - 4 : x + 4).toFixed(2) + '" y="' + (PT + 12).toFixed(2) +
        '"' + (labelLeft ? ' text-anchor="end"' : '') + '>数据起点</text>';
    }
  }

  // y 网格 + 刻度（4 格）
  for (let i = 0; i <= 4; i++) {
    const y = PT + ih - ih * i / 4;
    out += '<line class="gl" x1="' + PL + '" y1="' + y.toFixed(2) + '" x2="' + (W - PR) + '" y2="' + y.toFixed(2) + '"/>';
    out += '<text class="tk" x="' + (PL - 6) + '" y="' + (y + 4).toFixed(2) + '" text-anchor="end">' +
      chTickText(nice * i / 4, chMode === 'per1k' && chMeasure === 'intercept') + '</text>';
  }

  // x 刻度：窄屏 3 个（标签是「MM-DD HH:00」，4 个就会互相压字）。
  const ticks = W < 420 ? 2 : (narrow ? 3 : 6);
  for (let k = 0; k < ticks; k++) {
    const target = t0 + (t1 - t0) * (ticks === 1 ? 0.5 : k / (ticks - 1));
    const x = Math.min(W - PR, Math.max(PL, xOf(target)));
    const anchor = k === 0 ? 'start' : (k === ticks - 1 ? 'end' : 'middle');
    out += '<text class="tk" x="' + x.toFixed(2) + '" y="' + (yBase + 16).toFixed(2) + '" text-anchor="' + anchor + '">' +
      esc(chAxisLabel(target, d.bucket, tz)) + '</text>';
  }

  if (chMeasure === 'rewrite') {
    // 双折线：断档分段（相邻点 > 1.8 个桶断开）。
    for (const s of series) {
      const segs = chSegments(pts, pt => chTrendValue(pt, s, reqs), d.bucket);
      for (const seg of segs) {
        if (seg.length === 1) {
          out += '<circle cx="' + xOf(seg[0].t).toFixed(2) + '" cy="' + yOf(seg[0].v).toFixed(2) + '" r="2.5" fill="' + s.color + '"/>';
        } else {
          out += '<polyline fill="none" stroke="' + s.color + '" stroke-width="1.8" stroke-linejoin="round" points="' +
            seg.map(p => xOf(p.t).toFixed(2) + ',' + yOf(p.v).toFixed(2)).join(' ') + '"/>';
        }
      }
    }
  } else {
    // 堆叠柱：桶宽按最小桶间距算，clamp 到绘图区。
    let minGap = Infinity;
    for (let i = 1; i < pts.length; i++) minGap = Math.min(minGap, pts[i].t - pts[i - 1].t);
    const bucketSpan = d.bucket === 'day' ? 24 * 3600 * 1000 : 3600 * 1000;
    if (!isFinite(minGap) || minGap <= 0) minGap = bucketSpan;
    const bw = Math.max(2, Math.min(30, iw * (minGap / (t1 - t0)) * 0.72));
    for (const pt of pts) {
      const x = Math.min(W - PR - bw, Math.max(PL, xOf(pt.t) - bw / 2));
      let acc = 0;
      for (const s of series) {
        const v = chTrendValue(pt, s, reqs);
        if (v === null || v <= 0) continue;
        const h = ih * (v / nice);
        const y = yBase - ih * ((acc + v) / nice);
        out += '<rect class="dbar" x="' + x.toFixed(2) + '" y="' + y.toFixed(2) + '" width="' + bw.toFixed(2) +
          '" height="' + Math.max(0.6, h).toFixed(2) + '" fill="' + s.color + '"/>';
        acc += v;
      }
    }
  }

  // 每桶命中区：透明整列，tooltip 汇总全部系列（比挂每段柱子看得全）。
  let step = pts.length > 1 ? Math.min((t1 - t0) / pts.length, pts[1].t - pts[0].t) : (t1 - t0);
  if (!isFinite(step) || step <= 0) step = 3600 * 1000;
  const hitW = Math.max(4, step * 0.9);
  for (const pt of pts) {
    const x = Math.max(PL, Math.min(W - PR - hitW, xOf(pt.t) - hitW / 2));
    out += '<rect class="chhit" x="' + x.toFixed(2) + '" y="' + PT.toFixed(2) + '" width="' + hitW.toFixed(2) +
      '" height="' + ih.toFixed(2) + '" data-tip="' + esc(chTipForBucket(pt, d, series, reqs)) + '"/>';
  }
  out += '</svg>';
  host.innerHTML = out;

  // 图注：合计/峰值 + 桶数 + 口径
  let note;
  if (chMeasure === 'rewrite') {
    let sum = 0;
    for (const pt of pts) for (const s of series) sum += Number(((pt.pt.kinds || {})[s.key]) || 0);
    note = '合计 ' + fmtTok(sum) + ' 条 · ' + pts.length + ' 个桶 · ' + chWinLabel(d);
  } else if (chMode === 'per1k') {
    let peak = 0;
    for (const pt of pts) {
      let sum = 0;
      for (const s of series) {
        const v = chTrendValue(pt, s, reqs);
        if (v !== null) sum += v;
      }
      if (sum > peak) peak = sum;
    }
    const peakText = peak >= 10 ? peak.toFixed(0) : peak >= 1 ? peak.toFixed(1) : peak.toFixed(2);
    note = '峰值 ' + peakText + '‰ · ' + pts.length + ' 个桶 · 每千请求（桶内 <10 请求不计）';
  } else {
    let sum = 0;
    for (const pt of pts) sum += chChartTotal(pt.pt);
    note = '合计 ' + fmtTok(sum) + ' 次 · ' + pts.length + ' 个桶 · ' + chWinLabel(d);
  }
  $('dgTrendNote').textContent = note;
}

/* renderChannelRateChart 失败率折线：与主图同粒度/同 x 域；无请求的桶不画点，
   断档分段（不把停机连成一段连续曲线）；均值参考线 + 均值/峰值/当前。 */
function renderChannelRateChart(d) {
  const host = $('dgRateChart');
  if (!host) return;
  const tz = d.tz || '';
  const pts = [];
  for (const p of d.requests || []) {
    const n = Number(p.req || 0);
    if (n <= 0) continue;
    const t = chPointTime({ t: p.t, scope: p.scope }, tz);
    if (t === null) continue;
    pts.push({ t: t, raw: p.t, scope: p.scope, req: n, err: Number(p.err || 0), rate: Number(p.err || 0) / n * 100 });
  }
  $('dgRateLegend').innerHTML = '<span class="legend"><i class="sw" style="background:var(--accent)"></i>失败率</span>';
  if (!pts.length) {
    host.innerHTML = '<div class="us-empty">该窗口内没有请求数据（用量账本未启用或窗口内无请求）。</div>';
    $('dgRateNote').textContent = '—';
    return;
  }
  let t0 = parseAPITime(d.window_from), t1 = parseAPITime(d.window_to);
  if (!t0 || !t1 || t1 <= t0) {
    t0 = pts[0].t;
    t1 = pts[pts.length - 1].t;
  }
  if (t1 <= t0) t1 = t0 + 1;
  const totalReq = pts.reduce((s, p) => s + p.req, 0);
  const avg = totalReq ? pts.reduce((s, p) => s + p.req * p.rate, 0) / totalReq : 0;
  const peak = pts.reduce((a, b) => (b.rate > a.rate ? b : a), pts[0]);
  const nice = chNicePct(Math.max(2, peak.rate * 1.15));

  const W = typeof chartWidth === 'function'
    ? chartWidth(host, 1200, 320, 1400)
    : (typeof chNarrow === 'function' && chNarrow() ? 440 : 1200);
  const narrow = W < 700;
  const H = narrow ? 170 : 220;
  const PL = narrow ? 46 : 56, PR = 16, PT = 14, PB = 26;
  const iw = W - PL - PR, ih = H - PT - PB, yBase = PT + ih;
  const xOf = t => PL + (t - t0) / (t1 - t0) * iw;
  const yOf = v => PT + ih - ih * (v / nice);

  let out = '<svg viewBox="0 0 ' + W + ' ' + H + '" role="img" aria-label="失败率趋势" preserveAspectRatio="xMidYMid meet">';
  for (let i = 0; i <= 4; i++) {
    const y = PT + ih - ih * i / 4;
    out += '<line class="gl" x1="' + PL + '" y1="' + y.toFixed(2) + '" x2="' + (W - PR) + '" y2="' + y.toFixed(2) + '"/>';
    out += '<text class="tk" x="' + (PL - 6) + '" y="' + (y + 4).toFixed(2) + '" text-anchor="end">' + (nice * i / 4).toFixed(1) + '%</text>';
  }
  const ticks = W < 420 ? 2 : (narrow ? 3 : 6);
  for (let k = 0; k < ticks; k++) {
    const target = t0 + (t1 - t0) * (ticks === 1 ? 0.5 : k / (ticks - 1));
    const x = Math.min(W - PR, Math.max(PL, xOf(target)));
    const anchor = k === 0 ? 'start' : (k === ticks - 1 ? 'end' : 'middle');
    out += '<text class="tk" x="' + x.toFixed(2) + '" y="' + (yBase + 16).toFixed(2) + '" text-anchor="' + anchor + '">' +
      esc(chAxisLabel(target, d.bucket, tz)) + '</text>';
  }
  // 均值参考线
  if (avg > 0) {
    const y = yOf(avg);
    out += '<line class="avg" x1="' + PL + '" y1="' + y.toFixed(2) + '" x2="' + (W - PR) + '" y2="' + y.toFixed(2) + '"/>';
  }
  // 折线（断档分段）
  for (const seg of chSegments(pts, p => p.rate, d.bucket)) {
    if (seg.length === 1) {
      out += '<circle cx="' + xOf(seg[0].t).toFixed(2) + '" cy="' + yOf(seg[0].v).toFixed(2) + '" r="2.5" fill="var(--accent)"/>';
    } else {
      out += '<polyline fill="none" stroke="var(--accent)" stroke-width="1.8" stroke-linejoin="round" points="' +
        seg.map(p => xOf(p.t).toFixed(2) + ',' + yOf(p.v).toFixed(2)).join(' ') + '"/>';
    }
  }
  // 每桶命中区（只对有请求的桶）
  let step = pts.length > 1 ? Math.min((t1 - t0) / pts.length, pts[1].t - pts[0].t) : (t1 - t0);
  if (!isFinite(step) || step <= 0) step = 3600 * 1000;
  const hitW = Math.max(4, step * 0.9);
  for (const p of pts) {
    const x = Math.max(PL, Math.min(W - PR - hitW, xOf(p.t) - hitW / 2));
    const tip = chBucketTitle({ t: p.raw, scope: p.scope }, tz) + '\n请求 ' + fmtTok(p.req) +
      ' · 失败 ' + fmtTok(p.err) + '\n失败率 ' + p.rate.toFixed(2) + '%';
    out += '<rect class="chhit" x="' + x.toFixed(2) + '" y="' + PT.toFixed(2) + '" width="' + hitW.toFixed(2) +
      '" height="' + ih.toFixed(2) + '" data-tip="' + esc(tip) + '"/>';
  }
  out += '</svg>';
  host.innerHTML = out;
  $('dgRateNote').textContent = '均值 ' + avg.toFixed(1) + '% · 峰值 ' + peak.rate.toFixed(1) + '% @ ' +
    chBucketTitle({ t: peak.raw, scope: peak.scope }, tz) + ' · ' + pts.length + ' 个有请求的桶';
}

/* chSortedKinds 事件表排序：默认注册表顺序；点表头后按次数/每千/最近一次排。 */
function chSortedKinds(d) {
  const rows = ((d && d.kinds) || []).slice();
  if (!chSortKey) return rows;
  const key = chSortKey, dir = chSortDir;
  const num = v => Number(v || 0);
  rows.sort((a, b) => {
    let d2;
    if (key === 'count') d2 = num(a.count) - num(b.count);
    else if (key === 'per1k') d2 = num(a.per_1k) - num(b.per_1k);
    else if (key === 'last') d2 = parseAPITime(a.last_at) - parseAPITime(b.last_at);
    else d2 = String(a.label || a.kind).localeCompare(String(b.label || b.kind), 'zh-Hans-CN');
    if (d2 === 0) d2 = String(a.kind).localeCompare(String(b.kind));
    return d2 * dir;
  });
  return rows;
}

/* chKindRowsHtml 事件分类表：注册表标签 + 分组 + 单位 + Hint。
   unit=work 的事件（归一/清洗/目录探测）不换算每千请求，显示「—」。 */
function chKindRowsHtml(rows) {
  if (!rows || !rows.length) {
    return '<tr><td colspan="5"><div class="empty inline">该窗口内没有通道事件。事件计数器启用后才会产生数据。</div></td></tr>';
  }
  return rows.map(k => {
    const per1k = k.unit === 'request' && Number(k.per_1k || 0) > 0 ? Number(k.per_1k).toFixed(2) : '—';
    const last = parseAPITime(k.last_at);
    const hint = k.hint || '';
    return '<tr data-kind="' + esc(k.kind) + '">' +
      '<td>' + esc(k.label || k.kind) + '</td>' +
      '<td class="num">' + fmtTok(Number(k.count || 0)) + '</td>' +
      '<td class="num">' + per1k + '</td>' +
      '<td>' + (last ? esc(fmtStampShort(last)) : '—') + '</td>' +
      '<td class="ch-hint" title="' + esc(hint) + '">' + esc(hint) + '</td>' +
      '</tr>';
  }).join('');
}

/* chRealmRowsHtml 分域对比：只比较 cn/global，未标注不占一行。
   覆盖率低于 10% 时也照常渲染（低流量部署不该永远显示「积累中」），
   由表头 note 说明带域标记占比。 */
function chRealmRowsHtml(d) {
  const realms = (d && d.realms) || {};
  const labeled = Number((d && d.realm_labeled) || 0);
  const total = Number((d && d.realm_total) || 0);
  if (!labeled) {
    return '<tr><td colspan="5"><div class="empty inline">该窗口内没有带域标记的请求' +
      (total ? '（共 ' + fmtTok(total) + ' 条归档，1.21.0 之前没有域字段）' : '') + '。</div></td></tr>';
  }
  const order = ['cn', 'global'];
  const keys = order.filter(k => realms[k]).concat(Object.keys(realms).filter(k => order.indexOf(k) < 0 && k !== 'unknown'));
  return keys.map(k => {
    const r = realms[k] || {};
    const rate = Number(r.success_rate || 0);
    const pct = Math.max(0, Math.min(100, rate));
    return '<tr>' +
      '<td>' + esc(k) + '</td>' +
      '<td class="num">' + fmtTok(Number(r.requests || 0)) + '</td>' +
      '<td class="num">' + fmtTok(Number(r.success || 0)) + '</td>' +
      '<td class="num"><span class="ch-rate"><i style="width:' + pct.toFixed(1) + '%"></i></span>' + rate.toFixed(1) + '%</td>' +
      '<td class="num">' + fmtTok(Number(r.fallbacks || 0)) + '</td>' +
      '</tr>';
  }).join('');
}

/* chBindKindSort 事件表表头排序（点击切换升/降序）。 */
function chBindKindSort() {
  const ths = document.querySelectorAll('#dgKindTable th[data-sort]');
  if (!ths.length) return;
  ths.forEach(th => {
    th.onclick = () => {
      const key = th.dataset.sort;
      if (chSortKey === key) chSortDir = -chSortDir; else { chSortKey = key; chSortDir = 1; }
      ths.forEach(x => x.removeAttribute('aria-sort'));
      th.setAttribute('aria-sort', chSortDir > 0 ? 'ascending' : 'descending');
      if (channelData) $('dgKindBody').innerHTML = chKindRowsHtml(chSortedKinds(channelData));
    };
  });
}

/* renderChannelHealth 看板总装：状态条 / 覆盖条 / KPI / 两张图 / 两张表。 */
function renderChannelHealth(d) {
  channelData = d || {};
  const realms = channelData.realms || {};
  let fallbacks = 0;
  for (const k in realms) fallbacks += Number(realms[k].fallbacks || 0);
  $('dgState').innerHTML = chStateChips(channelData.state || null, fallbacks);
  $('dgCover').innerHTML = chCoverText(channelData);
  renderChannelKpis(channelData);
  renderChannelTrend(channelData);
  renderChannelRateChart(channelData);
  $('dgKindBody').innerHTML = chKindRowsHtml(chSortedKinds(channelData));
  $('dgRealmBody').innerHTML = chRealmRowsHtml(channelData);

  const kinds = channelData.kinds || [];
  $('dgKindNote').textContent = kinds.length + ' 类 · 每千请求只对按请求计的事件换算';
  const labeled = Number(channelData.realm_labeled || 0);
  const total = Number(channelData.realm_total || 0);
  $('dgRealmNote').textContent = total
    ? '带域标记 ' + fmtTok(labeled) + ' / ' + fmtTok(total) + '（' + (total ? (labeled / total * 100).toFixed(0) : 0) + '%）'
    : '—';
  $('dgUpdated').textContent = '最后更新 ' + new Date().toLocaleTimeString('zh-CN', { hour12: false });
}

/* chSyncSeg 同步分段控件的选中态。 */
function chSyncSeg() {
  const m = $('dgMeasure');
  if (m) m.querySelectorAll('button[data-m]').forEach(b => b.classList.toggle('on', b.dataset.m === chMeasure));
  const mode = $('dgMode');
  if (mode) mode.querySelectorAll('button[data-mode]').forEach(b => b.classList.toggle('on', b.dataset.mode === chMode));
}

/* chStartAuto 启动自动刷新（隐藏标签页/非本视图时跳过）。 */
function chStartAuto() {
  if (chTimer) { clearInterval(chTimer); chTimer = null; }
  if (!(chAutoSec > 0)) return;
  chTimer = setInterval(() => {
    if (document.hidden || view !== 'channel') return;
    loadChannelHealth(true);
  }, chAutoSec * 1000);
}

async function loadChannelHealth(quiet) {
  const q = trangeQuery('dgRange', false);
  // 「全部历史」预设不发区间；本接口缺省是近 7 天，这里显式从 1970 起，
  // 由后端按 90 天保留期截断（与账本保留时长一致）。
  if (!q.has('from') && !q.has('to')) q.set('from', '1');
  const btn = $('btnDg');
  const done = !quiet && btn ? setBusy(btn, '刷新中…') : null;
  try {
    const d = await api('channel_health?' + q.toString());
    renderChannelHealth(d);
  } catch (e) {
    if (quiet) return; // 自动刷新失败保留旧数据，不闪屏
    channelData = null;
    $('dgState').innerHTML = '<span class="ch-badge mute">读取失败</span>';
    $('dgCover').textContent = '—';
    $('dgStats').innerHTML = '<div class="ch-error">读取失败：' + esc(shortErr(e.message)) +
      ' <button class="xs" id="dgRetry">重试</button></div>';
    $('dgKindBody').innerHTML = '<tr><td colspan="5"><div class="empty inline">读取失败</div></td></tr>';
    $('dgRealmBody').innerHTML = '<tr><td colspan="5"><div class="empty inline">读取失败</div></td></tr>';
    $('dgTrendChart').innerHTML = '<div class="us-empty">读取失败：' + esc(shortErr(e.message)) + '</div>';
    $('dgRateChart').innerHTML = '<div class="us-empty">读取失败：' + esc(shortErr(e.message)) + '</div>';
    $('dgTrendNote').textContent = '—';
    $('dgRateNote').textContent = '—';
    $('dgKindNote').textContent = '—';
    $('dgRealmNote').textContent = '—';
    $('dgTrendLegend').innerHTML = '';
    $('dgRateLegend').innerHTML = '';
    if ($('dgRetry')) $('dgRetry').onclick = () => loadChannelHealth();
  } finally {
    if (done) done('刷新');
  }
}

/* 分段控件：切换度量/量纲只重画主图，不重新打接口。 */
if ($('dgMeasure')) {
  $('dgMeasure').onclick = e => {
    const b = e.target.closest('button[data-m]');
    if (!b) return;
    chMeasure = b.dataset.m;
    try { localStorage.setItem('wb2api.ch.measure', chMeasure); } catch (err) {}
    chSyncSeg();
    if (channelData) renderChannelTrend(channelData);
  };
}
if ($('dgMode')) {
  $('dgMode').onclick = e => {
    const b = e.target.closest('button[data-mode]');
    if (!b) return;
    chMode = b.dataset.mode;
    try { localStorage.setItem('wb2api.ch.mode', chMode); } catch (err) {}
    chSyncSeg();
    if (channelData) renderChannelTrend(channelData);
  };
}
if ($('dgAuto')) {
  $('dgAuto').value = String(chAutoSec);
  $('dgAuto').onchange = () => {
    chAutoSec = Number($('dgAuto').value) || 0;
    try { localStorage.setItem('wb2api.ch.auto', String(chAutoSec)); } catch (err) {}
    chStartAuto();
  };
}
chSyncSeg();
chBindKindSort();
chStartAuto();
if ($('btnDg')) $('btnDg').onclick = () => loadChannelHealth();
if ($('dgRange')) trangeBind('dgRange', loadChannelHealth, '24');

/* ══ 剩余积分走势（积分构成页顶部）═══════════════════════════════════════
   采样点是「余额后台刷新 / 签到结束时」落的（后端 creditline），所以这张图不需要
   打上游；采样依赖 schedule.balance_refresh_enabled，关掉后不再产生新点，
   空态会如实说明。 */

let creditTrendData = null;

/* renderCreditTrend 画三条折线（合计 / CN / Global）。
   x 轴用真实时间轴（采样点不等距，等距排布会把停机断档画成一段正常曲线）；
   y 轴从 0 起——积分为负没有意义，从 0 起才能直观看出「还剩多少」。 */
function renderCreditTrend(d) {
  const host = $('ctChart');
  if (!host) return;
  creditTrendData = d || {};
  const pts = [];
  for (const p of (d && d.points) || []) {
    const t = parsePointTime({ t: p.t, scope: p.scope });
    if (t === null) continue;
    pts.push({ t, raw: p.t, scope: p.scope,
               total: Number(p.total || 0), cn: Number(p.cn || 0), gl: Number(p.global || 0) });
  }
  const cur = (d && d.current) || {};
  const curTxt = Number(cur.accounts || 0) > 0
    ? '当前合计 ' + fmtTok(cur.total) +
      '（CN ' + fmtTok(cur.cn) + ' · Global ' + fmtTok(cur.global) + '）' +
      (Number(cur.unlimited || 0) > 0 ? ' · 另有 ' + cur.unlimited + ' 个不限量账号未计入' : '')
    : '当前池内无账号（历史曲线仍保留）';
  $('ctNote').textContent = curTxt;
  $('ctNote').title = curTxt;
  if (!pts.length) {
    // 空态说清「为什么没有点」：采样依赖余额后台刷新，关掉后只在签到时落点。
    const on = d && d.refresh_enabled;
    const every = Number((d && d.refresh_sec) || 0);
    host.innerHTML = '<div class="us-empty">' + (on
      ? '还没有采样点：余额后台刷新（每 ' + Math.max(1, Math.round(every / 60)) +
        ' 分钟）或签到结束后会自动记录，稍后刷新即可。'
      : '还没有采样点：余额后台刷新已关闭（schedule.balance_refresh_enabled=false），' +
        '只在签到时采样；开启后约一个刷新周期即可看到曲线。') + '</div>';
    $('ctChartNote').textContent = '—';
    return;
  }
  // 画布宽度跟随容器：1024/768 不再把 1200 图缩成 0.6 倍，手机也不放大小图。
  const W = typeof chartWidth === 'function' ? chartWidth(host, 1200, 320, 1400) : 1200;
  const H = 220, PL = 58, PR = 14, PT = 16, PB = 30;
  const iw = W - PL - PR, ih = H - PT - PB, yBase = PT + ih;
  const t0 = pts[0].t, t1 = pts[pts.length - 1].t;
  const span = Math.max(1, t1 - t0);
  const max = Math.max(1, ...pts.map(p => Math.max(p.total, p.cn, p.gl)));
  const step = typeof niceStep === 'function' ? niceStep(max, 4) : max;
  const axisMax = step * 4;
  const tickFmt = typeof fmtTokTick === 'function' ? fmtTokTick : fmtTok;
  const axisFmt = typeof fmtAxisTimeLabel === 'function' ? fmtAxisTimeLabel
    : (ms => String(new Date(ms).getHours()).padStart(2, '0') + ':00');
  const xOf = t => pts.length === 1 ? PL + iw / 2 : PL + (t - t0) / span * iw;
  const yOf = v => yBase - ih * (v / axisMax);

  let out = '<svg viewBox="0 0 ' + W + ' ' + H + '" role="img" aria-label="剩余积分走势：' +
            esc(curTxt) + '" preserveAspectRatio="xMidYMid meet">';
  for (let i = 0; i <= 4; i++) {
    const y = yBase - (ih * i / 4);
    out += '<line class="gl" x1="' + PL + '" y1="' + y.toFixed(1) + '" x2="' + (W - PR) +
           '" y2="' + y.toFixed(1) + '"/>';
    out += '<text class="tk" x="' + (PL - 6) + '" y="' + (y + 3.5).toFixed(1) +
           '" text-anchor="end">' + tickFmt(axisMax * i / 4) + '</text>';
  }
  // 合计的面积填充先画（在折线之下，不遮线）。
  if (pts.length > 1) {
    let area = 'M' + xOf(pts[0].t).toFixed(1) + ',' + yBase.toFixed(1);
    for (const p of pts) area += ' L' + xOf(p.t).toFixed(1) + ',' + yOf(p.total).toFixed(1);
    area += ' L' + xOf(pts[pts.length - 1].t).toFixed(1) + ',' + yBase.toFixed(1) + ' Z';
    out += '<path class="ctarea" d="' + area + '"/>';
  }
  const line = (key, cls) => {
    if (pts.length < 2) return '';
    const dstr = pts.map(p => xOf(p.t).toFixed(1) + ',' + yOf(p[key]).toFixed(1)).join(' ');
    return '<polyline class="ctl ' + cls + '" points="' + dstr + '"/>';
  };
  out += line('cn', 'ctl-cn') + line('gl', 'ctl-gl') + line('total', 'ctl-total');
  // 采样点 ≤ 60 时画点并给 tooltip（点多时圆点会糊成一条粗线）。
  if (pts.length <= 60) {
    for (const p of pts) {
      out += '<g><title>' + esc(p.raw) + '  合计 ' + fmtTok(p.total) +
             ' · CN ' + fmtTok(p.cn) + ' · Global ' + fmtTok(p.gl) + '</title>' +
             '<circle class="ctdot" cx="' + xOf(p.t).toFixed(1) + '" cy="' + yOf(p.total).toFixed(1) +
             '" r="2.6"/></g>';
    }
  }
  out += '<line class="ax" x1="' + PL + '" y1="' + yBase + '" x2="' + (W - PR) +
         '" y2="' + yBase + '"/>';
  // x 轴刻度：首尾 + 中间，取最近的真实采样点；窄屏只放 4 个。
  const TICKS = Math.min(W < 420 ? 2 : (W < 700 ? 3 : (W < 900 ? 5 : 6)), pts.length);
  const used = new Set();
  for (let k = 0; k < TICKS; k++) {
    const target = t0 + span * (TICKS === 1 ? 0.5 : k / (TICKS - 1));
    let bi = 0, best = Infinity;
    for (let i = 0; i < pts.length; i++) {
      const dd = Math.abs(pts[i].t - target);
      if (dd < best) { best = dd; bi = i; }
    }
    if (used.has(bi)) continue;
    used.add(bi);
    const p = pts[bi];
    const cx = xOf(p.t);
    const anchor = cx < PL + 14 ? 'start' : (cx > W - PR - 14 ? 'end' : 'middle');
    out += '<text class="tk" x="' + Math.max(PL, Math.min(W - PR, cx)).toFixed(1) +
           '" y="' + (PT + ih + 15) + '" text-anchor="' + anchor + '">' +
           esc(axisFmt(p.t, t0, t1, d.bucket === 'day' ? 'day' : 'hour')) + '</text>';
  }
  out += '</svg>';
  host.innerHTML = out;
  $('ctChartNote').textContent = pts.length + ' 个采样点 · ' +
    (d.bucket === 'day' ? '按日' : '按小时') + ' · 最近 ' +
    (d.last_sample_at ? fmtStampShort(d.last_sample_at) : '—');
}

async function loadCreditTrend() {
  const q = trangeQuery('ctRange', false);
  try {
    const d = await api('credit_trend?' + q.toString());
    renderCreditTrend(d);
  } catch (e) {
    creditTrendData = null;
    $('ctChart').innerHTML = '<div class="us-empty">读取失败：' + esc(shortErr(e.message)) + '</div>';
    $('ctChartNote').textContent = '—';
    $('ctNote').textContent = '读取失败';
  }
}
if ($('btnCt')) $('btnCt').onclick = loadCreditTrend;
if ($('ctRange')) trangeBind('ctRange', loadCreditTrend, '168');

