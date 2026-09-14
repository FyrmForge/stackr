// HTMX event handlers for stackr.

// Log HTMX errors to the console.
document.addEventListener('htmx:responseError', function(evt) {
    console.error('HTMX request failed:', evt.detail.xhr.status, evt.detail.xhr.statusText);
});

// Re-initialize components after HTMX swaps.
document.addEventListener('htmx:afterSwap', function(evt) {
    // Scroll to the first validation error if present.
    var firstError = evt.detail.target.querySelector('.field-error');
    if (firstError && firstError.textContent.trim() !== '') {
        firstError.scrollIntoView({ behavior: 'smooth', block: 'center' });
    }
});

// Handle HTMX send errors (network failures).
document.addEventListener('htmx:sendError', function(evt) {
    console.error('HTMX network error for:', evt.detail.elt);
});

// Custom confirm dialog: intercepts every hx-confirm so the browser's native
// dialog never appears. Confirming re-issues the request with the question
// suppressed.
(function () {
    var overlay = null;

    function closeModal() {
        if (overlay) { overlay.remove(); overlay = null; }
        document.removeEventListener('keydown', onKey);
    }

    function onKey(e) {
        if (e.key === 'Escape') closeModal();
    }

    function openModal(question, onConfirm) {
        closeModal();
        overlay = document.createElement('div');
        overlay.className = 'fixed inset-0 z-50 flex items-center justify-center p-4';
        overlay.innerHTML =
            '<div class="absolute inset-0 bg-black/60" data-confirm-cancel></div>' +
            '<div class="relative panel bg-rw-surface border border-rw-border rounded-xl shadow-2xl w-full max-w-sm p-5">' +
            '  <p class="text-sm text-rw-text mb-5" data-confirm-text></p>' +
            '  <div class="flex justify-end gap-2">' +
            '    <button type="button" class="btn btn-ghost btn-sm" data-confirm-cancel>Cancel</button>' +
            '    <button type="button" class="btn btn-danger btn-sm" data-confirm-ok>Confirm</button>' +
            '  </div>' +
            '</div>';
        overlay.querySelector('[data-confirm-text]').textContent = question;
        overlay.addEventListener('click', function (e) {
            if (e.target.closest('[data-confirm-ok]')) { closeModal(); onConfirm(); }
            else if (e.target.closest('[data-confirm-cancel]') || e.target === overlay) closeModal();
        });
        document.addEventListener('keydown', onKey);
        document.body.appendChild(overlay);
        overlay.querySelector('[data-confirm-ok]').focus();
    }

    document.addEventListener('htmx:confirm', function (evt) {
        var question = evt.detail.question;
        if (!question) return; // no hx-confirm on this element
        evt.preventDefault();
        openModal(question, function () { evt.detail.issueRequest(true); });
    });
})();

// The promote dialogue is a server-rendered fragment (releases.templ), not a
// one-line hx-confirm: it lists the commits going into the environment and can
// offer to apply a waiting config plan with them. It lands in #promote-modal,
// so closing it is emptying that container.
(function () {
    function box() { return document.getElementById('promote-modal'); }
    function close() { var b = box(); if (b) b.innerHTML = ''; }
    document.addEventListener('click', function (e) {
        if (e.target.closest('[data-modal-close]')) close();
    });
    document.addEventListener('keydown', function (e) {
        var b = box();
        if (e.key === 'Escape' && b && b.innerHTML !== '') {
            e.stopImmediatePropagation();
            close();
        }
    });
})();

