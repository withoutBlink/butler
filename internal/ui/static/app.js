(function () {
  var box = document.getElementById('messages');
  if (!box) return;
  var streamPath = box.getAttribute('data-stream');
  var es = null;

  function lastSeq() {
    var rows = box.querySelectorAll('.msg');
    if (!rows.length) return 0;
    return parseInt(rows[rows.length - 1].getAttribute('data-seq'), 10) || 0;
  }

  function append(html) {
    var tmp = document.createElement('div');
    tmp.innerHTML = html;
    var el = tmp.firstElementChild;
    if (!el) return;
    if (box.querySelector('[data-seq="' + el.getAttribute('data-seq') + '"]')) return;
    box.appendChild(el);
    markClamp(el);
    box.scrollTop = box.scrollHeight;
  }

  // Long messages clamp to a few lines (max-height in CSS); clicking one
  // toggles between clamped and expanded.
  function markClamp(scope) {
    var els = (scope || box).querySelectorAll('.m-content');
    for (var i = 0; i < els.length; i++) {
      var el = els[i];
      if (el.classList.contains('expanded')) continue;
      if (el.scrollHeight > el.clientHeight + 2) {
        el.classList.add('clamped');
        el.title = 'click to expand';
      }
    }
  }

  box.addEventListener('click', function (ev) {
    var el = ev.target.closest('.m-content');
    if (!el || !el.classList.contains('clamped')) return;
    var expanded = el.classList.toggle('expanded');
    if (expanded) {
      el.title = 'click to collapse';
    } else {
      el.title = 'click to expand';
    }
    box.scrollTop = box.scrollHeight;
  });
  markClamp();

  // Read marks: data-audience lists the session's participants as the marks
  // need them ({id,name,since,human}) and data-reads maps participant id ->
  // highest seq that participant has received (the server records a read
  // whenever it delivers messages to an agent). The human (human:true) is
  // always a reader: a mark is only ever visible to the human, so seeing it
  // means having seen the message, and nothing delivers messages to a human to
  // record a cursor for. A member whose membership starts after a message was
  // sent (its join notice's seq is past it) is not part of that message's
  // audience. SSE "reads"/"audience" events keep both current.
  var readBy = {};
  var audience = [];
  try {
    readBy = JSON.parse(box.getAttribute('data-reads') || '{}') || {};
    audience = JSON.parse(box.getAttribute('data-audience') || '[]') || [];
  } catch (err) { /* fall back to server-rendered marks */ }

  function readTip(readers, unread) {
    if (readers.length && unread.length) return 'read by ' + readers.join(', ') + ' \u00b7 not yet: ' + unread.join(', ');
    if (readers.length) return 'read by ' + readers.join(', ');
    if (unread.length) return 'not read yet: ' + unread.join(', ');
    return '';
  }

  function updateReadMarks() {
    var rows = box.querySelectorAll('.msg.chat');
    for (var i = 0; i < rows.length; i++) {
      var row = rows[i];
      var seq = parseInt(row.getAttribute('data-seq'), 10) || 0;
      var sender = row.getAttribute('data-sender');
      var readers = [];
      var unread = [];
      var total = 0;
      for (var j = 0; j < audience.length; j++) {
        var m = audience[j];
        if (!m || m.id === sender) continue;
        if ((m.since || 0) > seq) continue;
        total++;
        var name = m.name || m.id;
        if (m.human || (readBy[m.id] || 0) >= seq) readers.push(name);
        else unread.push(name);
      }
      if (!total) continue;
      var span = row.querySelector('.m-read');
      if (!span) {
        span = document.createElement('span');
        span.className = 'm-read';
        var meta = row.querySelector('.m-meta');
        if (meta) meta.insertAdjacentElement('afterend', span);
      }
      span.textContent = '✓' + readers.length + '/' + total;
      span.title = readTip(readers, unread);
      span.classList.toggle('all', readers.length === total);
    }
  }
  updateReadMarks();

  function connect() {
    var path = streamPath;
    var seq = lastSeq();
    if (seq > 0) path = path.replace(/([?&])after=\d+/, '$1after=' + seq);
    es = new EventSource(path);
    es.addEventListener('message', function (e) { append(e.data); });
    es.addEventListener('presence', function (e) {
      var d = document.createElement('div');
      d.innerHTML = e.data;
      var np = d.querySelector('#participants');
      if (np) {
        var cur = document.getElementById('participants');
        if (cur) cur.replaceWith(np);
      }
    });
    es.addEventListener('reads', function (e) {
      try {
        var d = JSON.parse(e.data);
        if (d && d.agent) readBy[d.agent] = d.seq;
      } catch (err) { /* ignore malformed events */ }
      updateReadMarks();
    });
    es.addEventListener('audience', function (e) {
      // Membership changed (join/removal): the denominator and the tooltips
      // follow within the same heartbeat as the presence re-render.
      try {
        var d = JSON.parse(e.data);
        if (Array.isArray(d)) audience = d;
      } catch (err) { /* ignore malformed events */ }
      updateReadMarks();
    });
    es.onerror = function () {
      es.close();
      setTimeout(connect, 1000);
    };
  }
  connect();

  var form = document.getElementById('compose');
  if (form) {
    form.addEventListener('submit', function (ev) {
      ev.preventDefault();
      var input = form.querySelector('textarea[name=content]');
      var content = (input.value || '').trim();
      if (!content) return;
      fetch(form.action, { method: 'POST', body: new URLSearchParams({ content: content }), credentials: 'same-origin' })
        .then(function (res) {
          if (!res.ok) throw new Error('send failed: ' + res.status);
          return res.text();
        })
        .then(function (html) {
          if (html) append(html);
          input.value = '';
        })
        .catch(function (err) { alert(err.message); });
    });

    setupMentions(form);
  }

  // @mention autocomplete: typing "@" (or "@par") in the composer shows the
  // session's other participants by display title plus "all"; picking one
  // completes the mention with the title. The server resolves both titles and
  // ids, so either form addresses an agent that must reply.
  function setupMentions(form) {
    var input = form.querySelector('textarea[name=content]');
    if (!input) return;
    var targets = [];
    try {
      targets = JSON.parse(input.getAttribute('data-mentionables') || '[]');
    } catch (err) { /* fall back to no autocomplete */ }
    if (!targets.length) return;

    var menu = null, items = [], active = -1;

    function closeMenu() {
      if (menu) { menu.remove(); menu = null; }
      items = [];
      active = -1;
    }

    function currentToken() {
      var upto = input.value.slice(0, input.selectionStart);
      var m = upto.match(/(^|\s)@([A-Za-z0-9_:-]*)$/);
      if (!m) return null;
      return { text: m[2], at: upto.length - m[2].length - 1 };
    }

    function matches(q) {
      q = q.toLowerCase();
      return targets.filter(function (t) {
        return t.toLowerCase().indexOf(q) !== -1;
      }).slice(0, 8);
    }

    function highlight() {
      for (var i = 0; i < items.length; i++) {
        items[i].className = 'mention-item' + (i === active ? ' active' : '');
      }
    }

    function openMenu(token) {
      var found = matches(token.text);
      if (!found.length) { closeMenu(); return; }
      closeMenu();
      menu = document.createElement('div');
      menu.className = 'mention-menu';
      found.forEach(function (t, i) {
        var it = document.createElement('div');
        it.className = 'mention-item' + (i === 0 ? ' active' : '');
        it.textContent = '@' + t;
        it.addEventListener('mousedown', function (ev) {
          ev.preventDefault(); // keep textarea focus
          pick(t);
        });
        menu.appendChild(it);
      });
      form.appendChild(menu);
      items = Array.prototype.slice.call(menu.children);
      active = 0;
    }

    function pick(target) {
      var tok = currentToken();
      if (!tok) { closeMenu(); return; }
      var v = input.value, ss = input.selectionStart;
      input.value = v.slice(0, tok.at) + '@' + target + ' ' + v.slice(ss);
      var pos = tok.at + target.length + 2;
      input.focus();
      input.setSelectionRange(pos, pos);
      closeMenu();
    }

    input.addEventListener('input', function () {
      var tok = currentToken();
      if (tok) openMenu(tok); else closeMenu();
    });

    input.addEventListener('keydown', function (ev) {
      if (!menu) return;
      if (ev.key === 'ArrowDown') {
        ev.preventDefault();
        active = (active + 1) % items.length;
        highlight();
      } else if (ev.key === 'ArrowUp') {
        ev.preventDefault();
        active = (active - 1 + items.length) % items.length;
        highlight();
      } else if (ev.key === 'Enter' || ev.key === 'Tab') {
        ev.preventDefault();
        pick(items[active] ? items[active].textContent.slice(1) : '');
      } else if (ev.key === 'Escape') {
        closeMenu();
      }
    });

    input.addEventListener('blur', function () {
      // Delay so a click on a menu item lands before the menu disappears.
      setTimeout(closeMenu, 150);
    });
  }
})();

