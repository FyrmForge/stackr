// <side-drawer open tab> is in the layout once. htmx swaps a card's drawer
// into its #drawer-body; the swap opens it and copies ?tab= into "tab" (the
// URL is pushed before the swap). [data-close], [data-backdrop] and Escape
// close it: the body empties, ?drawer, ?tab and ?open (settings-find's
// open row) leave the URL through history.replaceState, and a
// bubbling "drawer-closed" fires. While open, Tab cycles inside the drawer
// and closing hands focus back to what opened it. An edited form asks
// (in a modal of its own) before a close drops it; a save from inside the
// drawer's edited form clears the edits (the vars grid's staging requests do
// not; its pending footer counts as unsaved); a failed request into the open
// drawer shows there, with its request id, instead of as a toast.
// ponytail: a close during an in-flight open reopens on arrival; add a
// request guard if that shows up.
const FOCUSABLE = 'a[href], button:not([disabled]), input:not([disabled]), select, textarea, [tabindex]:not([tabindex="-1"])';

type Failed = { target?: Element; xhr?: { status: number; getResponseHeader(name: string): string | null } };

class SideDrawer extends HTMLElement {
  private opener: HTMLElement | null = null;
  private dirty = false;
  private fullByLink = false; // a data-full link set full: the close undoes it
  private saves = new WeakSet<object>(); // xhrs of non-GET requests sent from an edited form
  private edited = new WeakSet<Element>(); // forms the user changed

  private onSwap = (e: Event): void => {
    const d = (e as CustomEvent<{ target?: Element; requestConfig?: { elt?: Element } }>).detail;
    const target = d?.target;
    const body = this.body();
    if (!body || !target || !body.contains(target)) return;
    // only a new drawer (the body itself) resets edits and focus: a swap
    // of a child (a poll, a tab's refresh) must not
    if (target === body) this.dirty = false;
    const was = document.activeElement as HTMLElement | null;
    if (!this.hasAttribute("open") && was && !this.contains(was)) this.opener = was;
    this.setAttribute("open", "");
    // a link marked data-full (the Releases screen's reviews) opens the drawer wide
    if (d.requestConfig?.elt?.closest("[data-full]") && !this.hasAttribute("full")) this.expand(true, true);
    const tab = new URLSearchParams(location.search).get("tab");
    if (tab) this.setAttribute("tab", tab);
    else this.removeAttribute("tab");
    if (target === body) this.querySelector<HTMLElement>("[data-close]")?.focus({ preventScroll: true });
  };

  // A card's request into the closed drawer opens it at once on a loading
  // body, so a slow answer never looks like nothing happened.
  private onBefore = (e: Event): void => {
    const body = this.body();
    const d = (e as CustomEvent<{ elt?: Element; xhr?: object; target?: Element; requestConfig?: { verb?: string } }>).detail;
    // a save is a non-GET request from an edited form (not [data-hold]
    // staging); onDone clears dirty when it ends well, swap or not
    const form = d?.elt?.closest("form");
    if (body && d?.xhr && d.elt && form && this.edited.has(form) && !d.elt.closest("[data-hold]") && body.contains(d.elt) && (d.requestConfig?.verb ?? "get").toLowerCase() !== "get") this.saves.add(d.xhr);
    if (!body || d?.target !== body || this.hasAttribute("open")) return;
    const was = document.activeElement as HTMLElement | null;
    if (was && !this.contains(was)) this.opener = was;
    const p = document.createElement("p");
    p.className = "p-6 text-sm text-rw-muted";
    p.setAttribute("aria-busy", "true");
    p.dataset.loading = "";
    p.textContent = "Loading…";
    body.replaceChildren(p);
    this.setAttribute("open", "");
  };

  // The request never got an answer: nothing but the loading text is in the drawer.
  private onLost = (): void => {
    if (this.body()?.querySelector("[data-loading]")) this.close(true);
  };

  // An answer that swapped nothing (204, redirect header) leaves the loading
  // text up: this request's end closes it, another request's does not.
  private onDone = (e: Event): void => {
    const d = (e as CustomEvent<{ target?: Element; xhr?: object; successful?: boolean }>).detail;
    if (d?.xhr && this.saves.delete(d.xhr) && d.successful) this.dirty = false;
    if (d?.target === this.body()) this.onLost();
  };

  private onClick = (e: Event): void => {
    const el = e.target as Element;
    if (el.closest("[data-expand]")) return this.expand(!this.hasAttribute("full"), false);
    if (el.closest("[data-close], [data-backdrop]")) return this.close();
    const ask = this.querySelector<HTMLDialogElement>("dialog[data-discard]");
    if (el.closest("[data-keep]")) ask?.close();
    else if (el.closest("[data-discard-yes]")) {
      ask?.close();
      this.close(true);
    }
  };

  // "full" outlives the body, so the next drawer opens full too; every
  // settle (a new drawer, a tab, an action's answer) tells the fresh
  // header's button. Full fills the shell under the top bar, whose height
  // varies (env band, wrapped crumbs), so it is measured here.
  // ponytail: measured per toggle and settle, not on resize; add a
  // ResizeObserver if a wrapping top bar ever overlaps.
  private expand(on: boolean, byLink = false): void {
    this.fullByLink = byLink;
    this.toggleAttribute("full", on);
    const top = document.getElementById("shell-header")?.getBoundingClientRect().bottom ?? 0;
    this.style.setProperty("--shell-top", `${top}px`);
    const label = on ? "Exit full screen" : "Full screen";
    const btn = this.querySelector<HTMLElement>("[data-expand]");
    btn?.setAttribute("aria-pressed", String(on));
    btn?.setAttribute("aria-label", label);
    if (btn) btn.title = label;
  }

