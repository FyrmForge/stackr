const picked = new Set();
class WaitingSelect extends HTMLElement {
    boxes() {
        return [...this.querySelectorAll("input[data-pick]")];
    }
    onChange = (e) => {
        const t = e.target;
        if (t.matches("[data-all]")) {
            this.boxes().filter((b) => !b.disabled).forEach((b) => (t.checked ? picked.add(b.value) : picked.delete(b.value)));
        }
        else if (t.matches("[data-pick]")) {
            t.checked ? picked.add(t.value) : picked.delete(t.value);
        }
        else
            return;
        this.sync();
    };
    sync() {
        const open = this.boxes().filter((b) => !b.disabled);
        const live = new Set(open.map((b) => b.value));
        [...picked].filter((v) => !live.has(v)).forEach((v) => picked.delete(v));
        open.forEach((b) => (b.checked = picked.has(b.value)));
        const n = picked.size;
        const count = this.querySelector("[data-count]");
        if (count)
            count.textContent = n ? `${n} selected` : "";
        const bulk = this.querySelector("[data-bulk]");
        if (bulk)
            bulk.hidden = n === 0;
        const label = bulk?.querySelector("[data-open]");
        if (label)
            label.textContent = `Promote ${n}`;
        const all = this.querySelector("[data-all]");
        if (all) {
            all.checked = n > 0 && n === open.length;
            all.indeterminate = n > 0 && n < open.length;
        }
    }
    connectedCallback() {
        this.addEventListener("change", this.onChange);
        this.sync();
    }
    disconnectedCallback() {
        this.removeEventListener("change", this.onChange);
        setTimeout(() => {
            if (!document.querySelector("waiting-select"))
                picked.clear();
        }, 0);
    }
}
customElements.define("waiting-select", WaitingSelect);
export {};
