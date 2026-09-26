const RANK = { error: 0, warn: 1, info: 2, debug: 3 };
const STATE = { "htmx:sseOpen": "open", "htmx:sseError": "error", "htmx:sseClose": "closed" };
const MAX = 3000;
class LogPane extends HTMLElement {
    observer = null;
    follow = true;
    pinned = 0;
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
    onState = (e) => {
        this.setAttribute("state", STATE[e.type]);
    };
    onScroll = () => {
        const l = this.lines();
        if (!l || !this.follow || l.scrollTop >= this.pinned - 4)
            return;
        this.follow = false;
        const box = this.querySelector('[data-toggle="follow"]');
        if (box)
            box.checked = false;
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
        for (const t of Object.keys(STATE))
            this.addEventListener(t, this.onState);
        const lines = this.lines();
        if (!lines)
            return;
        lines.addEventListener("scroll", this.onScroll);
        this.observer = new MutationObserver((records) => {
            for (const r of records)
                r.addedNodes.forEach((n) => this.filter(n));
            while (lines.childElementCount > MAX)
                lines.firstElementChild?.remove();
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
        for (const t of Object.keys(STATE))
            this.removeEventListener(t, this.onState);
        this.lines()?.removeEventListener("scroll", this.onScroll);
    }
    lines() {
        return this.querySelector("[data-lines]");
    }
    scrollDown() {
        const lines = this.lines();
        if (!lines)
            return;
        lines.scrollTop = lines.scrollHeight;
        this.pinned = lines.scrollTop;
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
addEventListener("pagehide", (e) => {
    if (e.persisted)
        return;
    for (const el of document.querySelectorAll("[sse-connect]")) {
        el["htmx-internal-data"]?.sseEventSource?.close();
    }
});
export {};
