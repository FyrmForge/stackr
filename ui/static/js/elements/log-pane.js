const RANK = { error: 0, warn: 1, info: 2, debug: 3 };
class LogPane extends HTMLElement {
    observer = null;
    follow = true;
    onInput = (e) => {
        const el = e.target;
        if (el.matches("[data-search], [data-level]")) {
            this.filterAll();
            return;
        }
        const lines = this.lines();
        if (!lines)
            return;
        switch (el.dataset.toggle) {
            case "timestamps":
                lines.toggleAttribute("data-no-ts", !el.checked);
                break;
            case "wrap":
                lines.toggleAttribute("data-wrap", el.checked);
                break;
            case "follow":
                this.follow = el.checked;
                if (this.follow)
                    this.scrollDown();
                break;
        }
    };
    connectedCallback() {
        const level = this.querySelector("[data-level]");
        if (level)
            level.value = this.getAttribute("level") ?? "";
        const search = this.querySelector("[data-search]");
        if (search)
            search.value = this.getAttribute("search") ?? "";
        this.addEventListener("input", this.onInput);
        this.addEventListener("change", this.onInput);
        const lines = this.lines();
        if (!lines)
            return;
        this.observer = new MutationObserver((records) => {
            for (const r of records)
                r.addedNodes.forEach((n) => this.filter(n));
            if (this.follow)
                this.scrollDown();
        });
        this.observer.observe(lines, { childList: true });
        this.filterAll();
    }
    disconnectedCallback() {
        this.observer?.disconnect();
        this.observer = null;
        this.removeEventListener("input", this.onInput);
        this.removeEventListener("change", this.onInput);
    }
    lines() {
        return this.querySelector("[data-lines]");
    }
    scrollDown() {
        const lines = this.lines();
        if (lines)
            lines.scrollTop = lines.scrollHeight;
    }
    filterAll() {
        this.lines()?.childNodes.forEach((n) => this.filter(n));
    }
    filter(node) {
        if (!(node instanceof HTMLElement))
            return;
        const min = this.querySelector("[data-level]")?.value ?? "";
        const query = (this.querySelector("[data-search]")?.value ?? "").toLowerCase();
        const rank = RANK[node.dataset.level ?? ""];
        const levelOK = min === "" || rank === undefined || rank <= (RANK[min] ?? 3);
        const textOK = query === "" || (node.textContent ?? "").toLowerCase().includes(query);
        node.hidden = !(levelOK && textOK);
    }
}
customElements.define("log-pane", LogPane);
export {};
