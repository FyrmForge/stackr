// <graph-canvas scale snap straight fan-out arrows hover-focus nooverlap
// badges boundary legend focus divider>: pan, zoom (wheel, pinch,
// [data-zoom]) about the cursor, fit, shift+drag marquee, hover focus,
// "focus" centring, <svg data-edges> re-path, <svg data-lanes> traffic lanes.
// Writes --px/--py/--s on itself (CSS makes the transforms), "selected"/"lit"
// on nodes, "data-lit" on paths and lanes; fires "selection-changed"
// (detail.ids). Look attributes follow [data-look] inputs (a checkbox, or
// radios valued 1/0), kept in localStorage.
const [MIN, MAX, MARGIN, CARD_W, CARD_H, FAN_GAP] = [0.3, 2.5, 24, 220, 96, 14];
const LOOKS = ["snap", "straight", "fan-out", "arrows", "hover-focus", "nooverlap", "badges", "boundary", "legend"];

type Box = { x: number; y: number; w: number; h: number };
type Side = "l" | "r" | "t" | "b";
type End = { path: SVGPathElement; id: string; box: Box; side: Side; toward: number; offset: number };
type Pt = [number, number];

const NORMAL: Record<Side, Pt> = { l: [-1, 0], r: [1, 0], t: [0, -1], b: [0, 1] };
const OPPOSITE: Record<Side, Side> = { l: "r", r: "l", t: "b", b: "t" };
const clamp = (s: number): number => Math.min(MAX, Math.max(MIN, s));
const num = (el: Element, name: string): number => Number(el.getAttribute(name)) || 0;
const idOf = (el: Element): string => el.getAttribute("node-id") ?? "";
const across = (s: Side): boolean => s === "l" || s === "r";
const touches = (a: Box, b: Box): boolean => a.x < b.x + b.w && b.x < a.x + a.w && a.y < b.y + b.h && b.y < a.y + a.h;

// Storage can throw (private mode, blocked site data); the look then lasts
// until the next page load. Pan/zoom is per page and per tab (session).
const stored = (key: string, tab = false): string | null => { try { return (tab ? sessionStorage : localStorage).getItem(`graph.${key}`); } catch { return null; } };
const store = (key: string, v: string, tab = false): void => { try { (tab ? sessionStorage : localStorage).setItem(`graph.${key}`, v); } catch { /* not kept */ } };

