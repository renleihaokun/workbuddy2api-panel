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
