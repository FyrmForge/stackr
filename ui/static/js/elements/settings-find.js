const LABELS = "[data-acc-title], [data-acc-sum], label, .label, .settings-label, h2, h3, h4, legend, [data-hint], .settings-help, :is(input, select, textarea)[aria-label], option";
export function matches(n) {
    return n === 0 ? "No settings match." : n === 1 ? "1 section matches." : `${n} sections match.`;
}
export function pattern(q) {
    return new RegExp(q.replace(/[.*+?^${}()|[\]\\]/g, "\\$&").replace(/colou?r/g, "colou?r"));
}
class SettingsFind extends HTMLElement {
    was = null;
    opened = [];
    rows() {
        return [...this.querySelectorAll("details[data-acc]")];
    }
    onToggle = (e) => {
        const row = e.target;
        if (this.was !== null || !row.matches("details[data-acc]"))
            return;
        const key = row.dataset.acc ?? "";
        const url = new URL(location.href);
        if (row.open)
            url.searchParams.set("open", key);
        else if (url.searchParams.get("open") === key)
            url.searchParams.delete("open");
        else
            return;
        history.replaceState(history.state, "", url);
    };
    onInput = (e) => {
        if (!e.target.matches("[data-find], [data-only-changed]"))
            return;
        const q = (this.querySelector("[data-find]")?.value ?? "").trim().toLowerCase();
        const only = this.querySelector("[data-only-changed]")?.checked ?? false;
        this.unmark();
        const rows = this.rows();
        if (!q && !only)
            return this.restore(rows);
        if (this.was === null)
            this.was = rows.find((r) => r.open)?.dataset.acc ?? "";
        let n = 0;
        for (const row of rows) {
            row.removeAttribute("name");
            row.querySelectorAll("[data-field]").forEach((f) => (f.hidden = only && !f.hasAttribute("data-changed")));
            let hit = !q;
            if (q) {
                for (const el of row.querySelectorAll(LABELS)) {
                    const opt = el.matches("option"), said = opt ? (el.textContent ?? "") : el.getAttribute("aria-label");
                    if (!el.closest("[data-field][hidden]") && ((said !== null && pattern(q).test(said.toLowerCase())) || (!opt && this.mark(el, q)))) {
                        hit = true;
                        this.opened.push(...openTo(el, row));
                    }
                }
            }
            const show = hit && (!only || row.matches("[data-changed]") || row.querySelector("[data-changed]") !== null);
            row.hidden = !show;
            row.open = show;
            if (show)
                n++;
        }
        this.status(matches(n));
    };
    mark(el, q) {
        const walk = document.createTreeWalker(el, NodeFilter.SHOW_TEXT);
        const texts = [];
        while (walk.nextNode()) {
            const t = walk.currentNode;
            if (!t.parentElement?.closest("[data-find-skip], .sr-only, mark[data-find-hit]"))
                texts.push(t);
        }
        const re = pattern(q);
        let hit = false;
        for (let t of texts) {
            let found;
            while ((found = re.exec(t.data.toLowerCase()))) {
                const m = document.createElement("mark");
                m.dataset.findHit = "";
                const word = t.splitText(found.index);
                t = word.splitText(found[0].length);
                word.before(m);
                m.append(word);
                hit = true;
            }
        }
        return hit;
    }
    unmark() {
        this.opened.forEach((d) => (d.open = false));
        this.opened = [];
        this.querySelectorAll("mark[data-find-hit]").forEach((m) => {
            const parent = m.parentNode;
            m.replaceWith(...m.childNodes);
            parent?.normalize();
        });
    }
    restore(rows) {
        if (this.was === null)
            return;
        const before = this.was;
        this.was = null;
        for (const row of rows) {
            row.hidden = false;
            row.open = false;
            row.setAttribute("name", this.id);
            row.querySelectorAll("[data-field]").forEach((f) => (f.hidden = false));
        }
        rows.find((r) => r.dataset.acc === before)?.toggleAttribute("open", true);
        this.status("");
    }
    status(text) {
        this.querySelector("[data-find-status]")?.replaceChildren(text);
    }
    onSaved = (e) => {
        const d = e.detail;
        if (!d?.successful || d.requestConfig?.verb !== "post")
            return;
        this.querySelector("[data-acc-refresh]")?.dispatchEvent(new Event("refresh"));
    };
    connectedCallback() {
        const key = new URLSearchParams(location.search).get("open");
        const rows = this.rows();
        const row = rows.find((r) => r.dataset.acc === key);
        if (row && !rows.some((r) => r.open))
            row.open = true;
        this.addEventListener("toggle", this.onToggle, true);
        this.addEventListener("input", this.onInput);
        this.addEventListener("htmx:afterRequest", this.onSaved);
    }
    disconnectedCallback() {
        this.removeEventListener("toggle", this.onToggle, true);
        this.removeEventListener("input", this.onInput);
        this.removeEventListener("htmx:afterRequest", this.onSaved);
    }
}
export function openTo(el, row) {
    const out = [];
    for (let p = el.parentElement; p && p !== row; p = p.parentElement) {
        if (p.tagName === "DETAILS" && !p.open) {
            p.open = true;
            out.push(p);
        }
    }
    return out;
}
customElements.define("settings-find", SettingsFind);
