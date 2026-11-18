const CONTROL = "input:not([type=hidden]), select, textarea";
class SettingsRead extends HTMLElement {
    was = new WeakMap();
    old(val) {
        return (this.was.get(val) ?? []).map((n) => n.cloneNode(true));
    }
    control(row) {
        return row.querySelector("[data-editor]")?.querySelector(CONTROL) ?? null;
    }
    dirty(c) {
        if (c instanceof HTMLSelectElement) {
            const d = Array.from(c.options).findIndex((o) => o.defaultSelected);
            return c.selectedIndex !== (d < 0 ? 0 : d);
        }
        return c.value !== c.defaultValue;
    }
    reset(c) {
        if (c instanceof HTMLSelectElement) {
            const d = Array.from(c.options).findIndex((o) => o.defaultSelected);
            c.selectedIndex = d < 0 ? 0 : d;
        }
        else {
            c.value = c.defaultValue;
        }
    }
    said(c) {
        if (c instanceof HTMLSelectElement)
            return c.selectedOptions[0]?.textContent?.trim() ?? "";
        if (c instanceof HTMLInputElement && c.type === "password")
            return c.value === "" ? "" : "a new value";
        return c.value === "" ? c.placeholder || "inherit" : c.value;
    }
    show(row, editing) {
        row.querySelector("[data-read]").hidden = editing;
        row.querySelector("[data-editor]").hidden = !editing;
    }
    open(row) {
        this.show(row, true);
        this.control(row)?.focus();
        this.refresh();
    }
    paint(row) {
        const c = this.control(row);
        const val = row.querySelector("[data-val]");
        const undo = row.querySelector("[data-undo]");
        if (!c || !val)
            return;
        if (!this.was.has(val))
            this.was.set(val, Array.from(val.childNodes, (n) => n.cloneNode(true)));
        undo.hidden = !this.dirty(c);
        if (!this.dirty(c)) {
            val.replaceChildren(...this.old(val));
            return;
        }
        val.replaceChildren();
        const dot = document.createElement("span");
        dot.className = "mr-1.5 inline-block h-1.5 w-1.5 rounded-full bg-rw-accent align-middle";
        const now = document.createElement("span");
        now.textContent = this.said(c);
        const was = document.createElement("span");
        was.className = "ml-1.5 text-xs text-rw-faint line-through";
        was.append(...this.old(val));
        val.append(dot, now, was);
    }
    commit(row) {
        this.show(row, false);
        this.paint(row);
        this.refresh();
    }
    undo(row) {
        const c = this.control(row);
        if (c)
            this.reset(c);
        this.show(row, false);
        this.paint(row);
        this.refresh();
    }
    rows() {
        return Array.from(this.querySelectorAll("[data-row]"));
    }
    refresh() {
        const foot = this.querySelector("[data-pending]");
        if (!foot)
            return;
        const held = this.rows().filter((r) => {
            const c = this.control(r);
            return c !== null && this.dirty(c);
        });
        const open = this.rows().some((r) => !r.querySelector("[data-editor]").hidden);
        foot.hidden = held.length === 0 && !open;
        foot.querySelector("[data-count]").textContent = held.length === 1 ? "1 change not saved" : `${held.length} changes not saved`;
        const list = foot.querySelector("[data-list]");
        list.replaceChildren();
        for (const r of held) {
            const li = document.createElement("li");
            li.className = "flex flex-wrap items-baseline gap-x-2 text-sm";
            const val = r.querySelector("[data-val]");
            const spans = ["text-rw-muted", "text-xs text-rw-faint line-through", "text-rw-faint", "text-rw-text"].map((cls) => {
                const sp = document.createElement("span");
                sp.className = cls;
                return sp;
            });
            spans[0].textContent = r.dataset.label ?? "";
            spans[1].textContent = val ? this.old(val).map((n) => n.textContent).join("") : "";
            spans[2].textContent = "to";
            spans[3].textContent = this.said(this.control(r));
            li.append(...spans);
            list.append(li);
        }
    }
    onClick = (e) => {
        const t = e.target;
        const row = t.closest("[data-row]");
        if (t.closest("[data-cancel-all]")) {
            for (const r of this.rows())
                this.undo(r);
        }
        else if (row && t.closest("[data-edit]")) {
            this.open(row);
        }
        else if (row && t.closest("[data-undo], [data-cancel]")) {
            this.undo(row);
        }
    };
    onDblClick = (e) => {
        const row = e.target.closest("[data-row]");
        if (row && e.target.closest("[data-val]"))
            this.open(row);
    };
    onChange = (e) => {
        const row = e.target.closest("[data-row]");
        if (row && e.target.closest("[data-editor]"))
            this.commit(row);
    };
    connectedCallback() {
        this.addEventListener("click", this.onClick);
        this.addEventListener("dblclick", this.onDblClick);
        this.addEventListener("change", this.onChange);
        this.refresh();
    }
    disconnectedCallback() {
        this.removeEventListener("click", this.onClick);
        this.removeEventListener("dblclick", this.onDblClick);
        this.removeEventListener("change", this.onChange);
    }
}
customElements.define("settings-read", SettingsRead);
export {};
