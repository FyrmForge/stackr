// codearea: syntax-highlighted textareas via a transparent-text overlay.
// Usage: <textarea data-code="env|yaml">. The textarea keeps focus/input/
// submit behavior; a <pre> behind it paints the same text tokenized. Both
// share identical font metrics so glyphs line up exactly.
(function () {
  'use strict';

  var esc = function (s) {
    return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
  };

  // KEY=VALUE per line; quoted values may span lines (see internal/envutil).
  function highlightEnv(text) {
    var out = [];
    var warns = [];
    var seen = {};
    var lines = text.split('\n');
    for (var i = 0; i < lines.length; i++) {
      var line = lines[i];
      var t = line.trim();
      if (t === '' ) { out.push(''); continue; }
      if (t.charAt(0) === '#') { out.push('<span class="tok-c">' + esc(line) + '</span>'); continue; }
      var eq = line.indexOf('=');
      if (eq === -1) {
        out.push('<span class="tok-bad">' + esc(line) + '</span>');
        warns.push('line ' + (i + 1) + ': missing "="');
        continue;
      }
      var key = line.slice(0, eq);
      var val = line.slice(eq + 1);
      var ktrim = key.trim();
      if (seen[ktrim]) warns.push('line ' + (i + 1) + ': duplicate key ' + ktrim);
      seen[ktrim] = true;
      var q = val.trim().charAt(0);
      if (q === '"' || q === "'") {
        // consume until the closing quote (may span lines)
        var chunk = [val];
        var rest = val.trim().slice(1);
        while (rest.indexOf(q) === -1 && i + 1 < lines.length) {
          i++;
          chunk.push(lines[i]);
          rest = lines[i];
        }
        out.push('<span class="tok-k">' + esc(key) + '</span><span class="tok-eq">=</span><span class="tok-s">' + esc(chunk.join('\n')) + '</span>');
        continue;
      }
      out.push('<span class="tok-k">' + esc(key) + '</span><span class="tok-eq">=</span><span class="tok-v">' + esc(val) + '</span>');
    }
    return { html: out.join('\n'), warns: warns };
  }

  // Light YAML tinting: comments, "key:", quoted strings. Not a parser.
  function highlightYaml(text) {
    var out = text.split('\n').map(function (line) {
      if (line.trim().charAt(0) === '#') return '<span class="tok-c">' + esc(line) + '</span>';
      return esc(line)
        .replace(/^(\s*(?:- )?)([\w.\/-]+)(:)(\s|$)/, '$1<span class="tok-k">$2</span><span class="tok-eq">$3</span>$4')
        .replace(/(&quot;[^&]*&quot;|'[^']*')/g, '<span class="tok-s">$1</span>');
    });
    return { html: out.join('\n'), warns: [] };
  }

  var modes = { env: highlightEnv, yaml: highlightYaml };

  function enhance(ta) {
    if (ta.__codearea) return;
    ta.__codearea = true;
    var mode = modes[ta.dataset.code] || highlightEnv;

    var wrap = document.createElement('div');
    wrap.className = 'codearea';
    ta.parentNode.insertBefore(wrap, ta);

    var pre = document.createElement('pre');
    pre.className = 'codearea-hl';
    pre.setAttribute('aria-hidden', 'true');
    wrap.appendChild(pre);
    wrap.appendChild(ta);
    ta.classList.add('codearea-input');

    var warn = document.createElement('div');
    warn.className = 'codearea-warn';
    wrap.after(warn);

    function paint() {
      var r = mode(ta.value);
      // trailing newline keeps the pre's last line height in sync
      pre.innerHTML = r.html + '\n';
      warn.textContent = r.warns.join(' · ');
      warn.style.display = r.warns.length ? '' : 'none';
    }
    function sync() {
      pre.scrollTop = ta.scrollTop;
      pre.scrollLeft = ta.scrollLeft;
    }
    ta.addEventListener('input', paint);
    ta.addEventListener('scroll', sync);
    paint();
  }

  // Reference autocomplete: typing "${{" in a textarea carrying data-refs opens
  // a listbox of what this tile may reference. The catalogue is metadata only,
  // names, never values, so it is safe to render inline.
  function autocomplete(ta) {
    var raw = ta.getAttribute('data-refs');
    if (!raw) return;
    var items;
    try { items = JSON.parse(raw); } catch (e) { return; }
    if (!items || !items.length) return;

    var box = document.createElement('ul');
    box.className = 'codearea-ac';
    box.setAttribute('role', 'listbox');
    box.hidden = true;
    ta.setAttribute('role', 'combobox');
    ta.setAttribute('aria-expanded', 'false');
    ta.setAttribute('aria-autocomplete', 'list');
    ta.after(box);

    var shown = [];
    var active = -1;

    function close() {
      box.hidden = true;
      ta.setAttribute('aria-expanded', 'false');
      ta.removeAttribute('aria-activedescendant');
      active = -1;
    }

    // trigger returns the partial reference being typed, or null.
    function trigger() {
      var upto = ta.value.slice(0, ta.selectionStart);
      var open = upto.lastIndexOf('${{');
      if (open === -1) return null;
      var tail = upto.slice(open);
      if (tail.indexOf('}}') !== -1 || tail.indexOf('\n') !== -1) return null;
      return { start: open, query: tail.slice(3).trim().toLowerCase() };
    }

    function render() {
      var t = trigger();
      if (!t) { close(); return; }
      shown = items.filter(function (it) {
        return !t.query || it.expr.toLowerCase().indexOf(t.query) !== -1 || it.label.toLowerCase().indexOf(t.query) !== -1;
      }).slice(0, 8);
      if (!shown.length) { close(); return; }
      box.innerHTML = '';
      shown.forEach(function (it, i) {
        var li = document.createElement('li');
        li.id = 'ac-' + i;
        li.setAttribute('role', 'option');
        li.setAttribute('aria-selected', 'false');
        li.className = 'codearea-ac-item';
        li.textContent = it.label + (it.secret ? ' (secret)' : '');
        li.addEventListener('mousedown', function (e) { e.preventDefault(); choose(i); });
        box.appendChild(li);
      });
      active = 0;
      mark();
      box.hidden = false;
      ta.setAttribute('aria-expanded', 'true');
    }

    function mark() {
      Array.prototype.forEach.call(box.children, function (li, i) {
        li.setAttribute('aria-selected', i === active ? 'true' : 'false');
        li.classList.toggle('is-active', i === active);
      });
      if (active >= 0) ta.setAttribute('aria-activedescendant', 'ac-' + active);
    }

    function choose(i) {
      var t = trigger();
      if (!t || !shown[i]) return;
      var before = ta.value.slice(0, t.start);
      var after = ta.value.slice(ta.selectionStart);
      ta.value = before + shown[i].expr + after;
      var pos = before.length + shown[i].expr.length;
      ta.setSelectionRange(pos, pos);
      close();
      ta.dispatchEvent(new Event('input'));
    }

    ta.addEventListener('input', render);
    ta.addEventListener('blur', close);
    ta.addEventListener('keydown', function (e) {
      if (box.hidden) return;
      if (e.key === 'ArrowDown') { e.preventDefault(); active = (active + 1) % shown.length; mark(); }
      else if (e.key === 'ArrowUp') { e.preventDefault(); active = (active - 1 + shown.length) % shown.length; mark(); }
      else if (e.key === 'Enter' || e.key === 'Tab') { e.preventDefault(); choose(active); }
      else if (e.key === 'Escape') { close(); }
    });
  }

  function scan(root) {
    (root.querySelectorAll ? root : document)
      .querySelectorAll('textarea[data-code]')
      .forEach(function (ta) { enhance(ta); autocomplete(ta); });
  }

  document.addEventListener('DOMContentLoaded', function () { scan(document); });
  document.addEventListener('htmx:afterSwap', function (e) { scan(e.target); });
})();
