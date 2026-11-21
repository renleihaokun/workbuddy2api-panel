/* ── 原生控件接管层 ─────────────────────────────────────────────────────
   只做两件事：
   1. `<select>` 的展开弹层：原生 select 仍是唯一的语义与取值来源（读屏、表单、
      change 事件全部照旧），我们只在它上面盖一层自绘 listbox——因为系统候选框
      是浏览器/OS 画的独立窗口，CSS 碰不到，截图也拍不到。
   2. `confirm()` 的确认框：换成与面板同一套视觉的对话框，返回 Promise<boolean>。

   本文件独立于 app.js：不依赖任何面板函数，加载失败时原生控件仍能工作
   （select 只是外观被 appearance:none 改了，点击会回落到系统弹层）。 */
(function () {
  'use strict';

  /* ══ 通用：定位 / 关闭 / 键盘 ══════════════════════════════════════ */
  var active = null; // { sel, pop, veil, items, idx, sheet }

  function isSheetMode() {
    return window.matchMedia('(max-width: 760px)').matches ||
      (window.matchMedia && window.matchMedia('(pointer: coarse)').matches);
  }

  function fieldLabel(sel) {
    var aria = sel.getAttribute('aria-label');
    if (aria) return aria.trim();
    var id = sel.id;
    if (id) {
      var l = document.querySelector('label[for="' + id.replace(/"/g, '\\"') + '"]');
      if (l) return l.textContent.trim();
    }
    var wrap = sel.closest('label');
    if (wrap) {
      var lb = wrap.querySelector('.lb');
      if (lb) return lb.textContent.trim();
    }
    return '请选择';
  }

  function buildItems(sel) {
    var out = [];
    for (var i = 0; i < sel.options.length; i++) {
      var o = sel.options[i];
      out.push({ i: i, text: o.textContent || o.label || '', disabled: !!o.disabled });
    }
    return out;
  }

  function positionPop() {
    if (!active || active.sheet) return;
    var r = active.sel.getBoundingClientRect();
    var pop = active.pop;
    var w = Math.max(160, r.width);
    pop.style.width = w + 'px';
    pop.style.left = Math.max(8, Math.min(r.left, window.innerWidth - w - 8)) + 'px';
    var h = pop.offsetHeight || 0;
    var below = r.bottom + 4;
    var above = r.top - h - 4;
    pop.style.top = (below + h > window.innerHeight - 8 && above > 8 ? above : below) + 'px';
  }

  function renderPop() {
    if (!active) return;
    var pop = active.pop;
    // 只重建选项行，保留 sheet 的拖拽条/标题
    var rows = pop.querySelectorAll('.sel-opt');
    for (var i = 0; i < rows.length; i++) rows[i].remove();
    active.items.forEach(function (it, k) {
      var d = document.createElement('div');
      d.className = 'sel-opt' + (it.i === active.sel.selectedIndex ? ' on' : '') + (k === active.idx ? ' act' : '');
      d.setAttribute('role', 'option');
      d.setAttribute('aria-disabled', it.disabled ? 'true' : 'false');
      d.innerHTML = '<svg class="ck" viewBox="0 0 12 12" fill="none" stroke="currentColor" stroke-width="1.8" ' +
        'stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M2 6.2 4.8 9 10 3.4"/></svg>' +
        '<span class="txt"></span>';
      d.querySelector('.txt').textContent = it.text;
      d.addEventListener('pointerdown', function (e) {
        e.preventDefault();
        e.stopPropagation();
        if (!it.disabled) choose(it.i);
      });
      d.addEventListener('mouseenter', function () { setActive(k, false); });
      pop.appendChild(d);
    });
    var act = pop.querySelector('.sel-opt.act');
    if (act && act.scrollIntoView) act.scrollIntoView({ block: 'nearest' });
  }

  function setActive(k, rerender) {
    if (!active) return;
    active.idx = k;
    if (rerender) renderPop();
    else {
      var rows = active.pop.querySelectorAll('.sel-opt');
      for (var i = 0; i < rows.length; i++) rows[i].classList.toggle('act', i === k);
      if (rows[k] && rows[k].scrollIntoView) rows[k].scrollIntoView({ block: 'nearest' });
    }
  }

  function move(delta) {
    if (!active) return;
    var n = active.items.length;
    var k = active.idx;
    for (var step = 0; step < n; step++) {
      k = (k + delta + n) % n;
      if (!active.items[k].disabled) { setActive(k, false); return; }
    }
  }

  function choose(i) {
    if (!active) return;
    var sel = active.sel;
    var it = active.items[i];
    if (!it || it.disabled) return;
    if (sel.selectedIndex !== i) {
      sel.selectedIndex = i;
      sel.dispatchEvent(new Event('input', { bubbles: true }));
      sel.dispatchEvent(new Event('change', { bubbles: true }));
    }
    closePop();
    try { sel.focus({ preventScroll: true }); } catch (e) { sel.focus(); }
  }

  function closePop() {
    if (!active) return;
    var sel = active.sel;
    if (active.pop) active.pop.remove();
    if (active.veil) active.veil.remove();
    active = null;
    if (sel) sel.removeAttribute('aria-expanded');
    document.removeEventListener('keydown', onPopKey, true);
    window.removeEventListener('resize', closePop, true);
    window.removeEventListener('scroll', closePop, true);
  }

  function onPopKey(e) {
    if (!active) return;
    if (e.key === 'Escape') {
      e.preventDefault(); e.stopPropagation();
      var sel = active.sel;
      closePop();
      if (sel) { try { sel.focus({ preventScroll: true }); } catch (err) { sel.focus(); } }
      return;
    }
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault(); e.stopPropagation();
      move(e.key === 'ArrowDown' ? 1 : -1);
      return;
    }
    if (e.key === 'Home' || e.key === 'End') {
      e.preventDefault(); e.stopPropagation();
      setActive(e.key === 'Home' ? 0 : active.items.length - 1, false);
      return;
    }
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault(); e.stopPropagation();
      var it = active.items[active.idx];
      if (it && !it.disabled) choose(it.i);
      return;
    }
    if (e.key === 'Tab') { closePop(); }
  }

  function openPop(sel) {
    if (active) closePop();
    var items = buildItems(sel);
    if (!items.length) return;
    var sheet = isSheetMode();
    var pop = document.createElement('div');
    pop.className = sheet ? 'sel-sheet' : 'sel-pop';
    // 原生 select 负责语义，自绘层只是视觉镜像，避免读屏听到两份选项。
    pop.setAttribute('aria-hidden', 'true');
    if (sheet) {
      var grab = document.createElement('div');
      grab.className = 'grab';
      pop.appendChild(grab);
      var title = document.createElement('div');
      title.className = 'note';
      title.style.padding = '0 6px 6px';
      title.textContent = fieldLabel(sel);
      pop.appendChild(title);
    }
    document.body.appendChild(pop);
    var veil = null;
    if (sheet) {
      veil = document.createElement('div');
      veil.className = 'sel-veil';
      veil.addEventListener('pointerdown', function (e) { e.preventDefault(); closePop(); });
      document.body.appendChild(veil);
    }
    var idx = Math.max(0, sel.selectedIndex);
    active = { sel: sel, pop: pop, veil: veil, items: items, idx: idx, sheet: sheet };
    renderPop();
    if (!sheet) positionPop();
    sel.setAttribute('aria-expanded', 'true');
    document.addEventListener('keydown', onPopKey, true);
    window.addEventListener('resize', closePop, true);
    window.addEventListener('scroll', closePop, true);
  }

  /* ── select 的点击接管 ───────────────────────────────────────────────
     原生弹层在 mousedown 时打开，所以必须同时拦 pointerdown 和 mousedown。
     pointerdown 负责开我们的层；紧随其后的 mousedown 只做 preventDefault，
     不能再 toggle（否则一开一关）。 */
  var suppressMouseUntil = 0;
  function fromSelect(e) {
    var t = e.target;
    if (!t || !t.closest) return null;
    var sel = t.closest('select');
    if (!sel || sel.disabled || sel.dataset.noUpgrade === '1') return null;
    return sel;
  }
  document.addEventListener('pointerdown', function (e) {
    var sel = fromSelect(e);
    if (!sel) return;
    if (e.pointerType === 'mouse' && e.button !== 0) return;
    e.preventDefault();
    suppressMouseUntil = Date.now() + 600;
    try { sel.focus({ preventScroll: true }); } catch (err) { sel.focus(); }
    if (active && active.sel === sel) closePop(); else openPop(sel);
  }, true);
  document.addEventListener('mousedown', function (e) {
    var sel = fromSelect(e);
    if (!sel) return;
    if (e.button !== 0) return;
    if (Date.now() < suppressMouseUntil) { e.preventDefault(); return; }
    // 没有 pointerdown 的旧内核：这里兜底接管
    e.preventDefault();
    try { sel.focus({ preventScroll: true }); } catch (err) { sel.focus(); }
    if (active && active.sel === sel) closePop(); else openPop(sel);
  }, true);
  // 键盘打开：Enter / 空格 / 方向键 / F4（select 的原生弹层也走这些键）
  document.addEventListener('keydown', function (e) {
    if (active) return;
    var t = e.target;
    if (!t || t.tagName !== 'SELECT' || t.disabled || t.dataset.noUpgrade === '1') return;
    if (e.key === 'Enter' || e.key === ' ' || e.key === 'ArrowDown' || e.key === 'ArrowUp' || e.key === 'F4') {
      e.preventDefault();
      openPop(t);
    }
  }, true);
  // 点击页面其它地方：关闭
  document.addEventListener('pointerdown', function (e) {
    if (!active) return;
    if (active.pop && active.pop.contains(e.target)) return;
    if (e.target === active.sel) return;
    closePop();
  }, true);

  /* ══ 自绘 tooltip：把 title 换成同一套视觉的浮层 ═════════════════════
     系统黄气泡有三个问题：1 秒延迟、不可主题化、手机完全不可达。这里把
     title 搬进 data-tip 后由 pointerover/focusin 驱动，读屏仍能用 aria-label
     （元素没有文本时自动补一份）。<tr title> 直接删掉：整行提示会挡住数据。 */
  var tipEl = null, tipTimer = null, tipFor = null;

  function ensureTip() {
    if (!tipEl) {
      tipEl = document.createElement('div');
      tipEl.className = 'wb-tip';
      tipEl.setAttribute('role', 'tooltip');
      document.body.appendChild(tipEl);
    }
    return tipEl;
  }
  function showTip(el) {
    var text = el.getAttribute('data-tip');
    if (!text) return;
    var t = ensureTip();
    t.textContent = text;
    t.classList.add('on');
    var r = el.getBoundingClientRect();
    var w = t.offsetWidth, h = t.offsetHeight;
    var left = Math.min(Math.max(8, r.left + r.width / 2 - w / 2), window.innerWidth - w - 8);
    var top = r.bottom + 8;
    if (top + h > window.innerHeight - 8) top = r.top - h - 8;
    t.style.left = Math.max(8, left) + 'px';
    t.style.top = Math.max(8, top) + 'px';
  }
  function hideTip() {
    if (tipEl) tipEl.classList.remove('on');
    tipFor = null;
  }
  function upgradeTitles(root) {
    var nodes = root && root.querySelectorAll ? root.querySelectorAll('[title]') : [];
    for (var i = 0; i < nodes.length; i++) {
      var el = nodes[i];
      var text = el.getAttribute('title');
      if (!text) { el.removeAttribute('title'); continue; }
      // 整行 title（账号行 / 请求行）只会挡住下面的数据，直接丢弃。
      if (el.tagName === 'TR') { el.removeAttribute('title'); continue; }
      if (!el.getAttribute('aria-label') && !(el.textContent || '').trim()) {
        el.setAttribute('aria-label', text);
      }
      el.setAttribute('data-tip', text);
      el.removeAttribute('title');
    }
  }
  var tipPending = false;
  function scheduleTipUpgrade() {
    if (tipPending) return;
    tipPending = true;
    setTimeout(function () { tipPending = false; upgradeTitles(document); }, 60);
  }
  document.addEventListener('pointerover', function (e) {
    var el = e.target && e.target.closest ? e.target.closest('[data-tip]') : null;
    if (!el) return;
    if (tipFor === el) return;
    clearTimeout(tipTimer);
    tipTimer = setTimeout(function () { tipFor = el; showTip(el); }, 380);
  }, true);
  document.addEventListener('pointerout', function (e) {
    var el = e.target && e.target.closest ? e.target.closest('[data-tip]') : null;
    if (!el) return;
    clearTimeout(tipTimer);
    if (tipFor === el) hideTip();
  }, true);
  document.addEventListener('focusin', function (e) {
    var el = e.target && e.target.closest ? e.target.closest('[data-tip]') : null;
    if (el) { clearTimeout(tipTimer); tipFor = el; showTip(el); }
  }, true);
  document.addEventListener('focusout', function () { clearTimeout(tipTimer); hideTip(); }, true);
  window.addEventListener('scroll', function () { clearTimeout(tipTimer); hideTip(); }, true);
  upgradeTitles(document);
  upgradeDateTimes(document);
  labelFields(document);
  new MutationObserver(function (muts) {
    for (var i = 0; i < muts.length; i++) {
      if (muts[i].type === 'attributes' || (muts[i].addedNodes && muts[i].addedNodes.length)) { scheduleTipUpgrade(); scheduleDTUpgrade(); labelFields(document); return; }
    }
  }).observe(document.body, { subtree: true, childList: true, attributes: true, attributeFilter: ['title'] });

  /* ══ 行内「更多」菜单 ═══════════════════════════════════════════════
     表格最后一列的空间很贵：把低频且危险的操作收进这里，行宽能少 80px。
     与候选层一样用 fixed 定位挂 body，避免被 .tbl-wrap 的 overflow 裁掉。 */
  var activeMenu = null;

  function closeMenu() {
    if (!activeMenu) return;
    activeMenu.pop.remove();
    activeMenu.anchor.setAttribute('aria-expanded', 'false');
    activeMenu = null;
    document.removeEventListener('keydown', onMenuKey, true);
    document.removeEventListener('pointerdown', onMenuAway, true);
    window.removeEventListener('resize', closeMenu, true);
    window.removeEventListener('scroll', closeMenu, true);
  }
  function onMenuKey(e) {
    if (e.key !== 'Escape') return;
    e.preventDefault(); e.stopPropagation();
    var a = activeMenu && activeMenu.anchor;
    closeMenu();
    if (a) { try { a.focus({ preventScroll: true }); } catch (err) { a.focus(); } }
  }
  function onMenuAway(e) {
    if (!activeMenu) return;
    if (activeMenu.pop.contains(e.target) || e.target === activeMenu.anchor) return;
    closeMenu();
  }

  function openMenu(anchor, items) {
    closePop();
    closeMenu();
    if (!anchor || !items || !items.length) return;
    var pop = document.createElement('div');
    pop.className = 'sel-pop ctx-pop';
    pop.setAttribute('role', 'menu');
    items.forEach(function (it) {
      var b = document.createElement('button');
      b.type = 'button';
      b.className = 'ctx-item' + (it.danger ? ' danger' : '');
      b.setAttribute('role', 'menuitem');
      b.textContent = it.label;
      b.disabled = !!it.disabled;
      b.addEventListener('click', function () {
        if (b.disabled) return;
        closeMenu();
        if (it.onPick) it.onPick();
      });
      pop.appendChild(b);
    });
    document.body.appendChild(pop);
    var r = anchor.getBoundingClientRect();
    var w = pop.offsetWidth || 132;
    pop.style.left = Math.max(8, Math.min(r.right - w, window.innerWidth - w - 8)) + 'px';
    var h = pop.offsetHeight || 0;
    pop.style.top = (r.bottom + 4 + h > window.innerHeight - 8 ? r.top - h - 4 : r.bottom + 4) + 'px';
    anchor.setAttribute('aria-expanded', 'true');
    activeMenu = { anchor: anchor, pop: pop };
    var first = pop.querySelector('.ctx-item');
    if (first) setTimeout(function () { first.focus(); }, 0);
    document.addEventListener('keydown', onMenuKey, true);
    document.addEventListener('pointerdown', onMenuAway, true);
    window.addEventListener('resize', closeMenu, true);
    window.addEventListener('scroll', closeMenu, true);
  }

  /* ══ 自绘日期时间（datetime-local） ═════════════════════════════════ */
  var activeDT = null;
  function pad2(n) { return String(n).padStart(2, '0'); }
  function parseDT(v) { if (!v) return null; var d = new Date(v); return isNaN(d.getTime()) ? null : d; }
  function dtValue(d) {
    return d.getFullYear() + '-' + pad2(d.getMonth() + 1) + '-' + pad2(d.getDate()) + 'T' +
      pad2(d.getHours()) + ':' + pad2(d.getMinutes());
  }
  function closeDT() {
    if (!activeDT) return;
    var inp = activeDT.input;
    activeDT.pop.remove();
    activeDT = null;
    document.removeEventListener('keydown', onDTKey, true);
    document.removeEventListener('pointerdown', onDTAway, true);
    window.removeEventListener('resize', closeDT, true);
    window.removeEventListener('scroll', closeDT, true);
    if (inp) { try { inp.focus({ preventScroll: true }); } catch (e) { inp.focus(); } }
  }
  function onDTKey(e) { if (activeDT && e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); closeDT(); } }
  function onDTAway(e) {
    if (!activeDT) return;
    if (activeDT.pop.contains(e.target) || e.target === activeDT.input) return;
    closeDT();
  }
  function openDT(input) {
    closePop(); closeMenu(); closeDT();
    var base = parseDT(input.value) || new Date();
    var sel = new Date(base.getFullYear(), base.getMonth(), base.getDate(), base.getHours(), base.getMinutes());
    var viewMonth = new Date(base.getFullYear(), base.getMonth(), 1);
    var pop = document.createElement('div');
    pop.className = 'dt-pop';
    pop.setAttribute('role', 'dialog');
    pop.setAttribute('aria-label', '选择日期时间');
    document.body.appendChild(pop);
    var hours = sel.getHours(), minutes = sel.getMinutes();

    function render() {
      pop.innerHTML =
        '<div class="dt-head"><button type="button" data-nav="-1" aria-label="上个月">‹</button>' +
        '<span class="dt-title"></span><button type="button" data-nav="1" aria-label="下个月">›</button></div>' +
        '<div class="dt-week"><span>一</span><span>二</span><span>三</span><span>四</span><span>五</span><span>六</span><span>日</span></div>' +
        '<div class="dt-grid"></div>' +
        '<div class="dt-time"><input class="dt-h" type="text" inputmode="numeric" aria-label="小时">' +
        '<span>:</span><input class="dt-m" type="text" inputmode="numeric" aria-label="分钟"></div>' +
        '<div class="dt-foot"><button type="button" class="ghost" data-act="today">今天</button>' +
        '<button type="button" class="ghost" data-act="now">现在</button><span class="grow"></span>' +
        '<button type="button" data-act="cancel">取消</button>' +
        '<button type="button" class="primary" data-act="ok">确定</button></div>';
      pop.querySelector('.dt-title').textContent = viewMonth.getFullYear() + ' 年 ' + (viewMonth.getMonth() + 1) + ' 月';
      var hv = pop.querySelector('.dt-h'), mv = pop.querySelector('.dt-m');
      hv.value = pad2(hours); mv.value = pad2(minutes);
      hv.addEventListener('change', function () { hours = Math.max(0, Math.min(23, parseInt(hv.value, 10) || 0)); hv.value = pad2(hours); });
      mv.addEventListener('change', function () { minutes = Math.max(0, Math.min(59, parseInt(mv.value, 10) || 0)); mv.value = pad2(minutes); });
      var grid = pop.querySelector('.dt-grid');
      var first = new Date(viewMonth.getFullYear(), viewMonth.getMonth(), 1);
      var offset = (first.getDay() + 6) % 7; // 周一开头
      var start = new Date(first); start.setDate(1 - offset);
      var today = new Date();
      for (var i = 0; i < 42; i++) {
        var day = new Date(start); day.setDate(start.getDate() + i);
        var b = document.createElement('button');
        b.type = 'button'; b.className = 'dt-day'; b.textContent = String(day.getDate());
        if (day.getMonth() !== viewMonth.getMonth()) b.classList.add('out');
        if (day.toDateString() === today.toDateString()) b.classList.add('today');
        if (day.getFullYear() === sel.getFullYear() && day.getMonth() === sel.getMonth() && day.getDate() === sel.getDate()) b.classList.add('sel');
        b.dataset.y = day.getFullYear(); b.dataset.m = day.getMonth(); b.dataset.d = day.getDate();
        b.setAttribute('aria-label', day.getFullYear() + '-' + pad2(day.getMonth() + 1) + '-' + pad2(day.getDate()));
        b.addEventListener('click', function (ev) {
          var t = ev.currentTarget;
          sel = new Date(Number(t.dataset.y), Number(t.dataset.m), Number(t.dataset.d), hours, minutes);
          render();
        });
        grid.appendChild(b);
      }
    }
    render();
    // 定位：默认贴输入框下方，空间不够翻到上方
    var r = input.getBoundingClientRect();
    var w = pop.offsetWidth || 288, h = pop.offsetHeight || 320;
    pop.style.left = Math.max(8, Math.min(r.left, window.innerWidth - w - 8)) + 'px';
    pop.style.top = (r.bottom + 6 + h > window.innerHeight - 8 ? Math.max(8, r.top - h - 6) : r.bottom + 6) + 'px';
    pop.addEventListener('click', function (e) {
      var nav = e.target.closest('[data-nav]');
      if (nav) {
        viewMonth = new Date(viewMonth.getFullYear(), viewMonth.getMonth() + Number(nav.dataset.nav), 1);
        render();
        return;
      }
      var act = e.target.closest('[data-act]');
      if (!act) return;
      var a = act.dataset.act;
      if (a === 'cancel') { closeDT(); return; }
      if (a === 'today') {
        var t = new Date();
        sel = new Date(t.getFullYear(), t.getMonth(), t.getDate(), hours, minutes);
        viewMonth = new Date(sel.getFullYear(), sel.getMonth(), 1);
        render(); return;
      }
      if (a === 'now') {
        var n = new Date();
        hours = n.getHours(); minutes = n.getMinutes();
        sel = new Date(n.getFullYear(), n.getMonth(), n.getDate(), hours, minutes);
        viewMonth = new Date(sel.getFullYear(), sel.getMonth(), 1);
        render(); return;
      }
      if (a === 'ok') {
        var out = new Date(sel.getFullYear(), sel.getMonth(), sel.getDate(), hours, minutes);
        input.value = dtValue(out);
        input.dispatchEvent(new Event('input', { bubbles: true }));
        input.dispatchEvent(new Event('change', { bubbles: true }));
        closeDT();
      }
    });
    activeDT = { input: input, pop: pop };
    document.addEventListener('keydown', onDTKey, true);
    document.addEventListener('pointerdown', onDTAway, true);
    window.addEventListener('resize', closeDT, true);
    window.addEventListener('scroll', closeDT, true);
    var firstBtn = pop.querySelector('.dt-day.sel') || pop.querySelector('.dt-day');
    if (firstBtn) firstBtn.focus({ preventScroll: true });
  }

  function upgradeDateTimes(root) {
    var list = root && root.querySelectorAll ? root.querySelectorAll('input[type=datetime-local]:not([data-dt])') : [];
    for (var i = 0; i < list.length; i++) {
      (function (inp) {
        inp.setAttribute('data-dt', '1');
        inp.addEventListener('pointerdown', function (e) { e.preventDefault(); openDT(inp); });
        inp.addEventListener('keydown', function (e) {
          if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); openDT(inp); }
        });
      })(list[i]);
    }
  }
  var dtPending = false;
  function scheduleDTUpgrade() {
    if (dtPending) return;
    dtPending = true;
    setTimeout(function () { dtPending = false; upgradeDateTimes(document); }, 60);
  }

  /* ══ 表单可读名：把 .lb 文本补成 aria-label ═════════════════════════
     配置页用 <label class="fld"> 包裹整块（含 .hint 说明），读屏会把说明也念进
     控件名称里（「监听地址 改动需重启进程」）。aria-label 只取 .lb 一行。 */
  function labelFields(root) {
    var labs = root && root.querySelectorAll ? root.querySelectorAll('label.fld, label.switch') : [];
    for (var i = 0; i < labs.length; i++) {
      var lb = labs[i].querySelector('.lb');
      if (!lb) continue;
      var text = (lb.textContent || '').trim();
      if (!text) continue;
      var fields = labs[i].querySelectorAll('input, select, textarea');
      for (var j = 0; j < fields.length; j++) {
        if (!fields[j].getAttribute('aria-label')) fields[j].setAttribute('aria-label', text);
      }
    }
  }

  /* ══ 确认框 ════════════════════════════════════════════════════════ */
  function confirmBox(opts) {
    opts = opts || {};
    return new Promise(function (resolve) {
      var veil = document.createElement('div');
      veil.className = 'veil on';
      veil.innerHTML =
        '<div class="dlg sm" role="dialog" aria-modal="true" aria-labelledby="wbConfTitle">' +
        '<header><h3 id="wbConfTitle"></h3></header>' +
        '<div class="body"><p class="conf-body"></p></div>' +
        '<footer class="conf-foot"><span class="grow"></span>' +
        '<button type="button" class="cancel"></button>' +
        '<button type="button" class="ok primary"></button></footer></div>';
      veil.querySelector('#wbConfTitle').textContent = opts.title || '确认操作';
      veil.querySelector('.conf-body').textContent = opts.body || '';
      veil.querySelector('.cancel').textContent = opts.cancelText || '取消';
      var ok = veil.querySelector('.ok');
      ok.textContent = opts.confirmText || '确认';
      if (opts.danger) {
        ok.classList.remove('primary');
        ok.classList.add('btn-danger');
      }
      document.body.appendChild(veil);
      document.documentElement.classList.add('lock');
      var prev = document.activeElement;
      var cancelBtn = veil.querySelector('.cancel');
      setTimeout(function () { cancelBtn.focus(); }, 0);

      function done(v) {
        document.removeEventListener('keydown', onKey, true);
        document.documentElement.classList.remove('lock');
        veil.remove();
        if (prev && prev.focus) { try { prev.focus({ preventScroll: true }); } catch (e) { prev.focus(); } }
        resolve(v);
      }
      function onKey(e) {
        if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); done(false); return; }
        if (e.key === 'Tab') {
          var a = cancelBtn, b = ok;
          if (e.shiftKey && document.activeElement === a) { e.preventDefault(); b.focus(); }
          else if (!e.shiftKey && document.activeElement === b) { e.preventDefault(); a.focus(); }
        }
      }
      cancelBtn.addEventListener('click', function () { done(false); });
      ok.addEventListener('click', function () { done(true); });
      veil.addEventListener('mousedown', function (e) { if (e.target === veil) done(false); });
      document.addEventListener('keydown', onKey, true);
    });
  }

  window.WBControls = { confirm: confirmBox, close: closePop, menu: openMenu };
})();
