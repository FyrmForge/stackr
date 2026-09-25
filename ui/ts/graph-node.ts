// <graph-node node-id x y w h [system] [static]> places one card from
// x/y/w/h (CSS vars). Drag: slop, 22 px grid under a "snap" canvas,
// "system" cards stay left of the canvas "divider" and the rest right,
// a lone drop is nudged off other cards; then it writes its hidden x/y
// inputs and fires a bubbling "node-moved". A "selected" card drags every
// selected card, each fires its own. Static sub-tiles ride along inside.
// Enter or Space on a card's [role=button] body clicks it.
const [GRID, GAP, RINGS] = [22, 8, 12];

type Start = [GraphNode, number, number];

class GraphNode extends HTMLElement {
  static observedAttributes = ["x", "y", "w", "h"];
  private drag: { id: number; sx: number; sy: number; slop: number; scale: number; moved: boolean; start: Start[] } | null = null;
  private swallow = false;
  attributeChangedCallback(): void {
    if (this.hasAttribute("x")) this.style.setProperty("--x", `${this.num("x")}px`);
    if (this.hasAttribute("y")) this.style.setProperty("--y", `${this.num("y")}px`);
    if (this.hasAttribute("w")) this.style.width = `${this.num("w")}px`;
    if (this.hasAttribute("h")) this.style.height = `${this.num("h")}px`;
  }
  connectedCallback(): void {
    this.attributeChangedCallback();
    this.addEventListener("pointerdown", this.onDown);
    this.addEventListener("click", this.onClick, true);
    this.addEventListener("dragstart", this.onNativeDrag);
    this.addEventListener("keydown", this.onKey);
  }
  disconnectedCallback(): void {
    this.end();
    this.removeEventListener("pointerdown", this.onDown);
    this.removeEventListener("click", this.onClick, true);
    this.removeEventListener("dragstart", this.onNativeDrag);
    this.removeEventListener("keydown", this.onKey);
  }
  private num = (name: string): number => Number(this.getAttribute(name)) || 0;
  private canvas = (): Element | null => this.closest("graph-canvas");
  // Links and images inside a card must not start a native drag.
  private onNativeDrag = (e: Event): void => e.preventDefault();
  // A sub-tile's own graph-node answers its key; its card's lets it pass.
  private onKey = (e: KeyboardEvent): void => {
    const t = e.target as HTMLElement;
    if ((e.key !== "Enter" && e.key !== " ") || t.getAttribute("role") !== "button" || t.closest("graph-node") !== this) return;
    e.preventDefault();
    t.click();
  };
  // The click that ends a drag never reaches the card's hx-get or <a>.
  private onClick = (e: Event): void => {
    if (!this.swallow) return;
    this.swallow = false;
    e.preventDefault();
    e.stopImmediatePropagation();
  };
  // A sub-tile (static) lets the press bubble to its card, which drags.
  private onDown = (e: PointerEvent): void => {
    this.swallow = false;
    const canvas = this.canvas();
    if (e.button !== 0 || e.shiftKey || this.hasAttribute("static") || this.drag || this.parentElement !== canvas) return;
    const group = this.hasAttribute("selected") && canvas
      ? [...canvas.querySelectorAll<GraphNode>(":scope > graph-node[selected]:not([static])")]
      : [this];
    this.drag = {
      id: e.pointerId, sx: e.clientX, sy: e.clientY, moved: false,
      slop: e.pointerType === "touch" ? 10 : 3,
      scale: this.getBoundingClientRect().width / (this.offsetWidth || 1) || 1,
      start: group.map((n): Start => [n, n.num("x"), n.num("y")]),
    };
    this.wire(true);
  };
  private wire(on: boolean): void {
    const pairs: [string, (e: PointerEvent) => void][] = [
      ["pointermove", this.onMove], ["pointerup", this.onUp], ["pointercancel", this.onCancel], ["pointerdown", this.onSecond],
    ];
    for (const [name, fn] of pairs) (on ? window.addEventListener : window.removeEventListener).call(window, name, fn as EventListener);
  }
  private onMove = (e: PointerEvent): void => {
    const d = this.drag;
    if (!d || e.pointerId !== d.id) return;
    const [dx, dy] = [e.clientX - d.sx, e.clientY - d.sy];
    if (!d.moved && Math.hypot(dx, dy) < d.slop) return;
    d.moved = true;
    this.setAttribute("dragging", "");
    for (const [n, x, y] of d.start) n.moveTo(x + dx / d.scale, y + dy / d.scale);
  };
  private onUp = (e: PointerEvent): void => {
    const d = this.drag;
    if (!d || e.pointerId !== d.id) return;
    this.end();
    if (!d.moved) return;
    this.swallow = true;
    if (d.start.length === 1) this.nudge();
    for (const [n] of d.start) n.commit();
  };
  // A second finger means a pinch: put the cards back, the canvas zooms.
  private onSecond = (e: PointerEvent): void => {
    if (this.drag && e.pointerId !== this.drag.id) this.onCancel();
  };
  private onCancel = (): void => {
    const start = this.drag?.start ?? [];
    this.end();
    for (const [n, x, y] of start) n.moveTo(x, y);
  };
  private end(): void {
    this.drag = null;
    this.removeAttribute("dragging");
    this.wire(false);
  }
  private moveTo(x: number, y: number): void {
    if (this.canvas()?.hasAttribute("snap")) [x, y] = [Math.round(x / GRID) * GRID, Math.round(y / GRID) * GRID];
    const wall = this.canvas()?.getAttribute("divider");
    if (wall) x = this.hasAttribute("system") ? Math.min(x, Number(wall) - this.num("w")) : Math.max(x, Number(wall));
    this.setAttribute("x", String(Math.round(x)));
    this.setAttribute("y", String(Math.round(y)));
  }
  // Nearest free spot on rings one grid step apart, up to 12 rings out.
  // ponytail: boxes are w×h only; sub-tiles hang below and may overlap.
  private nudge(): void {
    const [x0, y0] = [this.num("x"), this.num("y")];
    const others = [...(this.parentElement?.children ?? [])].filter((n): n is GraphNode => n instanceof GraphNode && n !== this);
    for (let r = 0; r <= RINGS; r++) {
      const ring: [number, number][] = [];
      for (let i = -r; i <= r; i++) for (let j = -r; j <= r; j++) if (Math.max(Math.abs(i), Math.abs(j)) === r) ring.push([i * GRID, j * GRID]);
      ring.sort((a, b) => Math.hypot(...a) - Math.hypot(...b));
      for (const [dx, dy] of ring) {
        this.moveTo(x0 + dx, y0 + dy);
        if (!others.some((o) => this.overlaps(o))) return;
      }
    }
    this.moveTo(x0, y0);
  }
  private overlaps(o: GraphNode): boolean {
    const [a, b] = [this, o].map((n) => ({ x: n.num("x"), y: n.num("y"), w: n.num("w"), h: n.num("h") }));
    return a.x < b.x + b.w + GAP && b.x < a.x + a.w + GAP && a.y < b.y + b.h + GAP && b.y < a.y + a.h + GAP;
  }
  private commit(): void {
    for (const k of ["x", "y"]) {
      const input = this.querySelector<HTMLInputElement>(`:scope > input[name=${k}]`);
      if (input) input.value = this.getAttribute(k) ?? "";
    }
    this.dispatchEvent(new CustomEvent("node-moved", { bubbles: true }));
  }
}

customElements.define("graph-node", GraphNode);