// Global search palette: "/" or the rail icon opens it, Escape closes,
// arrows + Enter walk the results. Markup lives in components/search.templ
// (rendered by Layout on every signed-in page); results are an htmx fragment
// from GET /search. stopImmediatePropagation on handled keys keeps Escape
// from also closing a canvas drawer underneath, main.js loads before
// graph.js, so its document listener runs first.
(function () {
    function pal() { return document.getElementById('search-palette'); }
    function isOpen(p) { return p && !p.classList.contains('hidden'); }
    function open() {
        var p = pal();
        if (!p) return;
        p.classList.remove('hidden');
        var input = p.querySelector('#search-input');
        input.focus();
        input.select();
    }
    function close() { var p = pal(); if (p) p.classList.add('hidden'); }
    function rows(p) { return Array.prototype.slice.call(p.querySelectorAll('[data-search-row]')); }
    function move(p, dir) {
        var rs = rows(p);
        if (!rs.length) return;
        var cur = p.querySelector('[data-search-row].is-active');
        var i = rs.indexOf(cur);
        var n = i < 0 ? (dir > 0 ? 0 : rs.length - 1) : (i + dir + rs.length) % rs.length;
        rs.forEach(function (r) { r.classList.remove('is-active'); });
        rs[n].classList.add('is-active');
        rs[n].scrollIntoView({ block: 'nearest' });
    }
    document.addEventListener('keydown', function (e) {
        var t = e.target;
        var typing = t && (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName));
        var p = pal();
        if (e.key === '/' && !typing && !e.ctrlKey && !e.metaKey && !e.altKey && p) {
            e.preventDefault();
            e.stopImmediatePropagation();
            open();
            return;
        }
        if (!isOpen(p)) return;
        if (e.key === 'Escape') {
            e.stopImmediatePropagation();
            close();
        } else if (e.key === 'ArrowDown') {
            e.preventDefault();
            move(p, 1);
        } else if (e.key === 'ArrowUp') {
            e.preventDefault();
            move(p, -1);
        } else if (e.key === 'Enter') {
            var a = p.querySelector('[data-search-row].is-active') || rows(p)[0];
            if (a) { e.preventDefault(); window.location = a.getAttribute('href'); }
        }
    });
    document.addEventListener('click', function (e) {
        if (e.target.closest('[data-search-open]')) { open(); return; }
        var p = pal();
        if (isOpen(p) && e.target.closest('[data-search-close]')) close();
    });
    // Fresh results: first row pre-selected so Enter always goes somewhere.
    document.addEventListener('htmx:afterSwap', function (e) {
        if (e.detail.target.id !== 'search-results') return;
        var first = e.detail.target.querySelector('[data-search-row]');
        if (first) first.classList.add('is-active');
    });
})();

// Source-type visibility: settings-form rows tagged data-source-show="git"
// only render for matching source types, less noise than showing every field.
function applySourceVisibility(scope) {
    (scope || document).querySelectorAll('select[name="source_type"]').forEach(function (sel) {
        var form = sel.closest('form');
        if (!form) return;
        var update = function () {
            form.querySelectorAll('[data-source-show]').forEach(function (el) {
                el.style.display = el.dataset.sourceShow.split(' ').includes(sel.value) ? '' : 'none';
            });
        };
        if (!sel.dataset.sourceVisInit) {
            sel.dataset.sourceVisInit = '1';
            sel.addEventListener('change', update);
        }
        update();
    });
}
document.addEventListener('DOMContentLoaded', function () { applySourceVisibility(); });
document.addEventListener('htmx:afterSettle', function () { applySourceVisibility(); });

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

// Copy-from-server buttons: data-copy-url fetches the value before copying,
// so secret plaintext never sits in the DOM and the server can audit the read.
document.addEventListener('click', function (evt) {
    var btn = evt.target.closest('[data-copy-url]');
    if (!btn) return;
    fetch(btn.dataset.copyUrl).then(function (res) {
        if (!res.ok) throw new Error(res.status);
        return res.text();
    }).then(function (text) {
        return navigator.clipboard.writeText(text);
    }).then(function () {
        var old = btn.textContent;
        btn.textContent = 'Copied';
        setTimeout(function () { btn.textContent = old; }, 1500);
    }).catch(function () {
        btn.textContent = 'Failed';
        setTimeout(function () { btn.textContent = 'Copy'; }, 1500);
    });
});

