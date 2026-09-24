const [MIN, MAX, MARGIN, KEEP, FAN_GAP] = [0.3, 2.5, 24, 80, 14];
const LOOKS = ["snap", "straight", "fan-out", "arrows"];
const NORMAL = { l: [-1, 0], r: [1, 0], t: [0, -1], b: [0, 1] };
const OPPOSITE = { l: "r", r: "l", t: "b", b: "t" };
const clamp = (s) => Math.min(MAX, Math.max(MIN, s));
const num = (el, name) => Number(el.getAttribute(name)) || 0;
const idOf = (el) => el.getAttribute("node-id") ?? "";
const across = (s) => s === "l" || s === "r";
const stored = (key) => { try {
    return localStorage.getItem(`graph.${key}`);
}
catch {
    return null;
} };
const store = (key, on) => { try {
    localStorage.setItem(`graph.${key}`, on ? "1" : "0");
}
catch { } };
class GraphCanvas extends HTMLElement {
    s = 1;
    px = 0;
    py = 0;
    pointers = new Map();
    pan = null;
    marquee = null;
    pinch = null;
    frame = 0;
    observer = new MutationObserver(() => {
        cancelAnimationFrame(this.frame);
        this.frame = requestAnimationFrame(() => this.repath());
    });
    wire(on) {
        const pairs = [
            [this, "pointerdown", this.onDown], [this, "wheel", this.onWheel], [this, "click", this.onClick],
            [this, "change", this.onChange], [this, "pointerover", this.onOver], [this, "pointerleave", this.onOver],
            [window, "pointermove", this.onMove], [window, "pointerup", this.onUp], [window, "pointercancel", this.onUp],
            [window, "resize", this.onResize], [document, "keydown", this.onKey],
        ];
        for (const [t, name, fn] of pairs)
            (on ? t.addEventListener : t.removeEventListener).call(t, name, fn, { passive: false });
    }
    connectedCallback() {
        for (const look of LOOKS)
            if (stored(look) !== null)
                this.toggleAttribute(look, stored(look) === "1");
        this.querySelectorAll("input[data-look]").forEach((i) => (i.checked = this.hasAttribute(i.dataset.look ?? "")));
        this.wire(true);
        this.observer.observe(this, { subtree: true, attributes: true, attributeFilter: ["x", "y", "w", "h"] });
        this.fit();
        this.repath();
        const focus = this.getAttribute("focus");
        if (focus)
            this.centre(focus);
    }
    disconnectedCallback() {
        this.observer.disconnect();
        cancelAnimationFrame(this.frame);
        this.wire(false);
    }
    onResize = () => this.apply();
    nodes = () => [...this.querySelectorAll(":scope > graph-node")];
    node = (id) => this.querySelector(`graph-node[node-id="${CSS.escape(id)}"]`);
    paths = () => [...this.querySelectorAll("svg[data-edges] path[data-from]")];
    local(e) {
        const r = this.getBoundingClientRect();
        return [e.clientX - r.left, e.clientY - r.top];
    }
    top(el) {
        while (el.parentElement?.closest("graph-node"))
            el = el.parentElement.closest("graph-node");
        return el;
    }
    box(el) {
        const top = this.top(el);
        const t = { x: num(top, "x"), y: num(top, "y"), w: num(top, "w"), h: num(top, "h") };
        if (top === el)
            return t;
        const [r, tr] = [el.getBoundingClientRect(), top.getBoundingClientRect()];
        const k = tr.width / (t.w || 1) || 1;
        return { x: t.x + (r.left - tr.left) / k, y: t.y + (r.top - tr.top) / k, w: r.width / k, h: r.height / k };
    }
    bounds() {
        const boxes = this.nodes().map((n) => this.box(n));
        if (boxes.length === 0)
            return null;
        const x = Math.min(...boxes.map((b) => b.x));
        const y = Math.min(...boxes.map((b) => b.y));
        return { x, y, w: Math.max(...boxes.map((b) => b.x + b.w)) - x, h: Math.max(...boxes.map((b) => b.y + b.h)) - y };
    }
    apply() {
        const b = this.bounds();
        if (b) {
            this.px = Math.min(Math.max(this.px, KEEP - (b.x + b.w) * this.s), this.clientWidth - KEEP - b.x * this.s);
            this.py = Math.min(Math.max(this.py, KEEP - (b.y + b.h) * this.s), this.clientHeight - KEEP - b.y * this.s);
        }
        this.style.setProperty("--px", `${this.px}px`);
        this.style.setProperty("--py", `${this.py}px`);
        this.style.setProperty("--s", String(this.s));
    }
    fit() {
        this.s = clamp(Number(this.getAttribute("scale")) || 0.8);
        const b = this.bounds();
        if (!b)
            return this.apply();
        const [W, H] = [this.clientWidth, this.clientHeight];
        this.s = clamp(Math.min(this.s, (W - 2 * MARGIN) / (b.w || 1), (H - 2 * MARGIN) / (b.h || 1)));
        this.px = (W - b.w * this.s) / 2 - b.x * this.s;
        this.py = (H - b.h * this.s) / 2 - b.y * this.s;
        this.apply();
    }
    zoomAt([cx, cy], factor) {
        const s = clamp(this.s * factor);
        this.px = cx - ((cx - this.px) * s) / this.s;
        this.py = cy - ((cy - this.py) * s) / this.s;
        this.s = s;
        this.apply();
    }
    centre(id) {
        const n = this.node(id);
        if (!n)
            return;
        const b = this.box(n);
        this.px = this.clientWidth / 2 - (b.x + b.w / 2) * this.s;
        this.py = this.clientHeight / 2 - (b.y + b.h / 2) * this.s;
        this.apply();
        this.light(this.top(n));
    }
    fingers() {
        const [a, b] = [...this.pointers.values()];
        return { dist: Math.hypot(a[0] - b[0], a[1] - b[1]), mid: [(a[0] + b[0]) / 2, (a[1] + b[1]) / 2] };
    }
    onDown = (e) => {
        const p = this.local(e);
        this.pointers.set(e.pointerId, p);
        if (this.pointers.size === 2) {
            this.pinch = this.fingers();
            this.pan = null;
            return this.endMarquee(false);
        }
        const t = e.target;
        if (e.button !== 0 || this.pointers.size > 2 || t.closest("button, input, label, select, [data-zoom]"))
            return;
        if (e.shiftKey) {
            const el = document.createElement("div");
            el.setAttribute("data-marquee", "");
            this.append(el);
            this.marquee = { x: p[0], y: p[1], el };
        }
        else if (!t.closest("graph-node")) {
            this.pan = [p[0] - this.px, p[1] - this.py];
            this.select(new Set());
        }
    };
    onMove = (e) => {
        if (!this.pointers.has(e.pointerId))
            return;
        const p = this.local(e);
        this.pointers.set(e.pointerId, p);
        if (this.pinch && this.pointers.size === 2) {
            const now = this.fingers();
            this.px += now.mid[0] - this.pinch.mid[0];
            this.py += now.mid[1] - this.pinch.mid[1];
            this.zoomAt(now.mid, now.dist / (this.pinch.dist || 1));
            this.pinch = now;
        }
        else if (this.pan) {
            this.px = p[0] - this.pan[0];
            this.py = p[1] - this.pan[1];
            this.apply();
        }
        else if (this.marquee) {
            const m = this.marquee;
            const [x, y] = [Math.min(p[0], m.x), Math.min(p[1], m.y)];
            m.el.style.cssText = `left:${x}px;top:${y}px;width:${Math.abs(p[0] - m.x)}px;height:${Math.abs(p[1] - m.y)}px`;
        }
    };
    onUp = (e) => {
        this.pointers.delete(e.pointerId);
        if (this.pointers.size < 2)
            this.pinch = null;
        this.pan = null;
        this.endMarquee(true);
    };
    endMarquee(select) {
        const m = this.marquee;
        if (!m)
            return;
        this.marquee = null;
        const r = m.el.getBoundingClientRect();
        m.el.remove();
        if (!select)
            return;
        const hit = this.nodes().filter((n) => {
            const b = n.getBoundingClientRect();
            return b.left < r.right && r.left < b.right && b.top < r.bottom && r.top < b.bottom;
        });
        this.select(new Set(hit.map(idOf)));
    }
    select(ids) {
        const had = this.querySelector(":scope > graph-node[selected]") !== null;
        for (const n of this.nodes())
            n.toggleAttribute("selected", ids.has(idOf(n)));
        if (had || ids.size > 0)
            this.dispatchEvent(new CustomEvent("selection-changed", { bubbles: true, detail: { ids: [...ids] } }));
    }
    onWheel = (e) => {
        e.preventDefault();
        if (e.ctrlKey || e.metaKey)
            return this.zoomAt(this.local(e), Math.exp(-e.deltaY * 0.01));
        if (e.deltaMode === 1 || (e.deltaX === 0 && Math.abs(e.deltaY) >= 100 && Number.isInteger(e.deltaY))) {
            return this.zoomAt(this.local(e), 1.1 ** -Math.sign(e.deltaY));
        }
        this.px -= e.deltaX;
        this.py -= e.deltaY;
        this.apply();
    };
    onClick = (e) => {
        const zoom = e.target.closest("[data-zoom]")?.dataset.zoom;
        if (zoom === "fit")
            this.fit();
        else if (zoom)
            this.zoomAt([this.clientWidth / 2, this.clientHeight / 2], zoom === "in" ? 1.2 : 1 / 1.2);
    };
    onChange = (e) => {
        const input = e.target;
        const look = input.dataset.look ?? "";
        if (!LOOKS.includes(look))
            return;
        this.toggleAttribute(look, input.checked);
        store(look, input.checked);
        this.repath();
    };
    onKey = (e) => {
        if (e.key !== "Escape")
            return;
        this.select(new Set());
        this.light(null);
    };
    onOver = (e) => {
        if (this.pan || this.marquee || this.querySelector("graph-node[dragging]"))
            return;
        const n = e.type === "pointerleave" ? null : e.target.closest("graph-node");
        this.light(n && this.top(n));
    };
    light(n) {
        this.toggleAttribute("focusing", n !== null);
        const family = (el) => [el, ...el.querySelectorAll("graph-node")].map(idOf);
        const own = new Set(n ? family(n) : []);
        const lit = new Set(own);
        for (const p of this.paths()) {
            const [from, to] = [p.dataset.from ?? "", p.dataset.to ?? ""];
            const on = own.has(from) || own.has(to);
            p.toggleAttribute("data-lit", on);
            if (on)
                lit.add(from).add(to);
        }
        for (const top of this.nodes())
            top.toggleAttribute("lit", family(top).some((id) => lit.has(id)));
    }
    repath() {
        const ends = [];
        for (const path of this.paths()) {
            const [fromId, toId] = [path.dataset.from ?? "", path.dataset.to ?? ""];
            const [from, to] = [this.node(fromId), this.node(toId)];
            if (!from || !to)
                continue;
            const [a, b] = [this.box(from), this.box(to)];
            const dx = b.x + b.w / 2 - (a.x + a.w / 2);
            const dy = b.y + b.h / 2 - (a.y + a.h / 2);
            const side = Math.abs(dx) >= Math.abs(dy) ? (dx > 0 ? "r" : "l") : dy > 0 ? "b" : "t";
            const far = (o) => (across(side) ? o.y + o.h / 2 : o.x + o.w / 2);
            ends.push({ path, id: fromId, box: a, side, toward: far(b), offset: 0 });
            ends.push({ path, id: toId, box: b, side: OPPOSITE[side], toward: far(a), offset: 0 });
        }
        if (this.hasAttribute("fan-out")) {
            const groups = new Map();
            for (const e of ends)
                groups.set(`${e.id} ${e.side}`, [...(groups.get(`${e.id} ${e.side}`) ?? []), e]);
            for (const g of groups.values()) {
                if (g.length < 2)
                    continue;
                g.sort((p, q) => p.toward - q.toward);
                const span = Math.min(FAN_GAP * (g.length - 1), 0.6 * (across(g[0].side) ? g[0].box.h : g[0].box.w));
                g.forEach((e, i) => (e.offset = -span / 2 + (i * span) / (g.length - 1)));
            }
        }
        for (let i = 0; i < ends.length; i += 2)
            this.draw(ends[i], ends[i + 1]);
    }
    draw(a, b) {
        const [ax, ay] = this.anchor(a);
        const [bx, by] = this.anchor(b);
        if (this.hasAttribute("straight"))
            return a.path.setAttribute("d", `M${ax},${ay} L${bx},${by}`);
        const c = Math.max((across(a.side) ? Math.abs(bx - ax) : Math.abs(by - ay)) / 2, 60);
        const [[anx, any], [bnx, bny]] = [NORMAL[a.side], NORMAL[b.side]];
        a.path.setAttribute("d", `M${ax},${ay} C${ax + anx * c},${ay + any * c} ${bx + bnx * c},${by + bny * c} ${bx},${by}`);
    }
    anchor({ box: { x, y, w, h }, side, offset }) {
        const [nx, ny] = NORMAL[side];
        return [x + (w * (1 + nx)) / 2 + (ny ? offset : 0), y + (h * (1 + ny)) / 2 + (nx ? offset : 0)];
    }
}
customElements.define("graph-canvas", GraphCanvas);
export {};
