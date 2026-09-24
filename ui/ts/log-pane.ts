// <log-pane level="warn" search="timeout"> owns what the reader does with a
// log: follow the tail, filter by minimum level, search, and toggle
// timestamps and wrapping. htmx owns the stream and appends LogLine divs to
// [data-lines]; this element only reads and hides them (the hidden
// attribute), it never builds markup.
const RANK: Record<string, number> = { error: 0, warn: 1, info: 2, debug: 3 };

class LogPane extends HTMLElement {
  private observer: MutationObserver | null = null;
  private follow = true;

  private onInput = (e: Event): void => {
    const el = e.target as HTMLInputElement;
    if (el.matches("[data-search], [data-level]")) {
      this.filterAll();
      return;
    }
    const lines = this.lines();
    if (!lines) return;
    switch (el.dataset.toggle) {
      case "timestamps":
        lines.toggleAttribute("data-no-ts", !el.checked);
        break;
      case "wrap":
        lines.toggleAttribute("data-wrap", el.checked);
        break;
      case "follow":
        this.follow = el.checked;
        if (this.follow) this.scrollDown();
        break;
    }
  };

  connectedCallback(): void {
    const level = this.querySelector<HTMLSelectElement>("[data-level]");
    if (level) level.value = this.getAttribute("level") ?? "";
    const search = this.querySelector<HTMLInputElement>("[data-search]");
    if (search) search.value = this.getAttribute("search") ?? "";
    this.addEventListener("input", this.onInput);
    this.addEventListener("change", this.onInput);

    const lines = this.lines();
    if (!lines) return;
    this.observer = new MutationObserver((records) => {
      for (const r of records) r.addedNodes.forEach((n) => this.filter(n));
      if (this.follow) this.scrollDown();
    });
    this.observer.observe(lines, { childList: true });
    this.filterAll();
  }

  disconnectedCallback(): void {
    this.observer?.disconnect();
    this.observer = null;
    this.removeEventListener("input", this.onInput);
    this.removeEventListener("change", this.onInput);
  }

  private lines(): HTMLElement | null {
    return this.querySelector<HTMLElement>("[data-lines]");
  }

  private scrollDown(): void {
    const lines = this.lines();
    if (lines) lines.scrollTop = lines.scrollHeight;
  }

  private filterAll(): void {
    this.lines()?.childNodes.forEach((n) => this.filter(n));
  }

  // A line shows when its level is at or above the minimum (an unknown
  // level always shows) and its text contains the search, any case.
  private filter(node: Node): void {
    if (!(node instanceof HTMLElement)) return;
    const min = this.querySelector<HTMLSelectElement>("[data-level]")?.value ?? "";
    const query = (this.querySelector<HTMLInputElement>("[data-search]")?.value ?? "").toLowerCase();
    const rank = RANK[node.dataset.level ?? ""];
    const levelOK = min === "" || rank === undefined || rank <= (RANK[min] ?? 3);
    const textOK = query === "" || (node.textContent ?? "").toLowerCase().includes(query);
    node.hidden = !(levelOK && textOK);
  }
}

customElements.define("log-pane", LogPane);
