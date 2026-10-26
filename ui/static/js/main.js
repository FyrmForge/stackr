// HTMX event handlers for stackr.

// Log HTMX errors to the console.
document.addEventListener('htmx:responseError', function(evt) {
    console.error('HTMX request failed:', evt.detail.xhr.status, evt.detail.xhr.statusText);
});

// Re-initialize components after HTMX swaps.
document.addEventListener('htmx:afterSwap', function(evt) {
    // Bring what the answer said into view: the first field showing an
    // error, else the note a Section drew by its form. Nearest: no jump when
    // it is already on screen. An SSE outerHTML swap has no target.
    // An outerHTML swap leaves target as the old, detached node: read the
    // new one by its id. A refused field folded in a closed <details> (Show
    // advanced) opens it, so the reason can be seen.
    var t = evt.detail.target;
    if (t && !t.isConnected) t = t.id ? document.getElementById(t.id) : null;
    if (!t || !t.querySelector) return;
    t.querySelectorAll('.field-error[data-has-error="true"]').forEach(function (e) {
        for (var d = e.closest('details'); d; d = d.parentElement.closest('details')) d.open = true;
    });
    var hit = t.querySelector('.field-error[data-has-error="true"]') || t.querySelector('[data-acted]');
    if (hit) {
        hit.scrollIntoView({ behavior: 'smooth', block: 'nearest' });
    }
});

// Handle HTMX send errors (network failures).
document.addEventListener('htmx:sendError', function(evt) {
    console.error('HTMX network error for:', evt.detail.elt);
});

// A live tab ([data-live]: it polls itself or follows a job stream) that
// loses the panel, as during an upgrade's restart, says Reconnecting and holds
// its buttons. The poll keeps going and the stream reconnects with backoff;
// the next good poll swaps the whole tab back in, a reopened stream lifts it.
(function () {
    function mark(evt, lost) {
        var live = evt.target.closest && evt.target.closest('[data-live]');
        if (!live) return;
        live.querySelectorAll('[data-reconnecting]').forEach(function (n) { n.hidden = !lost; });
        live.querySelectorAll('button').forEach(function (b) { b.disabled = lost; });
    }
    ['htmx:sendError', 'htmx:responseError', 'htmx:sseError'].forEach(function (name) {
        document.addEventListener(name, function (evt) { mark(evt, true); });
    });
    document.addEventListener('htmx:sseOpen', function (evt) { mark(evt, false); });
})();

// Re-validate a field while typing, but only once it is already showing an
// error. Pairs with hx-trigger="blur, hamr:revalidate" on the input. The
// condition lives here rather than in an hx-trigger [...] filter because htmx
// compiles those with Function(), which needs 'unsafe-eval' in the CSP.
// Fields are matched to their error span by the id convention in
// components/form: <input name="x"> <-> <span id="error-x" data-has-error>.
// data-hamr-watch="y" watches another field's error span instead (cross-field).
document.addEventListener('input', function(evt) {
    var el = evt.target;
    if (!el.name) return;
    var err = document.getElementById('error-' + (el.dataset.hamrWatch || el.name));
    if (!err || err.dataset.hasError !== 'true') return;
    clearTimeout(el._hamrRevalidate);
    el._hamrRevalidate = setTimeout(function() { htmx.trigger(el, 'hamr:revalidate'); }, 300);
});

// Copy-to-clipboard buttons: any element with data-copy copies its value and
// flashes "Copied" for a moment.
document.addEventListener('click', function (evt) {
    var btn = evt.target.closest('[data-copy]');
    if (!btn) return;
    navigator.clipboard.writeText(btn.dataset.copy).then(function () {
        var old = btn.textContent;
        btn.textContent = 'Copied';
        setTimeout(function () { btn.textContent = old; }, 1500);
    });
});

