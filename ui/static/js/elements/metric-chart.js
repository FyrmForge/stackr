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
function human(v, unit, more = 0) {
    for (const [n, s] of [[2 ** 30, "GB"], [2 ** 20, "MB"], [2 ** 10, "KB"]]) {
        if (Math.abs(v) >= n)
            return `${(v / n).toFixed(1 + more)} ${s}${unit}`;
    }
    return `${v.toFixed(more)} B${unit}`;
}
export function fmt(v, unit, more = 0) {
    if (v == null)
        return "-";
    const d = (n) => v.toFixed(n + more);
    const big = v >= 10 && !more;
    if (unit === "bytes" || unit === "bps")
        return human(v, unit === "bps" ? "/s" : "", more);
    if (unit === "rps")
        return `${big ? Math.round(v) : d(2)}/s`;
    if (unit === "s")
        return `${big ? Math.round(v) : d(1)} s`;
    return { pct: `${d(1)}%`, ratio: `${(v * 100).toFixed(1 + more)}%`, ms: `${d(1)} ms` }[unit] ?? d(1);
}
export function ticks(vs, unit) {
    let out = vs.map((v) => fmt(v, unit));
    const lean = out.map((s) => s.replace(".0 ", " "));
    if ((unit === "bytes" || unit === "bps") && new Set(lean).size === lean.length)
        return lean;
    for (let more = 1; more <= 3 && new Set(out).size < out.length; more++)
        out = vs.map((v) => fmt(v, unit, more));
    return out;
}
export function yScale(max, unit) {
    const hi = max > 0 ? max : 1, bin = unit === "bytes" || unit === "bps";
    const base = (bin && [2 ** 30, 2 ** 20, 2 ** 10].find((b) => hi / 4 >= b)) || 1, r = hi / 4 / base, e = 10 ** Math.floor(Math.log10(r));
    let step = [1, 2, 5, 10].find((m) => m * e >= r * (1 - 1e-9)) * e * base;
    if (bin && step >= 1000 * base)
        step = 1024 * base;
    return { step, top: Math.ceil(hi / step - 1e-9) * step };
}
export function when(ts, zone, step = 3 * 3600) {
    const p = {};
    for (const x of new Intl.DateTimeFormat("en-US", { timeZone: zone || undefined, month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", hourCycle: "h23" }).formatToParts(new Date(ts * 1e3)))
        p[x.type] = x.value;
    return step >= 86400 ? `${p.month} ${p.day}` : step >= 3 * 3600 ? `${p.month} ${p.day} ${p.hour}:${p.minute}` : `${p.hour}:${p.minute}`;
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
const sparse = (u, i) => u.data[i].filter((v) => v != null).length <= 100;
function draw(el, ls, unit, xs, ys) {
    const text = token("--rw-faint"), grid = token("--rw-border");
    const axis = { stroke: text, grid: { stroke: grid, width: 1 }, ticks: { stroke: grid, width: 1 } };
    let y = yScale(0, unit);
    const zone = document.body.dataset.tz ?? "", zoneName = new Intl.DateTimeFormat("en-GB", { timeZone: zone || undefined, timeZoneName: "short" }).formatToParts().find((x) => x.type === "timeZoneName")?.value ?? "";
    const u = new uPlot({
        width: el.clientWidth || 400, ...(zone ? { tzDate: (ts) => uPlot.tzDate(new Date(ts * 1e3), zone) } : {}),
        height: 160,
        cursor: { drag: { x: true, y: false } },
        scales: { x: { time: true, range: (u, min, max) => { const x = u.data[0]; return x.length && x[0] === x[x.length - 1] ? [x[0] - Number(el.getAttribute("range") ?? 3600), x[0]] : [min, max]; } }, y: { range: (_, _min, max) => { y = yScale(max, unit); return [0, y.top]; } } },
        series: [{ label: `Time (${zoneName})`, value: (_, ts) => (ts == null ? "" : when(ts, zone)) }, ...ls.map((l, i) => ({ label: l.label, stroke: token(PALETTE[i % PALETTE.length]), width: 1.5, spanGaps: false, points: { show: sparse, size: 6, fill: token(PALETTE[i % PALETTE.length]) }, value: (_, v) => fmt(v, unit) }))],
        axes: [{ ...axis, space: 80, values: (_, vs, _a, _s, incr) => vs.map((v) => when(v, zone, incr)) }, { ...axis, size: 70, splits: () => Array.from({ length: Math.round(y.top / y.step) + 1 }, (_, i) => i * y.step), values: (_, vs) => ticks(vs, unit) }],
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
