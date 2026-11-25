// <waiting-select> is the Waiting page's bulk pick: [data-pick] boxes on the
// release rows, one [data-all] that ticks the enabled ones, "N selected" in
// [data-count] and the bulk [data-bulk] shown only while something is ticked.
// A live swap replaces the whole element, so the ticks live here, by value
// (release and env), and are put back when the new rows connect; a value that
// is no longer a pickable row drops out and the count follows. The ticks are
// cleared when the page is left: a swap of #waiting removes and adds an
// element in one go, leaving the page leaves none.
const picked = new Set<string>();

class WaitingSelect extends HTMLElement {
  private boxes(): HTMLInputElement[] {
    return [...this.querySelectorAll<HTMLInputElement>("input[data-pick]")];
  }

  private onChange = (e: Event): void => {
    const t = e.target as HTMLInputElement;
    if (t.matches("[data-all]")) {
      this.boxes().filter((b) => !b.disabled).forEach((b) => (t.checked ? picked.add(b.value) : picked.delete(b.value)));
    } else if (t.matches("[data-pick]")) {
      t.checked ? picked.add(t.value) : picked.delete(t.value);
    } else return;
    this.sync();
  };

  private sync(): void {
    const open = this.boxes().filter((b) => !b.disabled);
    const live = new Set(open.map((b) => b.value));
    [...picked].filter((v) => !live.has(v)).forEach((v) => picked.delete(v));
    open.forEach((b) => (b.checked = picked.has(b.value)));
    const n = picked.size;
    const count = this.querySelector("[data-count]");
    if (count) count.textContent = n ? `${n} selected` : "";
    const bulk = this.querySelector<HTMLElement>("[data-bulk]");
    if (bulk) bulk.hidden = n === 0;
    const label = bulk?.querySelector("[data-open]");
    if (label) label.textContent = `Promote ${n}`;
    const all = this.querySelector<HTMLInputElement>("[data-all]");
    if (all) {
      all.checked = n > 0 && n === open.length;
      all.indeterminate = n > 0 && n < open.length;
    }
  }

  connectedCallback(): void {
    this.addEventListener("change", this.onChange);
    this.sync();
  }

  disconnectedCallback(): void {
    this.removeEventListener("change", this.onChange);
    setTimeout(() => {
      if (!document.querySelector("waiting-select")) picked.clear();
    }, 0);
  }
}

customElements.define("waiting-select", WaitingSelect);
