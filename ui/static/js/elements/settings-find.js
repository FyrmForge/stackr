const LABELS = "[data-acc-title], label, .label, .settings-label";
class SettingsFind extends HTMLElement {
    was = null;
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
            row.querySelectorAll("[data-field]").forEach((f) => {
                f.hidden = only && !f.hasAttribute("data-changed");
            });
            let hit = !q;
            if (q) {
                for (const el of row.querySelectorAll(LABELS)) {
                    if (!el.closest("[data-field][hidden]") && this.mark(el, q))
                        hit = true;
                }
            }
            const show = hit && (!only || row.matches("[data-changed]") || row.querySelector("[data-changed]") !== null);
            row.hidden = !show;
            row.open = show;
            if (show)
                n++;
        }
        this.status(n === 1 ? "1 group matches." : `${n} groups match.`);
    };
    mark(el, q) {
        const walk = document.createTreeWalker(el, NodeFilter.SHOW_TEXT);
        const texts = [];
        while (walk.nextNode()) {
            const t = walk.currentNode;
            if (!t.parentElement?.closest("[data-find-skip]"))
                texts.push(t);
        }
        let hit = false;
        for (let t of texts) {
            let i;
            while ((i = t.data.toLowerCase().indexOf(q)) >= 0) {
                const m = document.createElement("mark");
                m.dataset.findHit = "";
                const word = t.splitText(i);
                t = word.splitText(q.length);
                word.before(m);
                m.append(word);
                hit = true;
            }
        }
        return hit;
    }
    unmark() {
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
        const open = rows.find((r) => r.dataset.acc === before);
        if (open)
            open.open = true;
        this.status("");
    }
    status(text) {
        const el = this.querySelector("[data-find-status]");
        if (el)
            el.textContent = text;
    }
    connectedCallback() {
        const key = new URLSearchParams(location.search).get("open");
        const row = this.rows().find((r) => r.dataset.acc === key);
        if (row)
            row.open = true;
        this.addEventListener("toggle", this.onToggle, true);
        this.addEventListener("input", this.onInput);
    }
    disconnectedCallback() {
        this.removeEventListener("toggle", this.onToggle, true);
        this.removeEventListener("input", this.onInput);
    }
}
customElements.define("settings-find", SettingsFind);
export {};
