// <metric-chart src="/api/v1/.../metrics?range=1h" measures="cpu_pct"
// labels="CPU" unit="pct" label="CPU"> is one live graph. It reads the
// metrics JSON (a plain {measure: [{t, v}]} or a tile's {tile, replicas}),
// draws one line per replica and measure (the tile's own series when no
// replica has the measure), and polls every 30 s. Pages only ever write these
// attributes: the chart library lives behind draw() below, the one place
// that knows it, so swapping it touches this file and the vendored files only.
// Colours come from the theme's --rw-* tokens and follow a theme switch.
type Sample = { t: string; v: number };
type Series = Record<string, Sample[]>;
type Body = Series | { tile: Series; replicas: Record<string, Series> };
export type Line = { label: string; points: Sample[] };
type Plot = { update(xs: number[], ys: (number | null)[][]): void; destroy(): void };

const POLL = 30_000;
const PALETTE = ["--rw-accent", "--rw-teal", "--rw-amber", "--rw-tunnel-lan", "--rw-danger", "--rw-tunnel-net", "--rw-edge-connector", "--rw-success"];

// lines picks what to draw from a body: per replica when any has the measure.
export function lines(body: Body, measures: string[], labels: string[]): Line[] {
  const name = (i: number): string => labels[i] ?? measures[i];
  const reps = "replicas" in body && body.replicas ? body.replicas : {};
  const own: Series = "replicas" in body ? (body.tile as Series) ?? {} : (body as Series);
  const out: Line[] = [];
  for (const [r, s] of Object.entries(reps).sort(([a], [b]) => a.localeCompare(b))) {
    measures.forEach((m, i) => s[m]?.length && out.push({ label: measures.length > 1 ? `${r} ${name(i)}` : r, points: s[m] }));
  }
  if (out.length) return out;
  measures.forEach((m, i) => own[m]?.length && out.push({ label: name(i), points: own[m] }));
  return out;
}

// align puts every line on one time axis (seconds), null where a line has no
// point.
export function align(ls: Line[]): [number[], (number | null)[][]] {
  const xs = [...new Set(ls.flatMap((l) => l.points.map((p) => Date.parse(p.t) / 1000)))].sort((a, b) => a - b);
  const at = new Map(xs.map((x, i) => [x, i]));
  const ys = ls.map((l) => {
    const y: (number | null)[] = xs.map(() => null);
    for (const p of l.points) y[at.get(Date.parse(p.t) / 1000)!] = p.v;
    return y;
  });
  return [xs, ys];
}

function human(v: number, unit: string): string {
  for (const [n, s] of [[2 ** 30, "GB"], [2 ** 20, "MB"], [2 ** 10, "KB"]] as const) {
    if (Math.abs(v) >= n) return `${(v / n).toFixed(1)} ${s}${unit}`;
  }
  return `${Math.round(v)} B${unit}`;
}

export function fmt(v: number | null, unit: string): string {
  if (v == null) return "-";
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

// A theme token is "R G B" or a hex colour.
function token(name: string): string {
  const v = getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  return /^\d/.test(v) ? `rgb(${v.split(/\s+/).join(",")})` : v || "gray";
}

// One request per src at a time: a tab's charts often share one.
const inflight = new Map<string, Promise<Body>>();
function load(src: string): Promise<Body> {
  let p = inflight.get(src);
  if (!p) {
    p = fetch(src, { credentials: "same-origin", headers: { Accept: "application/json" } })
      .then((r) => (r.ok ? r.json() : Promise.reject(new Error(String(r.status)))))
      .finally(() => inflight.delete(src));
    inflight.set(src, p);
  }
  return p;
}

// ---- the chart library: uPlot, a classic script the element loads itself.
type UPlot = { setData(d: unknown, reset?: boolean): void; setSize(s: { width: number; height: number }): void; destroy(): void; scales: { x: { min?: number; max?: number } } };
declare const uPlot: new (opts: object, data: unknown, el: HTMLElement) => UPlot;
let lib: Promise<void> | null = null;
function loadLib(): Promise<void> {
  if (typeof uPlot !== "undefined") return Promise.resolve();
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

function draw(el: HTMLElement, ls: Line[], unit: string, xs: number[], ys: (number | null)[][]): Plot {
  const text = token("--rw-faint"), grid = token("--rw-border");
  const axis = { stroke: text, grid: { stroke: grid, width: 1 }, ticks: { stroke: grid, width: 1 } };
  const u = new uPlot(
    {
      width: el.clientWidth || 400,
      height: 160,
      cursor: { drag: { x: true, y: false } },
      scales: { x: { time: true } },
      series: [{}, ...ls.map((l, i) => ({ label: l.label, stroke: token(PALETTE[i % PALETTE.length]), width: 1.5, spanGaps: false, value: (_: unknown, v: number | null) => fmt(v, unit) }))],
      axes: [axis, { ...axis, size: 70, values: (_: unknown, vs: number[]) => vs.map((v) => fmt(v, unit)) }],
    },
    [xs, ...ys],
    el,
  );
  const watch = new ResizeObserver(() => u.setSize({ width: el.clientWidth, height: 160 }));
  watch.observe(el);
  return {
    update(nx, ny) {
      // Keep a drag-zoom across a poll; otherwise follow the new data.
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
// ---- end of the chart library.

class MetricChart extends HTMLElement {
  private timer = 0;
  private plot: Plot | null = null;
  private shape = ""; // the line labels the plot was built for
  private dark = document.documentElement.classList.contains("dark");
  private theme = new MutationObserver(() => {
    const dark = document.documentElement.classList.contains("dark");
    if (dark === this.dark) return;
    this.dark = dark;
    this.rebuild();
  });

  connectedCallback(): void {
    this.timer = window.setInterval(() => this.refresh(), POLL);
    this.theme.observe(document.documentElement, { attributes: true, attributeFilter: ["class"] });
    this.refresh();
  }

  disconnectedCallback(): void {
    clearInterval(this.timer);
    this.theme.disconnect();
    this.plot?.destroy();
    this.plot = null;
  }

  private rebuild(): void {
    this.plot?.destroy();
    this.plot = null;
    this.refresh();
  }

  private async refresh(): Promise<void> {
    const words = (a: string): string[] => (this.getAttribute(a) ?? "").split(a === "labels" ? "," : /\s+/).map((s) => s.trim()).filter(Boolean);
    const unit = this.getAttribute("unit") ?? "";
    let ls: Line[];
    try {
      [ls] = await Promise.all([load(this.getAttribute("src") ?? "").then((b) => lines(b, words("measures"), words("labels"))), loadLib()]);
    } catch {
      if (!this.plot) this.empty("Could not load metrics.");
      return;
    }
    if (!this.isConnected) return;
    if (!ls.length) {
      this.plot?.destroy();
      this.plot = null;
      return this.empty("No data yet.");
    }
    const [xs, ys] = align(ls);
    const shape = ls.map((l) => l.label).join("\n");
    if (this.plot && shape === this.shape) return this.plot.update(xs, ys);
    this.plot?.destroy();
    this.replaceChildren();
    this.shape = shape;
    this.plot = draw(this, ls, unit, xs, ys);
  }

  private empty(msg: string): void {
    const p = document.createElement("p");
    p.className = "text-sm text-rw-faint py-6 text-center";
    p.textContent = msg;
    this.replaceChildren(p);
  }
}

customElements.define("metric-chart", MetricChart);
