// <settings-find> wraps components.SettingsAccordion. The rows are native
// <details> sharing one name, so the browser keeps one open. The open row
// lives in ?open= (history.replaceState, so Back does not walk every
// toggle) and a fresh load opens the row ?open= names. [data-find] filters the
// rows by their title, summary, labels, headings, hints, a nested <details>'
// summary (Show advanced carries data-hint) and a field's aria-label, opens the hits and marks them; [data-only-changed]
// hides the [data-field]s without data-changed and the rows with none. While a
// filter runs the rows leave the name group and the URL is left alone; clearing
// it restores the row open before. [data-find-skip] and .sr-only are never searched.
// A hit inside a closed <details> (Show advanced) opens it until the filter
// changes. A POST answered inside a row fires "refresh" on [data-acc-refresh].
const LABELS = "[data-acc-title], [data-acc-sum], label, .label, .settings-label, h2, h3, h4, legend, [data-hint], .settings-help, :is(input, select, textarea)[aria-label]";

// matches is the live region's count of the rows a filter shows.
export function matches(n: number): string {
  return n === 0 ? "No settings match." : n === 1 ? "1 section matches." : `${n} sections match.`;
}

// pattern finds q literally, color and colour as one word.
export function pattern(q: string): RegExp {
  return new RegExp(q.replace(/[.*+?^${}()|[\]\\]/g, "\\$&").replace(/colou?r/g, "colou?r"));
}

class SettingsFind extends HTMLElement {
  private was: string | null = null; // the open row while filtering; null = no filter
  private opened: HTMLDetailsElement[] = []; // nested <details> a hit opened

  private rows(): HTMLDetailsElement[] {
    return [...this.querySelectorAll<HTMLDetailsElement>("details[data-acc]")];
  }

  private onToggle = (e: Event): void => {
    const row = e.target as HTMLDetailsElement;
    if (this.was !== null || !row.matches("details[data-acc]")) return;
    const key = row.dataset.acc ?? "";
    const url = new URL(location.href);
    if (row.open) url.searchParams.set("open", key);
    else if (url.searchParams.get("open") === key) url.searchParams.delete("open");
    else return;
    history.replaceState(history.state, "", url);
  };

  private onInput = (e: Event): void => {
    if (!(e.target as Element).matches("[data-find], [data-only-changed]")) return; // a field in a row
    const q = (this.querySelector<HTMLInputElement>("[data-find]")?.value ?? "").trim().toLowerCase();
    const only = this.querySelector<HTMLInputElement>("[data-only-changed]")?.checked ?? false;
    this.unmark();
    const rows = this.rows();
    if (!q && !only) return this.restore(rows);
    if (this.was === null) this.was = rows.find((r) => r.open)?.dataset.acc ?? "";
    let n = 0;
    for (const row of rows) {
      row.removeAttribute("name");
      row.querySelectorAll<HTMLElement>("[data-field]").forEach((f) => (f.hidden = only && !f.hasAttribute("data-changed")));
      let hit = !q;
      if (q) {
        for (const el of row.querySelectorAll(LABELS)) {
          const named = el.matches("[aria-label]") && pattern(q).test(el.getAttribute("aria-label")!.toLowerCase());
          if (!el.closest("[data-field][hidden]") && (named || this.mark(el, q))) {
            hit = true;
            this.opened.push(...openTo(el, row));
          }
        }
      }
      const show = hit && (!only || row.matches("[data-changed]") || row.querySelector("[data-changed]") !== null);
      row.hidden = !show;
      row.open = show;
      if (show) n++;
    }
    this.status(matches(n));
  };

  // mark wraps each hit in el's own text (not in [data-find-skip]) in a
  // <mark>; true = el holds q.
  private mark(el: Element, q: string): boolean {
    const walk = document.createTreeWalker(el, NodeFilter.SHOW_TEXT);
    const texts: Text[] = [];
    while (walk.nextNode()) {
      const t = walk.currentNode as Text;
      if (!t.parentElement?.closest("[data-find-skip], .sr-only, mark[data-find-hit]")) texts.push(t);
    }
    const re = pattern(q);
    let hit = false;
    for (let t of texts) {
      let found: RegExpExecArray | null;
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

  private unmark(): void {
    this.opened.forEach((d) => (d.open = false));
    this.opened = [];
    this.querySelectorAll("mark[data-find-hit]").forEach((m) => {
      const parent = m.parentNode;
      m.replaceWith(...m.childNodes);
      parent?.normalize();
    });
  }

  // restore ends a filter: every row and field back, the name group back
  // (after closing, or the browser would close all but one), the row that
  // was open before opened again.
  private restore(rows: HTMLDetailsElement[]): void {
    if (this.was === null) return;
    const before = this.was;
    this.was = null;
    for (const row of rows) {
      row.hidden = false;
      row.open = false;
      row.setAttribute("name", this.id);
      row.querySelectorAll<HTMLElement>("[data-field]").forEach((f) => (f.hidden = false));
    }
    rows.find((r) => r.dataset.acc === before)?.toggleAttribute("open", true);
    this.status("");
  }

  private status(text: string): void {
    this.querySelector("[data-find-status]")?.replaceChildren(text);
  }

  // onSaved: a POST answered inside a row redraws the row heads. htmx fires
  // afterRequest on the nearest still-connected ancestor of the poster, so a
  // whole-drawer answer lands above the new accordion, not here.
  private onSaved = (e: Event): void => {
    const d = (e as CustomEvent).detail;
    if (!d?.successful || d.requestConfig?.verb !== "post") return;
    this.querySelector("[data-acc-refresh]")?.dispatchEvent(new Event("refresh"));
  };

  connectedCallback(): void {
    // the row the server opened (a save's answer) wins over the URL's
    const key = new URLSearchParams(location.search).get("open");
    const rows = this.rows();
    const row = rows.find((r) => r.dataset.acc === key);
    if (row && !rows.some((r) => r.open)) row.open = true;
    this.addEventListener("toggle", this.onToggle, true); // toggle does not bubble
    this.addEventListener("input", this.onInput);
    this.addEventListener("htmx:afterRequest", this.onSaved);
  }

  disconnectedCallback(): void {
    this.removeEventListener("toggle", this.onToggle, true);
    this.removeEventListener("input", this.onInput);
    this.removeEventListener("htmx:afterRequest", this.onSaved);
  }
}

// openTo opens every closed <details> between el and its row; the ones it opened.
export function openTo(el: Element, row: Element): HTMLDetailsElement[] {
  const out: HTMLDetailsElement[] = [];
  for (let p = el.parentElement; p && p !== row; p = p.parentElement) {
    if (p.tagName === "DETAILS" && !(p as HTMLDetailsElement).open) {
      (p as HTMLDetailsElement).open = true;
      out.push(p as HTMLDetailsElement);
    }
  }
  return out;
}

customElements.define("settings-find", SettingsFind);
