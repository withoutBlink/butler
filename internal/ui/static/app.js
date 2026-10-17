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

  // Read marks: data-agents lists the session's agent participants and
  // data-reads maps agent id -> highest seq that agent has received (the
  // server records a read whenever it delivers messages to an agent).
  // SSE "reads" events advance the map so marks update live.
  var readBy = {};
  var readAgents = [];
  try {
    readBy = JSON.parse(box.getAttribute('data-reads') || '{}') || {};
    readAgents = JSON.parse(box.getAttribute('data-agents') || '[]') || [];
  } catch (err) { /* fall back to server-rendered marks */ }

  function updateReadMarks() {
    var rows = box.querySelectorAll('.msg.chat');
    for (var i = 0; i < rows.length; i++) {
      var row = rows[i];
      var seq = parseInt(row.getAttribute('data-seq'), 10) || 0;
      var sender = row.getAttribute('data-sender');
      var readers = [];
      var total = 0;
      for (var j = 0; j < readAgents.length; j++) {
        var a = readAgents[j];
        if (a === sender) continue;
        total++;
        if ((readBy[a] || 0) >= seq) readers.push(a);
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
      span.title = readers.length ? 'read by ' + readers.join(', ') : 'not read yet';
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
