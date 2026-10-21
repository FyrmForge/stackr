const PALETTE = ["--rw-accent", "--rw-teal", "--rw-amber", "--rw-tunnel-lan", "--rw-danger", "--rw-tunnel-net", "--rw-edge-connector", "--rw-success"];
const isTile = (b) => "replicas" in b || "tile" in b;
export function lines(body, measures, labels) {
    const name = (i) => labels[i] ?? measures[i];
    const reps = isTile(body) ? body.replicas ?? {} : {};
    const own = isTile(body) ? body.tile ?? {} : body;
    const out = [];
    for (const [r, s] of Object.entries(reps).sort(([a], [b]) => a.localeCompare(b))) {
        measures.forEach((m, i) => s[m]?.length && out.push({ label: measures.length > 1 ? `${r} ${name(i)}` : r, points: s[m] }));
    }
    if (out.length)
        return out;
    measures.forEach((m, i) => own[m]?.length && out.push({ label: name(i), points: own[m] }));
    return out;
}
export function merge(body, add, cut) {
    const one = (dst = {}, src = {}) => {
        for (const [m, xs] of Object.entries(src)) {
            const last = dst[m]?.length ? Date.parse(dst[m][dst[m].length - 1].t) : -Infinity;
            dst[m] = (dst[m] ?? []).concat(xs.filter((p) => Date.parse(p.t) > last));
        }
        for (const m of Object.keys(dst))
            dst[m] = (dst[m] ?? []).filter((p) => Date.parse(p.t) >= cut);
        return dst;
    };
    if (!isTile(body) && !isTile(add))
        return one(body, add);
    const b = isTile(body) ? body : { tile: {}, replicas: {} };
    const a = isTile(add) ? add : { tile: {}, replicas: {} };
    b.tile = one(b.tile, a.tile);
    b.replicas ??= {};
    for (const r of new Set([...Object.keys(b.replicas), ...Object.keys(a.replicas ?? {})])) {
        b.replicas[r] = one(b.replicas[r], a.replicas?.[r]);
    }
    return b;
}
export function align(ls) {
    const xs = [...new Set(ls.flatMap((l) => l.points.map((p) => Date.parse(p.t) / 1000)))].sort((a, b) => a - b);
    const at = new Map(xs.map((x, i) => [x, i]));
    const ys = ls.map((l) => {
        const y = xs.map(() => null);
        for (const p of l.points)
            y[at.get(Date.parse(p.t) / 1000)] = p.v;
        return y;
    });
    return [xs, ys];
}
function human(v, unit) {
    for (const [n, s] of [[2 ** 30, "GB"], [2 ** 20, "MB"], [2 ** 10, "KB"]]) {
        if (Math.abs(v) >= n)
            return `${(v / n).toFixed(1)} ${s}${unit}`;
    }
    return `${Math.round(v)} B${unit}`;
}
export function fmt(v, unit) {
    if (v == null)
        return "-";
    switch (unit) {
        case "pct":
            return `${v.toFixed(1)}%`;
        case "ratio":
            return `${(v * 100).toFixed(1)}%`;
        case "ms":
            return `${v.toFixed(1)} ms`;
        case "bytes":
            return human(v, "");
        case "bps":
            return human(v, "/s");
        case "rps":
            return `${v < 10 ? v.toFixed(2) : Math.round(v)}/s`;
        case "s":
            return `${Math.round(v)} s`;
    }
    return String(v);
}
function token(name) {
    const v = getComputedStyle(document.documentElement).getPropertyValue(name).trim();
    return /^\d/.test(v) ? `rgb(${v.split(/\s+/).join(",")})` : v || "gray";
}
let lib = null;
function loadLib() {
    if (typeof uPlot !== "undefined")
        return Promise.resolve();
    lib ??= new Promise((ok, fail) => {
        const css = document.createElement("link");
        css.rel = "stylesheet";
        css.href = new URL("../../css/uplot.min.css", import.meta.url).href;
        const js = document.createElement("script");
        js.src = new URL("../vendor/uplot.min.js", import.meta.url).href;
        js.onload = () => ok();
        js.onerror = () => fail(new Error("chart library did not load"));
        document.head.append(css, js);
    });
    return lib;
}
function draw(el, ls, unit, xs, ys) {
    const text = token("--rw-faint"), grid = token("--rw-border");
    const axis = { stroke: text, grid: { stroke: grid, width: 1 }, ticks: { stroke: grid, width: 1 } };
    const u = new uPlot({
        width: el.clientWidth || 400,
        height: 160,
        cursor: { drag: { x: true, y: false } },
        scales: { x: { time: true }, y: { range: (_, _min, max) => [0, max > 0 ? max * 1.1 : 1] } },
        series: [{}, ...ls.map((l, i) => ({ label: l.label, stroke: token(PALETTE[i % PALETTE.length]), width: 1.5, spanGaps: false, value: (_, v) => fmt(v, unit) }))],
        axes: [axis, { ...axis, size: 70, values: (_, vs) => vs.map((v) => fmt(v, unit)) }],
    }, [xs, ...ys], el);
    const watch = new ResizeObserver(() => u.setSize({ width: el.clientWidth, height: 160 }));
    watch.observe(el);
    return {
        append(nx, ny) {
            const { min, max } = u.scales.x;
            const zoomed = xs.length > 0 && min != null && max != null && (min > xs[0] || max < xs[xs.length - 1]);
            xs = nx;
            u.setData([nx, ...ny], !zoomed);
        },
        destroy() {
            watch.disconnect();
            u.destroy();
        },
    };
}
class MetricChart extends HTMLElement {
    body = {};
    plot = null;
    shape = "";
    stream = null;
    dropped = false;
    bg = "";
    onTheme = () => {
        const bg = token("--rw-bg");
        if (bg === this.bg)
            return;
        this.bg = bg;
        this.plot?.destroy();
        this.plot = null;
        this.render();
    };
    theme = new MutationObserver(this.onTheme);
    os = matchMedia("(prefers-color-scheme: dark)");
    onPoints = (e) => {
        const data = e.detail?.data;
        if (typeof data !== "string")
            return;
        this.body = merge(this.body, JSON.parse(data), Date.now() - Number(this.getAttribute("range") ?? 3600) * 1000);
        this.render();
    };
    onDrop = () => {
        this.dropped = true;
    };
    onOpen = () => {
        if (this.dropped)
            this.dispatchEvent(new CustomEvent("metrics-refill", { bubbles: true }));
    };
    connectedCallback() {
        const data = this.querySelector('script[type="application/json"]');
        if (data)
            this.body = JSON.parse(data.textContent || "{}");
        this.bg = token("--rw-bg");
        this.theme.observe(document.documentElement, { attributes: true, attributeFilter: ["class"] });
        this.os.addEventListener("change", this.onTheme);
        this.addEventListener("sse:points", this.onPoints);
        this.stream = this.closest("[sse-connect]");
        this.stream?.addEventListener("htmx:sseError", this.onDrop);
        this.stream?.addEventListener("htmx:sseOpen", this.onOpen);
        this.render();
    }
    disconnectedCallback() {
        this.theme.disconnect();
        this.os.removeEventListener("change", this.onTheme);
        this.removeEventListener("sse:points", this.onPoints);
        this.stream?.removeEventListener("htmx:sseError", this.onDrop);
        this.stream?.removeEventListener("htmx:sseOpen", this.onOpen);
        this.plot?.destroy();
        this.plot = null;
    }
    async render() {
        const words = (a) => (this.getAttribute(a) ?? "").split(a === "labels" ? "," : /\s+/).map((s) => s.trim()).filter(Boolean);
        const ls = lines(this.body, words("measures"), words("labels"));
        if (!ls.length) {
            this.plot?.destroy();
            this.plot = null;
            return this.empty("No data yet.");
        }
        try {
            await loadLib();
        }
        catch {
            return this.empty("Could not load the chart.");
        }
        if (!this.isConnected)
            return;
        const [xs, ys] = align(ls);
        const shape = ls.map((l) => l.label).join("\n");
        if (this.plot && shape === this.shape)
            return this.plot.append(xs, ys);
        this.plot?.destroy();
        this.replaceChildren();
        this.shape = shape;
        this.plot = draw(this, ls, this.getAttribute("unit") ?? "", xs, ys);
    }
    empty(msg) {
        const p = document.createElement("p");
        p.className = "text-sm text-rw-faint py-6 text-center";
        p.textContent = msg;
        this.replaceChildren(p);
    }
}
customElements.define("metric-chart", MetricChart);
