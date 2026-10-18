const POLL = 30_000;
const PALETTE = ["--rw-accent", "--rw-teal", "--rw-amber", "--rw-tunnel-lan", "--rw-danger", "--rw-tunnel-net", "--rw-edge-connector", "--rw-success"];
export function lines(body, measures, labels) {
    const name = (i) => labels[i] ?? measures[i];
    const reps = "replicas" in body && body.replicas ? body.replicas : {};
    const own = "replicas" in body ? body.tile ?? {} : body;
    const out = [];
    for (const [r, s] of Object.entries(reps).sort(([a], [b]) => a.localeCompare(b))) {
        measures.forEach((m, i) => s[m]?.length && out.push({ label: measures.length > 1 ? `${r} ${name(i)}` : r, points: s[m] }));
    }
    if (out.length)
        return out;
    measures.forEach((m, i) => own[m]?.length && out.push({ label: name(i), points: own[m] }));
    return out;
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
    }
    return String(v);
}
function token(name) {
    const v = getComputedStyle(document.documentElement).getPropertyValue(name).trim();
    return /^\d/.test(v) ? `rgb(${v.split(/\s+/).join(",")})` : v || "gray";
}
const inflight = new Map();
function load(src) {
    let p = inflight.get(src);
    if (!p) {
        p = fetch(src, { credentials: "same-origin", headers: { Accept: "application/json" } })
            .then((r) => (r.ok ? r.json() : Promise.reject(new Error(String(r.status)))))
            .finally(() => inflight.delete(src));
        inflight.set(src, p);
    }
    return p;
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
        update(nx, ny) {
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
    timer = 0;
    plot = null;
    shape = "";
    dark = document.documentElement.classList.contains("dark");
    theme = new MutationObserver(() => {
        const dark = document.documentElement.classList.contains("dark");
        if (dark === this.dark)
            return;
        this.dark = dark;
        this.rebuild();
    });
    connectedCallback() {
        this.timer = window.setInterval(() => this.refresh(), POLL);
        this.theme.observe(document.documentElement, { attributes: true, attributeFilter: ["class"] });
        this.refresh();
    }
    disconnectedCallback() {
        clearInterval(this.timer);
        this.theme.disconnect();
        this.plot?.destroy();
        this.plot = null;
    }
    rebuild() {
        this.plot?.destroy();
        this.plot = null;
        this.refresh();
    }
    async refresh() {
        const words = (a) => (this.getAttribute(a) ?? "").split(a === "labels" ? "," : /\s+/).map((s) => s.trim()).filter(Boolean);
        const unit = this.getAttribute("unit") ?? "";
        let ls;
        try {
            [ls] = await Promise.all([load(this.getAttribute("src") ?? "").then((b) => lines(b, words("measures"), words("labels"))), loadLib()]);
        }
        catch {
            if (!this.plot)
                this.empty("Could not load metrics.");
            return;
        }
        if (!this.isConnected)
            return;
        if (!ls.length) {
            this.plot?.destroy();
            this.plot = null;
            return this.empty("No data yet.");
        }
        const [xs, ys] = align(ls);
        const shape = ls.map((l) => l.label).join("\n");
        if (this.plot && shape === this.shape)
            return this.plot.update(xs, ys);
        this.plot?.destroy();
        this.replaceChildren();
        this.shape = shape;
        this.plot = draw(this, ls, unit, xs, ys);
    }
    empty(msg) {
        const p = document.createElement("p");
        p.className = "text-sm text-rw-faint py-6 text-center";
        p.textContent = msg;
        this.replaceChildren(p);
    }
}
customElements.define("metric-chart", MetricChart);
