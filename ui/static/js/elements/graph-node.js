const [GRID, GAP, RINGS] = [22, 12, 12];
class GraphNode extends HTMLElement {
    static observedAttributes = ["x", "y", "w", "h"];
    drag = null;
    swallow = false;
    attributeChangedCallback() {
        if (this.hasAttribute("x"))
            this.style.setProperty("--x", `${this.num("x")}px`);
        if (this.hasAttribute("y"))
            this.style.setProperty("--y", `${this.num("y")}px`);
        if (this.hasAttribute("w"))
            this.style.width = `${this.num("w")}px`;
        if (this.hasAttribute("h"))
            this.style.height = `${this.num("h")}px`;
    }
    connectedCallback() {
        this.attributeChangedCallback();
        this.addEventListener("pointerdown", this.onDown);
        this.addEventListener("click", this.onClick, true);
        this.addEventListener("dragstart", this.onNativeDrag);
        this.addEventListener("keydown", this.onKey);
    }
    disconnectedCallback() {
        this.end();
        this.removeEventListener("pointerdown", this.onDown);
        this.removeEventListener("click", this.onClick, true);
        this.removeEventListener("dragstart", this.onNativeDrag);
        this.removeEventListener("keydown", this.onKey);
    }
    num = (name) => Number(this.getAttribute(name)) || 0;
    canvas = () => this.closest("graph-canvas");
    onNativeDrag = (e) => e.preventDefault();
    onKey = (e) => {
        const t = e.target;
        if ((e.key !== "Enter" && e.key !== " ") || t.getAttribute("role") !== "button" || t.closest("graph-node") !== this)
            return;
        e.preventDefault();
        t.click();
    };
    onClick = (e) => {
        if (!this.swallow)
            return;
        this.swallow = false;
        e.preventDefault();
        e.stopImmediatePropagation();
    };
    onDown = (e) => {
        this.swallow = false;
        const canvas = this.canvas();
        if (e.button !== 0 || e.shiftKey || this.hasAttribute("static") || this.drag || this.parentElement !== canvas)
            return;
        const group = this.hasAttribute("selected") && canvas
            ? [...canvas.querySelectorAll(":scope > graph-node[selected]:not([static])")]
            : [this];
        this.drag = {
            id: e.pointerId, sx: e.clientX, sy: e.clientY, moved: false,
            slop: e.pointerType === "touch" ? 10 : 3,
            scale: this.getBoundingClientRect().width / (this.offsetWidth || 1) || 1,
            start: group.map((n) => [n, n.num("x"), n.num("y")]),
        };
        this.wire(true);
    };
    wire(on) {
        const pairs = [
            ["pointermove", this.onMove], ["pointerup", this.onUp], ["pointercancel", this.onCancel], ["pointerdown", this.onSecond],
        ];
        for (const [name, fn] of pairs)
            (on ? window.addEventListener : window.removeEventListener).call(window, name, fn);
    }
    onMove = (e) => {
        const d = this.drag;
        if (!d || e.pointerId !== d.id)
            return;
        const [dx, dy] = [e.clientX - d.sx, e.clientY - d.sy];
        if (!d.moved && Math.hypot(dx, dy) < d.slop)
            return;
        d.moved = true;
        this.setAttribute("dragging", "");
        for (const [n, x, y] of d.start)
            n.moveTo(x + dx / d.scale, y + dy / d.scale);
    };
    onUp = (e) => {
        const d = this.drag;
        if (!d || e.pointerId !== d.id)
            return;
        this.end();
        if (!d.moved)
            return;
        this.swallow = true;
        if (d.start.length === 1 && this.canvas()?.hasAttribute("nooverlap"))
            this.nudge();
        for (const [n] of d.start)
            n.commit();
    };
    onSecond = (e) => {
        if (this.drag && e.pointerId !== this.drag.id)
            this.onCancel();
    };
    onCancel = () => {
        const start = this.drag?.start ?? [];
        this.end();
        for (const [n, x, y] of start)
            n.moveTo(x, y);
    };
    end() {
        this.drag = null;
        this.removeAttribute("dragging");
        this.wire(false);
    }
    moveTo(x, y) {
        if (this.canvas()?.hasAttribute("snap"))
            [x, y] = [Math.round(x / GRID) * GRID, Math.round(y / GRID) * GRID];
        const wall = this.canvas()?.getAttribute("divider");
        if (wall)
            x = this.hasAttribute("system") ? Math.min(x, Number(wall) - this.num("w")) : Math.max(x, Number(wall));
        this.setAttribute("x", String(Math.round(x)));
        this.setAttribute("y", String(Math.round(y)));
    }
    nudge() {
        const [x0, y0] = [this.num("x"), this.num("y")];
        const others = [...(this.parentElement?.children ?? [])].filter((n) => n instanceof GraphNode && n !== this && !n.getAttribute("node-id")?.startsWith("note:"));
        for (let r = 0; r <= RINGS; r++) {
            const ring = [];
            for (let i = -r; i <= r; i++)
                for (let j = -r; j <= r; j++)
                    if (Math.max(Math.abs(i), Math.abs(j)) === r)
                        ring.push([i * GRID, j * GRID]);
            ring.sort((a, b) => Math.hypot(...a) - Math.hypot(...b));
            for (const [dx, dy] of ring) {
                this.moveTo(x0 + dx, y0 + dy);
                if (!others.some((o) => this.overlaps(o)))
                    return;
            }
        }
        this.moveTo(x0, y0);
    }
    overlaps(o) {
        const [a, b] = [this, o].map((n) => ({ x: n.num("x"), y: n.num("y"), w: n.num("w"), h: n.num("h") }));
        return a.x < b.x + b.w + GAP && b.x < a.x + a.w + GAP && a.y < b.y + b.h + GAP && b.y < a.y + a.h + GAP;
    }
    commit() {
        for (const k of ["x", "y"]) {
            const input = this.querySelector(`:scope > input[name=${k}]`);
            if (input)
                input.value = this.getAttribute(k) ?? "";
        }
        this.dispatchEvent(new CustomEvent("node-moved", { bubbles: true }));
    }
}
customElements.define("graph-node", GraphNode);
export {};
