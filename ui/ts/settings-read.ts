// <settings-read> turns the rows of a settings form into facts: each [data-row]
// shows its value with a pencil, and its [data-editor] (the real control)
// opens on the pencil or a double click. A committed edit stays local: the row
// shows the new value with the old one struck and an Undo, and the
// [data-pending] footer lists every change with Apply and Cancel. Nothing is
// posted until the form is.
type Control = HTMLInputElement | HTMLSelectElement | HTMLTextAreaElement;

const CONTROL = "input:not([type=hidden]), select, textarea";

class SettingsRead extends HTMLElement {
  private was = new WeakMap<HTMLElement, Node[]>();

  private old(val: HTMLElement): Node[] {
    return (this.was.get(val) ?? []).map((n) => n.cloneNode(true));
  }

  private control(row: Element): Control | null {
    return row.querySelector("[data-editor]")?.querySelector<Control>(CONTROL) ?? null;
  }

  private dirty(c: Control): boolean {
    if (c instanceof HTMLSelectElement) {
      const d = Array.from(c.options).findIndex((o) => o.defaultSelected);
      return c.selectedIndex !== (d < 0 ? 0 : d);
    }
    return c.value !== c.defaultValue;
  }

  private reset(c: Control): void {
    if (c instanceof HTMLSelectElement) {
      const d = Array.from(c.options).findIndex((o) => o.defaultSelected);
      c.selectedIndex = d < 0 ? 0 : d;
    } else {
      c.value = c.defaultValue;
    }
  }

  private said(c: Control): string {
    if (c instanceof HTMLSelectElement) return c.selectedOptions[0]?.textContent?.trim() ?? "";
    if (c instanceof HTMLInputElement && c.type === "password") return c.value === "" ? "" : "a new value";
    return c.value === "" ? c.placeholder || "inherit" : c.value;
  }

  private show(row: HTMLElement, editing: boolean): void {
    row.querySelector<HTMLElement>("[data-read]")!.hidden = editing;
    row.querySelector<HTMLElement>("[data-editor]")!.hidden = !editing;
  }

  private open(row: HTMLElement): void {
    this.show(row, true);
    this.control(row)?.focus();
    this.refresh();
  }

  // paint is the read view of a row: its stored value, or the held change.
  private paint(row: HTMLElement): void {
    const c = this.control(row);
    const val = row.querySelector<HTMLElement>("[data-val]");
    const undo = row.querySelector<HTMLElement>("[data-undo]");
    if (!c || !val) return;
    if (!this.was.has(val)) this.was.set(val, Array.from(val.childNodes, (n) => n.cloneNode(true)));
    undo!.hidden = !this.dirty(c);
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

  private commit(row: HTMLElement): void {
    this.show(row, false);
    this.paint(row);
    this.refresh();
  }

  private undo(row: HTMLElement): void {
    const c = this.control(row);
    if (c) this.reset(c);
    this.show(row, false);
    this.paint(row);
    this.refresh();
  }

  private rows(): HTMLElement[] {
    return Array.from(this.querySelectorAll<HTMLElement>("[data-row]"));
  }

  // refresh lists what is held in the footer; it shows while anything is held or open.
  private refresh(): void {
    const foot = this.querySelector<HTMLElement>("[data-pending]");
    if (!foot) return;
    const held = this.rows().filter((r) => {
      const c = this.control(r);
      return c !== null && this.dirty(c);
    });
    const open = this.rows().some((r) => !r.querySelector<HTMLElement>("[data-editor]")!.hidden);
    foot.hidden = held.length === 0 && !open;
    foot.querySelector("[data-count]")!.textContent = held.length === 1 ? "1 change not saved" : `${held.length} changes not saved`;
    const list = foot.querySelector("[data-list]")!;
    list.replaceChildren();
    for (const r of held) {
      const li = document.createElement("li");
      li.className = "flex flex-wrap items-baseline gap-x-2 text-sm";
      const val = r.querySelector<HTMLElement>("[data-val]");
      const spans = ["text-rw-muted", "text-xs text-rw-faint line-through", "text-rw-faint", "text-rw-text"].map((cls) => {
        const sp = document.createElement("span");
        sp.className = cls;
        return sp;
      });
      spans[0].textContent = r.dataset.label ?? "";
      spans[1].textContent = val ? this.old(val).map((n) => n.textContent).join("") : "";
      spans[2].textContent = "to";
      spans[3].textContent = this.said(this.control(r)!);
      li.append(...spans);
      list.append(li);
    }
  }

  private onClick = (e: Event): void => {
    const t = e.target as Element;
    const row = t.closest<HTMLElement>("[data-row]");
    if (t.closest("[data-cancel-all]")) {
      for (const r of this.rows()) this.undo(r);
    } else if (row && t.closest("[data-edit]")) {
      this.open(row);
    } else if (row && t.closest("[data-undo], [data-cancel]")) {
      this.undo(row);
    }
  };

  private onDblClick = (e: Event): void => {
    const row = (e.target as Element).closest<HTMLElement>("[data-row]");
    if (row && (e.target as Element).closest("[data-val]")) this.open(row);
  };

  private onChange = (e: Event): void => {
    const row = (e.target as Element).closest<HTMLElement>("[data-row]");
    if (row && (e.target as Element).closest("[data-editor]")) this.commit(row);
  };

  connectedCallback(): void {
    this.addEventListener("click", this.onClick);
    this.addEventListener("dblclick", this.onDblClick);
    this.addEventListener("change", this.onChange);
    this.refresh();
  }

  disconnectedCallback(): void {
    this.removeEventListener("click", this.onClick);
    this.removeEventListener("dblclick", this.onDblClick);
    this.removeEventListener("change", this.onChange);
  }
}

customElements.define("settings-read", SettingsRead);
