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
    box.scrollTop = box.scrollHeight;
  }

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
      fetch(form.action, { method: 'POST', body: new FormData(form), credentials: 'same-origin' })
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
  }
})();
