// Live log viewer: attaches to [data-logs-stream] containers rendered by
// components.LogView. Lines arrive over SSE as "O <ts> <msg>" / "E <ts> <msg>"
// (stderr), optionally prefixed "tile|" on merged environment streams.
//
// Railway-style ordered rows: timestamp | level-coloured edge | message.
// JSON lines are parsed, level and message lift out, the remaining fields
// render as a dim key=value preview and a click expands the full set.
(function () {
    var MAX_LINES = 3000;
    var PREFS_KEY = 'stackr.logview';
    // Dozzle-style coloured pills for the tile column on merged streams.
    // The hues are the env palette tokens, which are tuned per theme; raw
    // Tailwind -300 shades were dark-theme-only and measured 1.33:1 on the
    // light background.
    var TILE_HUES = ['teal', 'sky', 'lime', 'amber', 'rose', 'violet'];

    // Ranks: debug 0, info 1, warn 2, error 3. Lines with no detectable level
    // rank as info so they survive the default filter but hide under warn+.
    var RANK_STYLE = {
        3: { edge: 'border-rw-danger', line: 'text-rw-danger', tok: 'text-rw-danger font-semibold' },
        2: { edge: 'border-rw-warn', line: 'text-rw-warn', tok: 'text-rw-warn font-semibold' },
        1: { edge: 'border-transparent', line: '', tok: 'text-rw-accent font-semibold' },
        0: { edge: 'border-transparent', line: 'text-rw-faint', tok: 'text-rw-faint font-semibold' },
    };

    // Level detection in plain text: the matched token gets its own bright
    // colour; the rest of the line only tints for warnings/errors.
    var LEVELS = [
        { re: /\b(FATAL|PANIC|EXCEPTION)\b/i, rank: 3 },
        { re: /\b(ERROR|ERR)\b/i, rank: 3 },
        { re: /\b(WARN|WARNING)\b/i, rank: 2 },
        { re: /\bINFO\b/i, rank: 1 },
        { re: /\b(DEBUG|TRACE)\b/i, rank: 0 },
    ];

    var LEVEL_NAMES = {
        fatal: 3, panic: 3, error: 3, err: 3,
        warn: 2, warning: 2,
        info: 1,
        debug: 0, trace: 0,
    };

    function loadPrefs() {
        try { return JSON.parse(localStorage.getItem(PREFS_KEY)) || {}; } catch (e) { return {}; }
    }
    function savePrefs(p) {
        try { localStorage.setItem(PREFS_KEY, JSON.stringify(p)); } catch (e) { /* private mode */ }
    }

    // parseStructured pulls a JSON object out of a log line ("{...}" itself,
    // or after a text prefix). Returns {rank, msg, attrs, prefix} or null.
    function parseStructured(msg) {
        var start = msg.indexOf('{');
        if (start < 0 || msg[msg.length - 1] !== '}') return null;
        var obj;
        try { obj = JSON.parse(msg.slice(start)); } catch (e) { return null; }
        if (!obj || typeof obj !== 'object' || Array.isArray(obj)) return null;
        var rank = null, text = null, attrs = [];
        for (var k in obj) {
            var v = obj[k];
            var lk = k.toLowerCase();
            if (rank === null && (lk === 'level' || lk === 'lvl' || lk === 'severity') && typeof v === 'string') {
                rank = LEVEL_NAMES[v.toLowerCase()];
                if (rank === undefined) rank = null; else continue;
            }
            if (text === null && (lk === 'msg' || lk === 'message') && typeof v === 'string') {
                text = v;
                continue;
            }
            attrs.push([k, typeof v === 'string' ? v : JSON.stringify(v)]);
        }
        return {
            rank: rank === null ? 1 : rank,
            msg: text === null ? '' : text,
            attrs: attrs,
            prefix: msg.slice(0, start).trim(),
        };
    }

    // renderPlain fills msgEl with a plain-text message, colouring the level
    // token. Returns the line's rank. Deliberately ignores stdout/stderr: many
    // servers (postgres, nginx) log everything to stderr, so tinting by stream
    // painted whole logs red.
    function renderPlain(msgEl, mark, msg) {
        var rank = 1, m = null;
        for (var i = 0; i < LEVELS.length; i++) {
            m = msg.match(LEVELS[i].re);
            if (m) { rank = LEVELS[i].rank; break; }
        }
        var st = RANK_STYLE[rank];
        msgEl.className = st.line || 'text-rw-muted';
        if (!m) {
            msgEl.textContent = msg;
            return rank;
        }
        msgEl.appendChild(document.createTextNode(msg.slice(0, m.index)));
        var tok = document.createElement('span');
        tok.className = st.tok;
        tok.textContent = m[0];
        msgEl.appendChild(tok);
        msgEl.appendChild(document.createTextNode(msg.slice(m.index + m[0].length)));
        return rank;
    }

    function tileHue(name) {
        var h = 0;
        for (var i = 0; i < name.length; i++) h = (h * 31 + name.charCodeAt(i)) >>> 0;
        return 'var(--rw-env-' + TILE_HUES[h % TILE_HUES.length] + ')';
    }

    function init(root) {
        if (root.dataset.logsInit) return;
        root.dataset.logsInit = '1';
        var linesEl = root.querySelector('[data-logs-lines]');
        var search = root.querySelector('[data-logs-search]');
        var tsToggle = root.querySelector('[data-logs-ts]');
        var follow = root.querySelector('[data-logs-follow]');
        var levelSel = root.querySelector('[data-logs-level]');
        var wrapToggle = root.querySelector('[data-logs-wrap]');
        var status = root.querySelector('[data-logs-status]');
        var multi = root.hasAttribute('data-logs-multi');
        var filter = '';
        var minRank = -1;

        var prefs = loadPrefs();
        if (prefs.ts !== undefined) tsToggle.checked = prefs.ts;
        if (prefs.wrap !== undefined) wrapToggle.checked = prefs.wrap;
        if (prefs.level !== undefined) levelSel.value = String(prefs.level);
        minRank = parseInt(levelSel.value, 10);

        function persist() {
            savePrefs({ ts: tsToggle.checked, wrap: wrapToggle.checked, level: minRank });
        }

        function applyFilter(el) {
            var show = (!filter || (el.dataset.raw || '').includes(filter)) &&
                parseInt(el.dataset.rank || '1', 10) >= minRank;
            el.style.display = show ? '' : 'none';
        }
        function refilter() {
            linesEl.querySelectorAll('.log-line').forEach(applyFilter);
        }

        search.addEventListener('input', function () {
            filter = search.value.toLowerCase();
            refilter();
        });
        levelSel.addEventListener('change', function () {
            minRank = parseInt(levelSel.value, 10);
            persist();
            refilter();
        });
        tsToggle.addEventListener('change', function () {
            linesEl.classList.toggle('logs-show-ts', tsToggle.checked);
            persist();
        });
        function applyWrap() {
            // wrapped: long lines fold; unwrapped: one line each, scroll sideways
            linesEl.classList.toggle('whitespace-pre-wrap', wrapToggle.checked);
            linesEl.classList.toggle('break-all', wrapToggle.checked);
            linesEl.classList.toggle('whitespace-pre', !wrapToggle.checked);
            linesEl.classList.toggle('overflow-x-auto', !wrapToggle.checked);
            linesEl.classList.toggle('overflow-x-hidden', wrapToggle.checked);
        }
        wrapToggle.addEventListener('change', function () { applyWrap(); persist(); });
        linesEl.classList.toggle('logs-show-ts', tsToggle.checked);
        applyWrap();

        // Scrolling up pauses follow; scrolling back to the bottom resumes it.
        linesEl.addEventListener('scroll', function () {
            var atBottom = linesEl.scrollTop + linesEl.clientHeight >= linesEl.scrollHeight - 8;
            if (!atBottom && follow.checked) follow.checked = false;
            else if (atBottom && !follow.checked) follow.checked = true;
        });

        // Click a structured row to unfold its full attribute set.
        linesEl.addEventListener('click', function (e) {
            var line = e.target.closest('.log-line');
            if (!line || !line.dataset.expandable) return;
            var attrs = line.querySelector('.log-attrs');
            if (attrs) attrs.classList.toggle('hidden');
        });

        function addLine(raw, plain) {
            var tile = '';
            if (multi) {
                var bar = raw.indexOf('|');
                if (bar > 0) { tile = raw.slice(0, bar); raw = raw.slice(bar + 1); }
            }
            raw = raw.replace(/\x1b\[[0-9;]*[A-Za-z]/g, ''); // strip ANSI colour codes
            // Stored output (a finished cron run) arrives unframed: no stream
            // marker, no timestamp, just the line as the container printed it.
            var mark = 'O', ts = '', msg = raw;
            if (!plain) {
                mark = raw.slice(0, 1);
                var rest = raw.slice(2);
                var sp = rest.indexOf(' ');
                ts = sp > 0 ? rest.slice(0, sp) : '';
                msg = sp > 0 ? rest.slice(sp + 1) : rest;
            }

            var div = document.createElement('div');
            div.className = 'log-line border-l-2 pl-2 -ml-1';
            div.dataset.raw = (tile + ' ' + msg).toLowerCase();
            if (tile) {
                var tag = document.createElement('span');
                tag.className = 'log-chip mr-2 align-middle';
                tag.style.setProperty('--env-c', tileHue(tile));
                tag.textContent = tile;
                div.appendChild(tag);
            }
            if (ts) {
                var tsEl = document.createElement('span');
                tsEl.className = 'log-ts text-rw-faint mr-2';
                tsEl.textContent = ts.replace(/\.\d+Z?$/, 'Z').replace('T', ' ');
                div.appendChild(tsEl);
            }

            var msgEl = document.createElement('span');
            var rank;
            var js = parseStructured(msg);
            if (js) {
                rank = js.rank;
                var st = RANK_STYLE[rank];
                msgEl.className = st.line || 'text-rw-text';
                msgEl.textContent = (js.prefix ? js.prefix + ' ' : '') + js.msg;
                div.appendChild(msgEl);
                if (js.attrs.length > 0) {
                    var preview = document.createElement('span');
                    preview.className = 'text-rw-faint ml-2';
                    preview.textContent = js.attrs.map(function (kv) {
                        var v = kv[1].length > 60 ? kv[1].slice(0, 60) + '…' : kv[1];
                        return kv[0] + '=' + v;
                    }).join(' ');
                    div.appendChild(preview);

                    var attrsEl = document.createElement('div');
                    attrsEl.className = 'log-attrs hidden pl-6 py-1 space-y-0.5';
                    js.attrs.forEach(function (kv) {
                        var row = document.createElement('div');
                        var k = document.createElement('span');
                        k.className = 'text-rw-accentHi';
                        k.textContent = kv[0];
                        var v = document.createElement('span');
                        v.className = 'text-rw-muted';
                        v.textContent = ' ' + kv[1];
                        row.appendChild(k);
                        row.appendChild(v);
                        attrsEl.appendChild(row);
                    });
                    div.appendChild(attrsEl);
                    div.dataset.expandable = '1';
                    div.classList.add('cursor-pointer');
                }
            } else {
                rank = renderPlain(msgEl, mark, msg);
                div.appendChild(msgEl);
            }
            div.dataset.rank = String(rank);
            div.classList.add(RANK_STYLE[rank].edge);
            applyFilter(div);
            linesEl.appendChild(div);
            while (linesEl.childElementCount > MAX_LINES) linesEl.removeChild(linesEl.firstElementChild);
            if (follow.checked) linesEl.scrollTop = linesEl.scrollHeight;
        }

        // Stored output: seed from what the server rendered into the lines
        // element, then stop, there is nothing to follow.
        if (root.hasAttribute('data-logs-text')) {
            var stored = linesEl.textContent;
            linesEl.textContent = '';
            stored.split('\n').forEach(function (l) { addLine(l, true); });
            status.textContent = linesEl.childElementCount + ' lines';
            return;
        }

        var closed = false;
        var es = new EventSource(root.dataset.logsStream);
        es.onopen = function () {
            // Reconnects replay the tail, start clean to avoid duplicates.
            linesEl.textContent = '';
            status.textContent = 'live';
        };
        es.onerror = function () { if (!closed) status.textContent = 'reconnecting…'; };
        es.onmessage = function (evt) { addLine(evt.data); };
        // Some streams end: a finished deployment, a cron's run history with
        // nothing in flight. The server says so with a "done" event, without
        // closing on it EventSource reconnects and replays forever.
        es.addEventListener('done', function () {
            closed = true;
            es.close();
            status.textContent = linesEl.childElementCount + ' lines';
        });
        // Tear the stream down when the panel content is swapped away.
        var obs = new MutationObserver(function () {
            if (!document.body.contains(root)) { es.close(); obs.disconnect(); }
        });
        obs.observe(document.body, { childList: true, subtree: true });
    }

    function scan() { document.querySelectorAll('[data-logs-stream],[data-logs-text]').forEach(init); }
    document.addEventListener('DOMContentLoaded', scan);
    document.addEventListener('htmx:afterSwap', scan);
    document.addEventListener('htmx:afterSettle', scan);
})();