// Repo picker: when a repo is chosen from the datalist, pre-fill the branch
// field with the repo's default branch (GitHub connector supplies it).
document.addEventListener('change', function(evt) {
    var el = evt.target;
    if (!el.matches || !el.matches('input[name="git_url"][list]')) return;
    var opt = document.querySelector('#' + el.getAttribute('list') + ' option[value="' + CSS.escape(el.value) + '"]');
    var branch = opt && opt.dataset.branch;
    var branchInput = el.form && el.form.querySelector('input[name="git_branch"]');
    if (branch && branchInput) branchInput.value = branch;
});

// Variables tab: reveal a masked secret, and toggle the row view against the
// raw editor. Delegated on document and defined here rather than in graph.js,
// the tab renders both inside the canvas drawer and on the standalone app page,
// and graph.js only loads on the canvas.
document.addEventListener('click', function (evt) {
    var reveal = evt.target.closest('[data-reveal]');
    if (reveal) {
        var secret = reveal.parentElement.querySelector('[data-secret]');
        if (!secret) return;
        var shown = secret.dataset.shown === '1';
        var show = function (value) {
            secret.textContent = value;
            secret.dataset.shown = '1';
            reveal.setAttribute('aria-pressed', 'true');
        };
        if (shown) {
            secret.textContent = '••••••••';
            secret.dataset.shown = '0';
            reveal.setAttribute('aria-pressed', 'false');
        } else if (secret.dataset.valueUrl) {
            // Secrets carry a URL, not the value: the fetch is the audit hook,
            // and plaintext stays out of the DOM until someone asks for it.
            fetch(secret.dataset.valueUrl).then(function (res) {
                if (!res.ok) throw new Error(res.status);
                return res.text();
            }).then(show).catch(function () { secret.textContent = 'unavailable'; });
        } else {
            show(secret.dataset.value || '');
        }
        return;
    }
    var view = document.getElementById('vars-view');
    var edit = document.getElementById('vars-edit');
    if (!view || !edit) return;
    if (evt.target.closest('[data-vars-edit]')) {
        view.classList.add('hidden');
        edit.classList.remove('hidden');
    } else if (evt.target.closest('[data-vars-cancel]')) {
        edit.classList.add('hidden');
        view.classList.remove('hidden');
    }
});

// Settings forms: mark a form dirty as soon as a control changes, so the
// "Unsaved changes" hint appears and the user can tell a section apart from
// its neighbours. Delegated because settings render both as pages and inside
// the canvas drawer, and htmx swaps them in and out.
document.addEventListener('input', function (evt) {
    var f = evt.target.closest && evt.target.closest('[data-settings-form]');
    if (f) f.classList.add('is-dirty');
});
document.addEventListener('change', function (evt) {
    var f = evt.target.closest && evt.target.closest('[data-settings-form]');
    if (f) f.classList.add('is-dirty');
});
document.body.addEventListener('htmx:afterRequest', function (evt) {
    var f = evt.target.closest && evt.target.closest('[data-settings-form]');
    // Status, not detail.successful: 4xx now counts as "handled" (see
    // responseHandling in layout.templ), and a rejected save must stay dirty.
    // Status 0 is an aborted request, a drawer swapped out, an hx-sync
    // cancel, and nothing was saved, so it must stay dirty too.
    var s = evt.detail.xhr && evt.detail.xhr.status;
    if (f && s > 0 && s < 400) f.classList.remove('is-dirty');
});

// API key scope presets. Delegated so it works on the account page whether it
// rendered as a page or was swapped in by htmx.
//
// "Read only" ticks everything not marked [data-scope-write], the same flag
// the server uses to decide what a viewer may grant, so the two can't drift.
// Disabled boxes are skipped throughout: they are the scopes this user has no
// access to grant, and a preset must not appear to grant them.
document.addEventListener('click', function (evt) {
    var btn = evt.target.closest && evt.target.closest('[data-scope-preset]');
    if (!btn) return;
    var root = btn.closest('[data-scope-presets]');
    if (!root) return;
    var mode = btn.dataset.scopePreset;
    root.querySelectorAll('input[type=checkbox][name=scopes]').forEach(function (box) {
        if (box.disabled) return;
        if (mode === 'all') box.checked = true;
        else if (mode === 'none') box.checked = false;
        else box.checked = !box.hasAttribute('data-scope-write');
    });
});