class GraphCanvas extends HTMLElement {
  private s = 1;
  private px = 0;
  private py = 0;
  private pointers = new Map<number, Pt>();
  private pan: Pt | null = null;
  private marquee: { x: number; y: number; el: HTMLElement } | null = null;
  private pinch: { dist: number; mid: Pt } | null = null;
  private frame = 0;
  private observer = new MutationObserver(() => {
    cancelAnimationFrame(this.frame);
    this.frame = requestAnimationFrame(() => this.repath());
  });
  // Listeners are arrow fields, so wire(false) removes the same functions.
  private wire(on: boolean): void {
    const pairs: [EventTarget, string, (e: never) => void][] = [
      [this, "pointerdown", this.onDown], [this, "wheel", this.onWheel], [this, "click", this.onClick],
      [this, "change", this.onChange], [this, "pointerover", this.onOver], [this, "pointerleave", this.onOver],
      [window, "pointermove", this.onMove], [window, "pointerup", this.onUp], [window, "pointercancel", this.onUp],
      [window, "resize", this.onResize], [document, "keydown", this.onKey],
    ];
    for (const [t, name, fn] of pairs) (on ? t.addEventListener : t.removeEventListener).call(t, name, fn as EventListener, { passive: false });
  }
  connectedCallback(): void {
    for (const look of LOOKS) if (stored(look) !== null) this.toggleAttribute(look, stored(look) === "1");
    this.querySelectorAll<HTMLInputElement>("input[data-look]").forEach((i) => {
      i.checked = this.hasAttribute(i.dataset.look ?? "") === (i.type !== "radio" || i.value === "1");
    });
    this.wire(true);
    this.observer.observe(this, { subtree: true, childList: true, attributes: true, attributeFilter: ["x", "y", "w", "h"] });
    const [px, py, s] = (stored(`view.${location.pathname}`, true) ?? "").split(",").map(Number);
    if (s) { [this.px, this.py, this.s] = [px, py, s]; this.apply(); } else this.fit();
    this.repath();
    const focus = this.getAttribute("focus");
    if (focus) this.centre(focus);
  }
  disconnectedCallback(): void {
    this.observer.disconnect();
    cancelAnimationFrame(this.frame);
    this.wire(false);
  }
  private onResize = (): void => this.apply();
  private nodes = (): HTMLElement[] => [...this.querySelectorAll<HTMLElement>(":scope > graph-node")];
  private node = (id: string): HTMLElement | null => this.querySelector<HTMLElement>(`graph-node[node-id="${CSS.escape(id)}"]`);
  private paths = (): SVGPathElement[] => [...this.querySelectorAll<SVGPathElement>("svg[data-edges] path[data-from]")];
  private local(e: { clientX: number; clientY: number }): Pt {
    const r = this.getBoundingClientRect();
    return [e.clientX - r.left, e.clientY - r.top];
  }
  // The card a sub-tile belongs to (a card is its own).
  private top(el: Element): Element {
    while (el.parentElement?.closest("graph-node")) el = el.parentElement.closest("graph-node") as Element;
    return el;
  }
  // World box: a card's own x/y/w/h; a sub-tile measured inside its card.
  private box(el: Element): Box {
    const top = this.top(el);
    const t = { x: num(top, "x"), y: num(top, "y"), w: num(top, "w"), h: num(top, "h") };
    if (top === el) return t;
    const [r, tr] = [el.getBoundingClientRect(), top.getBoundingClientRect()];
    const k = tr.width / (t.w || 1) || 1;
    return { x: t.x + (r.left - tr.left) / k, y: t.y + (r.top - tr.top) / k, w: r.width / k, h: r.height / k };
  }
  // The cards' extent (notes and boxes left out, as v0 did).
  private bounds(): Box | null {
    const boxes = this.nodes().filter((n) => !idOf(n).startsWith("note:")).map((n) => this.box(n));
    if (boxes.length === 0) return null;
    const x = Math.min(...boxes.map((b) => b.x));
    const y = Math.min(...boxes.map((b) => b.y));
    return { x, y, w: Math.max(...boxes.map((b) => b.x + b.w)) - x, h: Math.max(...boxes.map((b) => b.y + b.h)) - y };
  }
  // A solid chunk of the content (a card's worth plus 80 px, or all of it
  // when smaller) stays on screen whichever way it pans.
  private apply(): void {
    const b = this.bounds();
    if (b) {
      const [kx, ky] = [Math.min(b.w * this.s, CARD_W * this.s + 80), Math.min(b.h * this.s, CARD_H * this.s + 80)];
      this.px = Math.max(Math.min(this.px, this.clientWidth - kx - b.x * this.s), kx - (b.x + b.w) * this.s);
      this.py = Math.max(Math.min(this.py, this.clientHeight - ky - b.y * this.s), ky - (b.y + b.h) * this.s);
    }
    this.style.setProperty("--px", `${this.px}px`);
    this.style.setProperty("--py", `${this.py}px`);
    this.style.setProperty("--s", String(this.s));
    store(`view.${location.pathname}`, `${this.px},${this.py},${this.s}`, true);
  }
  // Back to the initial scale, centred; content bigger than the screen
  // starts a margin in from the top left instead of clipped.
  private fit(): void {
    this.s = clamp(Number(this.getAttribute("scale")) || 0.8);
    const b = this.bounds();
    if (b) {
      this.px = Math.max((this.clientWidth - b.w * this.s) / 2 - b.x * this.s, MARGIN - b.x * this.s);
      this.py = Math.max((this.clientHeight - b.h * this.s) / 2 - b.y * this.s, MARGIN - b.y * this.s);
    }
    this.apply();
  }
  private zoomAt([cx, cy]: Pt, factor: number): void {
    const s = clamp(this.s * factor);
    [this.px, this.py] = [cx - ((cx - this.px) * s) / this.s, cy - ((cy - this.py) * s) / this.s];
    this.s = s;
    this.apply();
  }
  private centre(id: string): void {
    const n = this.node(id);
    if (!n) return;
    const b = this.box(n);
    [this.px, this.py] = [this.clientWidth / 2 - (b.x + b.w / 2) * this.s, this.clientHeight / 2 - (b.y + b.h / 2) * this.s];
    this.apply();
    this.light(this.top(n));
  }
  private fingers(): { dist: number; mid: Pt } {
    const [a, b] = [...this.pointers.values()];
    return { dist: Math.hypot(a[0] - b[0], a[1] - b[1]), mid: [(a[0] + b[0]) / 2, (a[1] + b[1]) / 2] };
  }
  // Empty canvas: drag pans (and drops the selection); shift (no touch)
  // marquees, on a note or box too. A second finger: pinch. Cards drag
  // themselves; controls, links and the View panel keep their press.
  private onDown = (e: PointerEvent): void => {
    const p = this.local(e);
    this.pointers.set(e.pointerId, p);
    if (this.pointers.size === 2) {
      this.pinch = this.fingers();
      this.pan = null;
      return this.endMarquee();
    }
    const t = e.target as Element;
    if (e.button !== 0 || this.pointers.size > 2 || t.closest("a, button, input, label, select, textarea, [data-zoom], [popover]")) return;
    const card = t.closest("graph-node");
    const note = card !== null && idOf(this.top(card)).startsWith("note:");
    if (e.shiftKey && e.pointerType !== "touch" && (!card || note)) {
      const el = document.createElement("div");
      el.setAttribute("data-marquee", "");
      this.append(el);
      this.marquee = { x: p[0], y: p[1], el };
      this.lasso(p);
    } else if (!card) {
      this.pan = [p[0] - this.px, p[1] - this.py];
      this.select(new Set());
    }
  };
  private onMove = (e: PointerEvent): void => {
    if (!this.pointers.has(e.pointerId)) return;
    const p = this.local(e);
    this.pointers.set(e.pointerId, p);
    if (this.pinch && this.pointers.size === 2) {
      const now = this.fingers();
      this.px += now.mid[0] - this.pinch.mid[0];
      this.py += now.mid[1] - this.pinch.mid[1];
      this.zoomAt(now.mid, now.dist / (this.pinch.dist || 1));
      this.pinch = now;
    } else if (this.pan) {
      this.px = p[0] - this.pan[0];
      this.py = p[1] - this.pan[1];
      this.apply();
    } else if (this.marquee) this.lasso(p);
  };
  private onUp = (e: PointerEvent): void => {
    this.pointers.delete(e.pointerId);
    if (this.pointers.size < 2) this.pinch = null;
    this.pan = null;
    this.endMarquee();
  };
  // Draws the rectangle and selects, live, every card and note it touches
  // (intersect, not contain), measured in world units.
  private lasso([x1, y1]: Pt): void {
    const m = this.marquee;
    if (!m) return;
    const [x, y, w, h] = [Math.min(x1, m.x), Math.min(y1, m.y), Math.abs(x1 - m.x), Math.abs(y1 - m.y)];
    m.el.style.cssText = `left:${x}px;top:${y}px;width:${w}px;height:${h}px`;
    const r = { x: (x - this.px) / this.s, y: (y - this.py) / this.s, w: w / this.s, h: h / this.s };
    this.select(new Set(this.nodes().filter((n) => touches(this.box(n), r)).map(idOf)));
  }
  private endMarquee(): void {
    this.marquee?.el.remove();
    this.marquee = null;
  }
  // Fires only when the set really changed.
  private select(ids: Set<string>): void {
    const was = this.nodes().filter((n) => n.hasAttribute("selected")).map(idOf);
    for (const n of this.nodes()) n.toggleAttribute("selected", ids.has(idOf(n)));
    if (was.length === ids.size && was.every((id) => ids.has(id))) return;
    this.dispatchEvent(new CustomEvent("selection-changed", { bubbles: true, detail: { ids: [...ids] } }));
  }
  // ctrl/meta (trackpad pinch) zooms smoothly, a mouse notch (lines, or a
  // whole ±100) zooms 1.1×, anything else is a two-finger scroll: pan. The
  // View panel scrolls itself.
  private onWheel = (e: WheelEvent): void => {
    if ((e.target as Element).closest("[popover]")) return;
    e.preventDefault();
    if (e.ctrlKey || e.metaKey) return this.zoomAt(this.local(e), Math.exp(-Math.max(-20, Math.min(20, e.deltaY)) * 0.01));
    if (e.deltaMode !== 0 || (e.deltaX === 0 && Math.abs(e.deltaY) >= 100 && Number.isInteger(e.deltaY))) {
      return this.zoomAt(this.local(e), e.deltaY < 0 ? 1.1 : 1 / 1.1);
    }
    this.px -= e.deltaX;
    this.py -= e.deltaY;
    this.apply();
  };
  private onClick = (e: Event): void => {
    const zoom = (e.target as Element).closest<HTMLElement>("[data-zoom]")?.dataset.zoom;
    if (zoom === "fit") this.fit();
    else if (zoom) this.zoomAt([this.clientWidth / 2, this.clientHeight / 2], zoom === "in" ? 1.2 : 1 / 1.2);
  };
  private onChange = (e: Event): void => {
    const input = e.target as HTMLInputElement;
    const look = input.dataset.look ?? "";
    if (!LOOKS.includes(look)) return;
    const on = input.type === "radio" ? input.value === "1" : input.checked;
    this.toggleAttribute(look, on);
    store(look, on ? "1" : "0");
    if (look === "hover-focus") this.light(null);
    this.repath();
  };
  private onKey = (e: KeyboardEvent): void => {
    if (e.key === "Escape") { this.select(new Set()); this.light(null); }
  };
  // Also runs on pointerleave (target = the canvas: nothing lit). Hover
  // focus is a look (hover-focus) and off while anything is being dragged.
  private onOver = (e: PointerEvent): void => {
    if (!this.hasAttribute("hover-focus") || this.pan || this.marquee || this.querySelector("graph-node[dragging]")) return;
    const n = e.type === "pointerleave" ? null : (e.target as Element).closest("graph-node");
    this.light(n && this.top(n));
  };
  // Lights a card, its edges, their lanes and far ends; CSS dims the rest.
  private light(n: Element | null): void {
    this.toggleAttribute("focusing", n !== null);
    const family = (el: Element): string[] => [el, ...el.querySelectorAll("graph-node")].map(idOf);
    const own = new Set(n ? family(n) : []);
    const lit = new Set(own);
    const edges = [...this.paths(), ...this.querySelectorAll<SVGGElement>("svg[data-lanes] g[data-from]")];
    for (const p of edges) {
      const [from, to] = [p.dataset.from ?? "", p.dataset.to ?? ""];
      const on = own.has(from) || own.has(to);
      p.toggleAttribute("data-lit", on);
      if (on) lit.add(from).add(to);
    }
    for (const top of this.nodes()) top.toggleAttribute("lit", family(top).some((id) => lit.has(id)));
  }
  // Anchor side by run direction; with fan-out, ends on one side of one
  // card spread along it (14 px apart, within 60 % of the side) in the
  // order of their far ends. Cubic (control max(centre run/2, 60)) or
  // straight. Lanes follow their edges.
  private repath(): void {
    const ends: End[] = [];
    for (const path of this.paths()) {
      const [fromId, toId] = [path.dataset.from ?? "", path.dataset.to ?? ""];
      const [from, to] = [this.node(fromId), this.node(toId)];
      if (!from || !to) continue;
      const [a, b] = [this.box(from), this.box(to)];
      const dx = b.x + b.w / 2 - (a.x + a.w / 2);
      const dy = b.y + b.h / 2 - (a.y + a.h / 2);
      const side: Side = Math.abs(dx) >= Math.abs(dy) ? (dx > 0 ? "r" : "l") : dy > 0 ? "b" : "t";
      const far = (o: Box): number => (across(side) ? o.y + o.h / 2 : o.x + o.w / 2);
      ends.push({ path, id: fromId, box: a, side, toward: far(b), offset: 0 });
      ends.push({ path, id: toId, box: b, side: OPPOSITE[side], toward: far(a), offset: 0 });
    }
    if (this.hasAttribute("fan-out")) {
      const groups = new Map<string, End[]>();
      for (const e of ends) groups.set(`${e.id} ${e.side}`, [...(groups.get(`${e.id} ${e.side}`) ?? []), e]);
      for (const g of groups.values()) {
        if (g.length < 2) continue;
        g.sort((p, q) => p.toward - q.toward);
        const span = Math.min(FAN_GAP * (g.length - 1), 0.6 * (across(g[0].side) ? g[0].box.h : g[0].box.w));
        g.forEach((e, i) => (e.offset = -span / 2 + (i * span) / (g.length - 1)));
      }
    }
    for (let i = 0; i < ends.length; i += 2) this.draw(ends[i], ends[i + 1]);
    this.lanes();
  }
  private draw(a: End, b: End): void {
    const [[ax, ay], [bx, by]] = [this.anchor(a), this.anchor(b)];
    if (this.hasAttribute("straight")) return a.path.setAttribute("d", `M${ax},${ay} L${bx},${by}`);
    const mid = (e: End): number => (across(a.side) ? e.box.x + e.box.w / 2 : e.box.y + e.box.h / 2);
    const c = Math.max(Math.abs(mid(b) - mid(a)) / 2, 60);
    const [[anx, any], [bnx, bny]] = [NORMAL[a.side], NORMAL[b.side]];
    a.path.setAttribute("d", `M${ax},${ay} C${ax + anx * c},${ay + any * c} ${bx + bnx * c},${by + bny * c} ${bx},${by}`);
  }
  // Middle of the side, moved along it by the fan-out offset.
  private anchor({ box: { x, y, w, h }, side, offset }: End): Pt {
    const [nx, ny] = NORMAL[side];
    return [x + (w * (1 + nx)) / 2 + (ny ? offset : 0), y + (h * (1 + ny)) / 2 + (nx ? offset : 0)];
  }
  // A lane is a copy of the edge it rides (either direction), stroke and
  // all, pushed 4 px to its side of it; the rate label sits 14 px out at
  // the middle with an arrow the way the bytes go. No drawn edge: no lane.
  private lanes(): void {
    const edges = new Map(this.paths().map((p) => [`${p.dataset.from} ${p.dataset.to}`, p]));
    for (const g of this.querySelectorAll<SVGGElement>("svg[data-lanes] g[data-from]")) {
      const fwd = edges.get(`${g.dataset.from} ${g.dataset.to}`);
      const base = fwd ?? edges.get(`${g.dataset.to} ${g.dataset.from}`);
      const [lane, label, arrow] = [g.querySelector("path"), g.querySelector("text"), g.querySelector("tspan")];
      g.toggleAttribute("hidden", !base);
      if (!base || !lane || !label || !arrow) continue;
      const side = fwd ? 1 : -1;
      const len = base.getTotalLength();
      const [p0, p1, mid] = [base.getPointAtLength(0), base.getPointAtLength(len), base.getPointAtLength(len / 2)];
      const d = Math.hypot(p1.x - p0.x, p1.y - p0.y) || 1;
      const [nx, ny] = [(-(p1.y - p0.y) / d) * side, ((p1.x - p0.x) / d) * side];
      for (const a of ["d", "stroke", "stroke-opacity"]) lane.setAttribute(a, base.getAttribute(a) ?? "");
      lane.setAttribute("transform", `translate(${nx * 4},${ny * 4})`);
      label.setAttribute("x", String(mid.x + nx * 14));
      label.setAttribute("y", String(mid.y + ny * 14 + 3));
      arrow.textContent = fwd ? "→ " : "← ";
      g.toggleAttribute("data-rev", !fwd);
    }
  }
}

customElements.define("graph-canvas", GraphCanvas);