// Task board live updates. The columns are replaced on every SSE push,
// including while a log is open: gating on :focus used to freeze the board
// for as long as the summary or a field inside the card stayed focused
// (opening Log focuses its summary), so new tasks never appeared until a
// reload. Open panels and any text the user is mid-typing are copied across
// the swap. The Done column's collapsed state lives in localStorage.
(function () {
  var board = document.getElementById('board');
  if (!board) return;
  var doneKey = 'butler.board.doneCollapsed';

  function applyDone() {
    var col = board.querySelector('.col[data-status="done"]');
    if (!col) return;
    var collapsed = localStorage.getItem(doneKey) === '1';
    col.classList.toggle('collapsed', collapsed);
    var btn = col.querySelector('.col-toggle');
    if (btn) {
      btn.textContent = collapsed ? 'Expand' : 'Collapse';
      btn.setAttribute('aria-label', collapsed ? 'Expand the Done column' : 'Collapse the Done column');
    }
  }

  function capture() {
    var open = [];
    var nodes = board.querySelectorAll('details[open]');
    for (var i = 0; i < nodes.length; i++) {
      var card = nodes[i].closest('[data-task]');
      var panel = nodes[i].getAttribute('data-panel') || '';
      if (card && panel) open.push(card.getAttribute('data-task') + '\n' + panel);
    }
    var focus = null;
    var active = document.activeElement;
    if (active && board.contains(active) && active.name) {
      var host = active.closest('[data-task]');
      var box = active.closest('details');
      focus = {
        task: host ? host.getAttribute('data-task') : '',
        panel: box ? (box.getAttribute('data-panel') || '') : '',
        name: active.name,
        value: 'value' in active ? active.value : null,
        start: active.selectionStart,
        end: active.selectionEnd
      };
    }
    return { open: open, focus: focus };
  }

  function cardOf(task) {
    if (!task || !window.CSS || !CSS.escape) {
      var all = board.querySelectorAll('[data-task]');
      for (var i = 0; i < all.length; i++) {
        if (all[i].getAttribute('data-task') === task) return all[i];
      }
      return null;
    }
    return board.querySelector('[data-task="' + CSS.escape(task) + '"]');
  }

  function restore(state) {
    for (var i = 0; i < state.open.length; i++) {
      var parts = state.open[i].split('\n');
      var card = cardOf(parts[0]);
      if (!card) continue;
      var d = card.querySelector('details[data-panel="' + parts[1] + '"]');
      if (d) d.open = true;
    }
    applyDone();
    var f = state.focus;
    if (!f || !f.name) return;
    var card = cardOf(f.task);
    if (!card) return;
    var scope = card;
    if (f.panel) {
      var panel = card.querySelector('details[data-panel="' + f.panel + '"]');
      if (panel) scope = panel;
    }
    var el = scope.querySelector('[name="' + f.name + '"]');
    if (!el) return;
    if (f.value != null && 'value' in el && (el.tagName === 'TEXTAREA' || el.tagName === 'INPUT')) {
      el.value = f.value;
    }
    el.focus();
    if (el.setSelectionRange && typeof f.start === 'number') {
      try { el.setSelectionRange(f.start, f.end); } catch (e) {}
    }
  }

  function apply(html) {
    var state = capture();
    board.innerHTML = html;
    restore(state);
  }

  applyDone();
  board.addEventListener('click', function (ev) {
    var btn = ev.target.closest('.col-toggle');
    if (!btn || !board.contains(btn)) return;
    var collapsed = localStorage.getItem(doneKey) === '1';
    localStorage.setItem(doneKey, collapsed ? '0' : '1');
    applyDone();
  });

  var path = board.getAttribute('data-stream');
  if (!path || !window.EventSource) return;
  var es = new EventSource(path);
  es.addEventListener('board', function (ev) { apply(ev.data); });
})();