// Repo picker: the repository / branch / file-path trio on the config-as-code
// form. The repository list is already in the page (the install gate loaded
// it), so filtering is local and instant; branch and path have to be asked for,
// because neither is knowable until a repository is picked.
(function () {
    function pickerOf(el) { return el.closest && el.closest('[data-repo-picker]'); }

    // options() re-reads the DOM every time rather than caching: the form is
    // re-rendered by htmx on every save, and a cached list would go stale.
    function options(p) { return Array.prototype.slice.call(p.querySelectorAll('.combo-option')); }

    function close(p) {
        var list = p.querySelector('[data-combo-list]');
        list.hidden = true;
        p.querySelector('[data-combo-input]').setAttribute('aria-expanded', 'false');
        options(p).forEach(function (o) { o.setAttribute('aria-selected', 'false'); });
    }

    // filter shows what matches and marks where, so a long list reads as an
    // answer rather than as a wall to scroll.
    function filter(p) {
        var input = p.querySelector('[data-combo-input]');
        var q = input.value.trim().toLowerCase();
        var shown = 0;
        options(p).forEach(function (o) {
            var v = o.dataset.value, i = q ? v.toLowerCase().indexOf(q) : -1;
            var hit = !q || i >= 0;
            o.hidden = !hit;
            o.setAttribute('aria-selected', 'false');
            if (hit && i >= 0) {
                o.textContent = '';
                o.appendChild(document.createTextNode(v.slice(0, i)));
                var m = document.createElement('mark');
                m.textContent = v.slice(i, i + q.length);
                o.appendChild(m);
                o.appendChild(document.createTextNode(v.slice(i + q.length)));
            } else if (hit) {
                o.textContent = v;
            }
            if (hit) shown++;
        });
        var list = p.querySelector('[data-combo-list]');
        list.hidden = shown === 0;
        input.setAttribute('aria-expanded', String(shown > 0));
    }

    function active(p) { return p.querySelector('.combo-option[aria-selected="true"]'); }

    function move(p, dir) {
        var vis = options(p).filter(function (o) { return !o.hidden; });
        if (!vis.length) return;
        var i = vis.indexOf(active(p));
        vis.forEach(function (o) { o.setAttribute('aria-selected', 'false'); });
        i = i < 0 ? (dir > 0 ? 0 : vis.length - 1) : (i + dir + vis.length) % vis.length;
        vis[i].setAttribute('aria-selected', 'true');
        vis[i].scrollIntoView({ block: 'nearest' });
    }

    // No data-picker-base, nothing to ask: the branch and path lookups
    // stay quiet and the fields take what is typed.
    function base(p) {
        var sel = p.closest('form').querySelector('[name="connector_id"]');
        var id = sel ? sel.value : '';
        return id && p.dataset.pickerBase ? p.dataset.pickerBase + '/' + encodeURIComponent(id) : '';
    }

    function ask(p, path, params, then) {
        var b = base(p);
        if (!b) return;
        fetch(b + path + '?' + new URLSearchParams(params), { headers: { Accept: 'application/json' } })
            .then(function (r) { return r.ok ? r.json() : null; })
            .then(function (d) { if (d) then(d); })
            .catch(function () { /* the field still accepts anything typed */ });
    }

    // checkFile says whether the path is already in the repo. Both answers are
    // useful: "not there" means the plan will be an empty org, which is worth
    // knowing before it runs rather than after.
    // Debounced: every call is a GitHub API request, and typing a path is a
    // dozen keystrokes.
    var fileTimer;
    function checkFile(p) {
        clearTimeout(fileTimer);
        fileTimer = setTimeout(function () { checkFileNow(p); }, 300);
    }
    // The repo's YAML files at the branch fill the path's datalist, so the
    // browser filters them as the path is typed. One fetch per repo and
    // branch; typing the path only re-reads the cached list.
    function checkFileNow(p) {
        var pathIn = p.querySelector('[data-path-input]');
        var note = p.querySelector('[data-path-note]');
        var repo = p.querySelector('[data-combo-input]').value.trim();
        if (!note || !repo.includes('/')) return;
        var ref = p.querySelector('[data-branch-input]').value.trim();
        var key = repo + '@' + ref;
        function show(files) {
            var path = pathIn.value.trim() || pathIn.placeholder;
            var found = files.indexOf(path) >= 0;
            note.textContent = found ? path + ' found in this branch.' : path + ' is not in this branch yet.';
            note.classList.toggle('text-rw-warning', !found);
            note.classList.toggle('text-rw-faint', found);
        }
        if (p._filesKey === key) { show(p._files); return; }
        note.textContent = '';
        ask(p, '/files', { repo: repo, ref: ref }, function (files) {
            p._filesKey = key;
            p._files = files;
            var dl = p.querySelector('[data-path-list]');
            if (dl) {
                dl.innerHTML = '';
                files.forEach(function (f) {
                    var o = document.createElement('option');
                    o.value = f;
                    dl.appendChild(o);
                });
            }
            show(files);
        });
    }

    function pick(p, opt) {
        var input = p.querySelector('[data-combo-input]');
        input.value = opt.dataset.value;
        close(p);
        var branch = p.querySelector('[data-branch-input]');
        if (branch && !branch.value.trim() && opt.dataset.branch) branch.value = opt.dataset.branch;
        ask(p, '/branches', { repo: opt.dataset.value }, function (names) {
            var dl = p.querySelector('[data-branch-list]');
            if (!dl) return;
            dl.innerHTML = '';
            names.forEach(function (n) {
                var o = document.createElement('option');
                o.value = n;
                dl.appendChild(o);
            });
        });
        checkFile(p);
    }

    document.addEventListener('input', function (e) {
        var p = pickerOf(e.target);
        if (!p) return;
        if (e.target.matches('[data-combo-input]')) filter(p);
        if (e.target.matches('[data-branch-input], [data-path-input]')) checkFile(p);
    });
    document.addEventListener('focusin', function (e) {
        var p = pickerOf(e.target);
        if (p && e.target.matches('[data-combo-input]')) filter(p);
        // A saved repo was never picked on this page: list its files now.
        if (p && e.target.matches('[data-path-input]')) checkFileNow(p);
    });
    document.addEventListener('keydown', function (e) {
        var p = pickerOf(e.target);
        if (!p || !e.target.matches('[data-combo-input]')) return;
        if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
            e.preventDefault();
            move(p, e.key === 'ArrowDown' ? 1 : -1);
        } else if (e.key === 'Enter') {
            var a = active(p);
            // Enter with nothing highlighted is a plain submit: the field
            // accepts a repo that is not in the list.
            if (a) { e.preventDefault(); pick(p, a); }
        } else if (e.key === 'Escape') {
            close(p);
        }
    });
    // mousedown, not click: the input blurs first otherwise and the list is
    // already gone by the time the click lands.
    document.addEventListener('mousedown', function (e) {
        var p = pickerOf(e.target);
        if (!p) {
            document.querySelectorAll('[data-repo-picker]').forEach(close);
            return;
        }
        var opt = e.target.closest('.combo-option');
        if (opt) { e.preventDefault(); pick(p, opt); }
    });
})();

// The sharing control (components.Share): the org boxes count only under
// "Only these orgs", so they are disabled, and not posted, under the others.
document.addEventListener('change', function (evt) {
    var radio = evt.target;
    var share = radio.type === 'radio' && radio.closest('[data-share]');
    if (!share) return;
    share.querySelectorAll('[data-share-org]').forEach(function (b) { b.disabled = radio.value !== 'only'; });
});

// An input with data-follows="name" fills itself from that field of its
// form plus data-suffix as the name is typed (the add-node Address), until
// it is edited by hand (data-edited).
document.addEventListener('input', function (evt) {
    var t = evt.target;
    if (t.matches && t.matches('[data-follows]')) {
        t.setAttribute('data-edited', '');
        return;
    }
    if (!t.form || !t.name) return;
    t.form.querySelectorAll('[data-follows="' + t.name + '"]:not([data-edited])').forEach(function (f) {
        var suffix = f.getAttribute('data-suffix') || '';
        f.value = t.value.trim() && suffix ? t.value.trim() + suffix : '';
    });
});