// Logo picker: the file input is hidden behind an avatar tile (the browser's
// own "Choose File" chrome cannot be styled), so the tile has to show what was
// picked, and the org's initials until then.
(function () {
    // Mirrors components.Initials: word starts plus capitals inside a word,
    // first two only, so the preview matches the badge it stands in for.
    function initials(s) {
        var out = '';
        s.trim().split(/\s+/).forEach(function (w) {
            for (var i = 0; i < w.length && out.length < 2; i++) {
                if (i === 0 || (w[i] >= 'A' && w[i] <= 'Z')) out += w[i];
            }
        });
        return out.toUpperCase();
    }
    document.addEventListener('input', function (e) {
        if (!e.target.matches('[data-logo-initials-source]')) return;
        var box = document.querySelector('[data-logo-initials]');
        if (box) box.textContent = initials(e.target.value);
    });
    document.addEventListener('change', function (e) {
        if (!e.target.matches('[data-logo-input]')) return;
        var f = e.target.files && e.target.files[0];
        if (!f) return;
        var img = document.querySelector('[data-logo-img]');
        var ini = document.querySelector('[data-logo-initials]');
        var name = document.querySelector('[data-logo-name]');
        if (img) { img.src = URL.createObjectURL(f); img.classList.remove('hidden'); }
        if (ini) ini.classList.add('hidden');
        if (name) name.textContent = f.name;
    });
})();