  private onSettle = (): void => this.expand(this.hasAttribute("full"), this.fullByLink);

  // Only a form's own fields count: not a confirm's typed word, not the
  // log pane's filters, not the vars grid's staging forms (their pending
  // footer says so in close()).
  private onEdit = (e: Event): void => {
    const el = e.target as Element;
    const form = el.closest("form");
    if (form && !el.closest("dialog") && !el.closest("[data-hold]") && this.body()?.contains(el)) {
      this.edited.add(form);
      this.dirty = true;
    }
  };

  // An error aimed into the open drawer is the drawer's to show, so the
  // toast stands down (defaultPrevented). A 404 means what it showed is
  // gone (a deleted tile's refresh after its job): close, quietly.
  private onError = (e: Event): void => {
    const d = (e as CustomEvent<Failed>).detail;
    const body = this.body();
    if (!this.hasAttribute("open") || !body || !d?.target || !body.contains(d.target)) return;
    e.preventDefault();
    if (d.xhr?.status === 404) return this.close(true);
    body.querySelector("[data-loading]")?.remove();
    body.querySelector("[data-request-error]")?.remove();
    const p = document.createElement("p");
    p.className = "banner banner-danger";
    p.setAttribute("role", "alert");
    p.dataset.requestError = "";
    const id = d.xhr?.getResponseHeader("X-Request-ID");
    p.textContent = "Something went wrong. Try again." + (id ? ` Request id: ${id}` : "");
    (body.querySelector("#drawer-view")?.lastElementChild ?? body).prepend(p);
  };

  // A native <dialog> open anywhere takes Escape for itself.
  private onKey = (e: KeyboardEvent): void => {
    if (!this.hasAttribute("open") || document.querySelector("dialog[open]")) return;
    if (e.key === "Escape") this.close();
    if (e.key !== "Tab") return;
    const all = [...this.querySelectorAll<HTMLElement>(FOCUSABLE)].filter((el) => el.offsetParent !== null);
    if (!all.length) return;
    const first = all[0], last = all[all.length - 1], at = document.activeElement;
    if (e.shiftKey ? at === first || !this.contains(at) : at === last || !this.contains(at)) {
      e.preventDefault();
      (e.shiftKey ? last : first).focus();
    }
  };

  connectedCallback(): void {
    document.addEventListener("htmx:beforeRequest", this.onBefore);
    document.addEventListener("htmx:sendError", this.onLost);
    document.addEventListener("htmx:timeout", this.onLost);
    document.addEventListener("htmx:afterRequest", this.onDone);
    document.addEventListener("htmx:afterSwap", this.onSwap);
    document.addEventListener("htmx:afterSettle", this.onSettle);
    document.addEventListener("htmx:responseError", this.onError, true);
    document.addEventListener("keydown", this.onKey);
    this.addEventListener("click", this.onClick);
    this.addEventListener("input", this.onEdit);
    this.addEventListener("change", this.onEdit);
  }

  disconnectedCallback(): void {
    document.removeEventListener("htmx:beforeRequest", this.onBefore);
    document.removeEventListener("htmx:sendError", this.onLost);
    document.removeEventListener("htmx:timeout", this.onLost);
    document.removeEventListener("htmx:afterRequest", this.onDone);
    document.removeEventListener("htmx:afterSwap", this.onSwap);
    document.removeEventListener("htmx:afterSettle", this.onSettle);
    document.removeEventListener("htmx:responseError", this.onError, true);
    document.removeEventListener("keydown", this.onKey);
    this.removeEventListener("click", this.onClick);
    this.removeEventListener("input", this.onEdit);
    this.removeEventListener("change", this.onEdit);
  }

  private body(): HTMLElement | null {
    return this.querySelector<HTMLElement>("#drawer-body");
  }

  // force skips the unsaved-edits question (the body is already gone).
  private close(force = false): void {
    if (!this.hasAttribute("open")) return;
    if ((this.dirty || this.querySelector("#drawer-body form[data-hold][id]")) && !force) {
      const ask = this.querySelector<HTMLDialogElement>("dialog[data-discard]");
      if (ask) return void (ask.open || ask.showModal());
    }
    this.dirty = false;
    this.querySelector<HTMLDialogElement>("dialog[data-discard]")?.close();
    if (this.fullByLink) this.expand(false); // the next canvas drawer opens at its own width
    this.removeAttribute("open");
    this.removeAttribute("tab");
    this.body()?.replaceChildren();
    const url = new URL(location.href);
    url.searchParams.delete("drawer");
    url.searchParams.delete("tab");
    url.searchParams.delete("open");
    history.replaceState(history.state, "", url);
    this.dispatchEvent(new CustomEvent("drawer-closed", { bubbles: true }));
    if (this.opener?.isConnected) this.opener.focus();
    this.opener = null;
  }
}

customElements.define("side-drawer", SideDrawer);
