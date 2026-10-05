// <side-drawer open tab> is in the layout once. htmx swaps a card's drawer
// into its #drawer-body; the swap opens it and copies ?tab= into "tab" (the
// URL is pushed before the swap). [data-close], [data-backdrop] and Escape
// close it: the body empties, ?drawer and ?tab leave the URL through
// history.replaceState (the one place an element touches the URL), and a
// bubbling "drawer-closed" fires. While open, Tab cycles inside the drawer
// and closing hands focus back to what opened it. An edited form asks
// before a close drops it; a failed request into the open drawer shows
// there, with its request id, instead of as a toast.
// ponytail: a close during an in-flight open reopens on arrival; add a
// request guard if that shows up.
const FOCUSABLE = 'a[href], button:not([disabled]), input:not([disabled]), select, textarea, [tabindex]:not([tabindex="-1"])';

type Failed = { target?: Element; xhr?: { status: number; getResponseHeader(name: string): string | null } };

class SideDrawer extends HTMLElement {
  private opener: HTMLElement | null = null;
  private dirty = false;

  private onSwap = (e: Event): void => {
    const target = (e as CustomEvent<{ target?: Element }>).detail?.target;
    const body = this.body();
    if (!body || !target || !body.contains(target)) return;
    this.dirty = false;
    const was = document.activeElement as HTMLElement | null;
    if (!this.hasAttribute("open") && was && !this.contains(was)) this.opener = was;
    this.setAttribute("open", "");
    const tab = new URLSearchParams(location.search).get("tab");
    if (tab) this.setAttribute("tab", tab);
    else this.removeAttribute("tab");
    this.querySelector<HTMLElement>("[data-close]")?.focus();
  };

  private onClick = (e: Event): void => {
    if ((e.target as Element).closest("[data-close], [data-backdrop]")) this.close();
  };

  // Only a form's own fields count: not a confirm's typed word, not the
  // log pane's filters.
  private onEdit = (e: Event): void => {
    const el = e.target as Element;
    if (el.closest("form") && !el.closest("dialog") && this.body()?.contains(el)) this.dirty = true;
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
    document.addEventListener("htmx:afterSwap", this.onSwap);
    document.addEventListener("htmx:responseError", this.onError, true);
    document.addEventListener("keydown", this.onKey);
    this.addEventListener("click", this.onClick);
    this.addEventListener("input", this.onEdit);
    this.addEventListener("change", this.onEdit);
  }

  disconnectedCallback(): void {
    document.removeEventListener("htmx:afterSwap", this.onSwap);
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
    if (this.dirty && !force && !window.confirm("Discard your unsaved changes?")) return;
    this.dirty = false;
    this.removeAttribute("open");
    this.removeAttribute("tab");
    this.body()?.replaceChildren();
    const url = new URL(location.href);
    url.searchParams.delete("drawer");
    url.searchParams.delete("tab");
    history.replaceState(history.state, "", url);
    this.dispatchEvent(new CustomEvent("drawer-closed", { bubbles: true }));
    if (this.opener?.isConnected) this.opener.focus();
    this.opener = null;
  }
}

customElements.define("side-drawer", SideDrawer);
