// <tab-strip> makes a horizontally overflowing tab row reachable without a
// touchpad: the mouse wheel scrolls it sideways, [data-prev]/[data-next]
// buttons show only when there is more in that direction and page the row,
// and the [aria-current] tab is scrolled into view on connect (the drawer
// re-renders on every tab change, so that covers changes too).
class TabStrip extends HTMLElement {
  private row(): HTMLElement | null {
    return this.querySelector<HTMLElement>("[data-row]");
  }

  private update = (): void => {
    const row = this.row();
    if (!row) return;
    const max = row.scrollWidth - row.clientWidth;
    this.querySelector("[data-prev]")?.toggleAttribute("hidden", row.scrollLeft <= 1);
    this.querySelector("[data-next]")?.toggleAttribute("hidden", row.scrollLeft >= max - 1);
  };

  private onWheel = (e: WheelEvent): void => {
    const row = this.row();
    if (!row || row.scrollWidth <= row.clientWidth) return;
    if (Math.abs(e.deltaY) <= Math.abs(e.deltaX)) return; // touchpads already scroll sideways
    e.preventDefault();
    row.scrollLeft += e.deltaY;
  };

  private onClick = (e: Event): void => {
    const button = (e.target as Element).closest("[data-prev], [data-next]");
    const row = this.row();
    if (!button || !row) return;
    const page = row.clientWidth * 0.8;
    row.scrollBy({ left: button.hasAttribute("data-prev") ? -page : page, behavior: "smooth" });
  };

  private resize = new ResizeObserver(this.update);

  connectedCallback(): void {
    const row = this.row();
    if (!row) return;
    row.querySelector("[aria-current]")?.scrollIntoView({ block: "nearest", inline: "nearest" });
    row.addEventListener("scroll", this.update, { passive: true });
    this.addEventListener("wheel", this.onWheel, { passive: false });
    this.addEventListener("click", this.onClick);
    this.resize.observe(row);
    this.update();
  }

  disconnectedCallback(): void {
    this.row()?.removeEventListener("scroll", this.update);
    this.removeEventListener("wheel", this.onWheel);
    this.removeEventListener("click", this.onClick);
    this.resize.disconnect();
  }
}

customElements.define("tab-strip", TabStrip);
