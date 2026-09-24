// <side-drawer open tab> is in the layout once. htmx swaps a card's drawer
// into its #drawer-body; the swap opens it and copies ?tab= into "tab" (the
// URL is pushed before the swap). [data-close], [data-backdrop] and Escape
// close it: the body empties, ?drawer and ?tab leave the URL through
// history.replaceState (the one place an element touches the URL), and a
// bubbling "drawer-closed" fires.
// ponytail: a close during an in-flight open reopens on arrival; add a
// request guard if that shows up.
class SideDrawer extends HTMLElement {
  private onSwap = (e: Event): void => {
    const target = (e as CustomEvent<{ target?: Element }>).detail?.target;
    const body = this.body();
    if (!body || !target || !body.contains(target)) return;
    this.setAttribute("open", "");
    const tab = new URLSearchParams(location.search).get("tab");
    if (tab) this.setAttribute("tab", tab);
    else this.removeAttribute("tab");
    this.querySelector<HTMLElement>("[data-close]")?.focus();
  };

  private onClick = (e: Event): void => {
    if ((e.target as Element).closest("[data-close], [data-backdrop]")) this.close();
  };

  // A native <dialog> open anywhere takes Escape for itself.
  private onKey = (e: KeyboardEvent): void => {
    if (e.key === "Escape" && !document.querySelector("dialog[open]")) this.close();
  };

  connectedCallback(): void {
    document.addEventListener("htmx:afterSwap", this.onSwap);
    document.addEventListener("keydown", this.onKey);
    this.addEventListener("click", this.onClick);
  }

  disconnectedCallback(): void {
    document.removeEventListener("htmx:afterSwap", this.onSwap);
    document.removeEventListener("keydown", this.onKey);
    this.removeEventListener("click", this.onClick);
  }

  private body(): HTMLElement | null {
    return this.querySelector<HTMLElement>("#drawer-body");
  }

  private close(): void {
    if (!this.hasAttribute("open")) return;
    this.removeAttribute("open");
    this.removeAttribute("tab");
    this.body()?.replaceChildren();
    const url = new URL(location.href);
    url.searchParams.delete("drawer");
    url.searchParams.delete("tab");
    history.replaceState(history.state, "", url);
    this.dispatchEvent(new CustomEvent("drawer-closed", { bubbles: true }));
  }
}

customElements.define("side-drawer", SideDrawer);