// Password reveal: every password field gets an eye toggle, wired here rather
// than in markup so login, register, invite, account and any secret field get
// it without each template remembering to.
(function () {
    var EYE = '<svg width="18" height="18" viewBox="0 0 20 20" fill="none" aria-hidden="true"><path d="M2 10C4.5 5.8 7.2 4 10 4s5.5 1.8 8 6c-2.5 4.2-5.2 6-8 6s-5.5-1.8-8-6Z" stroke="currentColor" stroke-width="1.5" stroke-linejoin="round"/><circle cx="10" cy="10" r="2.6" stroke="currentColor" stroke-width="1.5"/></svg>';
    var EYE_OFF = EYE.replace('</svg>', '<path d="M4 16.5 16 3.5" stroke="currentColor" stroke-width="1.5" stroke-linecap="round"/></svg>');

    function attach(input) {
        if (input.dataset.revealReady) return;
        input.dataset.revealReady = '1';
        var wrap = document.createElement('span');
        wrap.className = 'relative block';
        input.parentNode.insertBefore(wrap, input);
        wrap.appendChild(input);
        input.classList.add('pr-10');
        var btn = document.createElement('button');
        btn.type = 'button';
        btn.className = 'absolute inset-y-0 right-0 flex items-center px-3 text-rw-faint hover:text-rw-text transition-colors';
        btn.setAttribute('aria-label', 'Show password');
        btn.innerHTML = EYE;
        btn.addEventListener('click', function () {
            var reveal = input.type === 'password';
            input.type = reveal ? 'text' : 'password';
            input.classList.toggle('font-mono', reveal);
            btn.innerHTML = reveal ? EYE_OFF : EYE;
            btn.setAttribute('aria-label', reveal ? 'Hide password' : 'Show password');
        });
        wrap.appendChild(btn);
    }

    function scan(root) {
        (root || document).querySelectorAll('input[type="password"]').forEach(attach);
    }
    document.addEventListener('DOMContentLoaded', function () { scan(); });
    document.addEventListener('htmx:afterSettle', function (e) { scan(e.detail && e.detail.target); });
})();

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

    function base(p) {
        var sel = p.closest('form').querySelector('[name="connector_id"]');
        var id = sel ? sel.value : '';
        return id ? p.dataset.pickerBase + '/' + encodeURIComponent(id) : '';
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

// Open <details> survive an htmx swap, by id.
//
// hx-preserve cannot do this job: it keeps the whole old node, looked up by id
// from the incoming fragment. A row that changed while it was open, a cron run
// finishing, would be restored to the stale copy and stay "running" forever.
// Only the open state needs carrying across, so carry only that.
(function () {
    var open = {};
    // toggle does not bubble; capture catches it anyway.
    document.addEventListener('toggle', function (e) {
        var d = e.target;
        if (d.tagName === 'DETAILS' && d.id && d.hasAttribute('data-keep-open')) open[d.id] = d.open;
    }, true);
    document.addEventListener('htmx:afterSettle', function () {
        document.querySelectorAll('details[data-keep-open][id]').forEach(function (d) {
            if (open[d.id] !== undefined) d.open = open[d.id];
        });
    });
})();

// Modals (components/modal.templ, docs/plans/32-multi-node-ui.md).
//
// The dialog is server-rendered and swapped into #modal-host by htmx, so
// "open" is "the host has children" and closing is emptying it. No state
// anywhere, and a modal survives nothing, which is right, because every one
// of them is a confirm for an action that has to be re-asked.
(function () {
    var host = function () { return document.getElementById('modal-host'); };
    function close() {
        var h = host();
        if (h) h.innerHTML = '';
    }
    document.addEventListener('click', function (e) {
        if (e.target.closest('[data-modal-close]')) { close(); return; }
        // A click on the backdrop itself, not on the panel inside it.
        var b = e.target.closest('[data-modal]');
        if (b && e.target === b) close();
    });
    document.addEventListener('keydown', function (e) {
        if (e.key === 'Escape' && host() && host().firstElementChild) close();
    });
    // An action inside a modal that redirects has finished with it.
    document.addEventListener('htmx:beforeSwap', function (e) {
        if (e.detail.xhr && e.detail.xhr.getResponseHeader('HX-Redirect')) close();
    });

    // Type-to-enable. Anything that destroys data refuses to arm its button
    // until the exact name is typed: the damage is not undoable, so a
    // mis-click cannot reach it. The server checks the same thing; this is
    // only the arming, and a disabled button is not a guard.
    //
    // The box and the button are often in different elements (the box has to
    // be inside the form that posts, or carry form=), so the pair is found
    // through the nearest enclosing scope: a modal, or anything marked
    // data-confirm-scope for the in-page danger sections.
    document.addEventListener('input', function (e) {
        var f = e.target.closest('[data-confirm-input]');
        if (!f) return;
        var want = f.getAttribute('data-confirm-input');
        var scope = f.closest('[data-confirm-scope],[data-modal]');
        if (!scope) return;
        var btn = scope.querySelector('[data-confirm-submit]');
        if (btn) btn.disabled = f.value.trim() !== want;
    });
})();

// A background poll must not swallow a flash message.
//
// The flash lives in a cookie that the server clears the moment it reads it,
// and several pages refresh themselves on a timer with hx-select, the server
// renders the whole page, the flash cookie is consumed, and then everything
// except the selected fragment is thrown away, flash slot included. Any
// Without the marker, a message set by any handler dies within the poll
// interval while the operator remains on that page.
//
// Marking the request here rather than adding a query parameter to each
// polling URL: this catches every hx-trigger with an interval in it, including
// ones added later.
(function () {
    document.addEventListener('htmx:configRequest', function (e) {
        var el = e.detail && e.detail.elt;
        var trigger = el && el.getAttribute && el.getAttribute('hx-trigger');
        if (trigger && /(^|[,\s])every\s/.test(trigger)) {
            e.detail.headers['X-Stackr-Poll'] = '1';
        }
    });
})();
