// HTMX event handlers for stackr.

// Log HTMX errors to the console.
document.addEventListener('htmx:responseError', function(evt) {
    console.error('HTMX request failed:', evt.detail.xhr.status, evt.detail.xhr.statusText);
});

// Re-initialize components after HTMX swaps.
document.addEventListener('htmx:afterSwap', function(evt) {
    // Scroll to the first validation error if present.
    // An SSE outerHTML swap has no target.
    var firstError = evt.detail.target && evt.detail.target.querySelector('.field-error');
    if (firstError && firstError.textContent.trim() !== '') {
        firstError.scrollIntoView({ behavior: 'smooth', block: 'center' });
    }
});

// Handle HTMX send errors (network failures).
document.addEventListener('htmx:sendError', function(evt) {
    console.error('HTMX network error for:', evt.detail.elt);
});

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
    function checkFileNow(p) {
        var pathIn = p.querySelector('[data-path-input]');
        var note = p.querySelector('[data-path-note]');
        var repo = p.querySelector('[data-combo-input]').value.trim();
        if (!note || !repo.includes('/')) return;
        note.textContent = '';
        ask(p, '/file', {
            repo: repo,
            ref: p.querySelector('[data-branch-input]').value.trim(),
            path: pathIn.value.trim(),
        }, function (d) {
            note.textContent = d.exists ? d.path + ' found in this branch.' : d.path + ' is not in this branch yet.';
            note.classList.toggle('text-rw-warning', !d.exists);
            note.classList.toggle('text-rw-faint', d.exists);
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
