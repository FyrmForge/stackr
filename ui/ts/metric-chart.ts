// <metric-chart measures="cpu_pct" labels="CPU" unit="pct" range="3600"
// hx-trigger="sse:points"> is one live graph. The page writes its window
// into a <script type="application/json"> child (a plain {measure: [{t, v}]}
// or a tile's {tile, replicas}); the enclosing [sse-connect] stream's
// "points" events carry each sampler round's new points in the same shape,
// which the chart appends, dropping what is older than range seconds. It
// draws one line per replica and measure (the tile's own series when no
// replica has the measure). When the stream drops and comes back it asks
// for a fresh window with a bubbling "metrics-refill". Pages only ever write
// these attributes: the chart library lives behind draw() below, the one
// place that knows it, so swapping it touches this file and the vendored
// files only. Colours come from the theme's --rw-* tokens and follow a
// theme switch.
type Sample = { t: string; v: number };
type Series = Record<string, Sample[]>;
type Body = Series | { tile: Series; replicas: Record<string, Series> };
export type Line = { label: string; points: Sample[] };
type Plot = { append(xs: number[], ys: (number | null)[][]): void; destroy(): void };

const PALETTE = ["--rw-accent", "--rw-teal", "--rw-amber", "--rw-tunnel-lan", "--rw-danger", "--rw-tunnel-net", "--rw-edge-connector", "--rw-success"];

const isTile = (b: Body): b is { tile: Series; replicas: Record<string, Series> } => "replicas" in b || "tile" in b;

// lines picks what to draw from a body: per replica when any has the measure.
export function lines(body: Body, measures: string[], labels: string[]): Line[] {
  const name = (i: number): string => labels[i] ?? measures[i];
  const reps = isTile(body) ? body.replicas ?? {} : {};
  const own: Series = isTile(body) ? body.tile ?? {} : body;
  const out: Line[] = [];
  for (const [r, s] of Object.entries(reps).sort(([a], [b]) => a.localeCompare(b))) {
    measures.forEach((m, i) => s[m]?.length && out.push({ label: measures.length > 1 ? `${r} ${name(i)}` : r, points: s[m] }));
  }
  if (out.length) return out;
  measures.forEach((m, i) => own[m]?.length && out.push({ label: name(i), points: own[m] }));
  return out;
}

// merge appends add's points to body (newer than each series' last only)
// and drops every point before cut (ms).
export function merge(body: Body, add: Body, cut: number): Body {
  const one = (dst: Series = {}, src: Series = {}): Series => {
    for (const [m, xs] of Object.entries(src)) {
      const last = dst[m]?.length ? Date.parse(dst[m][dst[m].length - 1].t) : -Infinity;
      dst[m] = (dst[m] ?? []).concat(xs.filter((p) => Date.parse(p.t) > last));
    }
    for (const m of Object.keys(dst)) dst[m] = (dst[m] ?? []).filter((p) => Date.parse(p.t) >= cut);
    return dst;
  };
  if (!isTile(body) && !isTile(add)) return one(body, add);
  const b = isTile(body) ? body : { tile: {}, replicas: {} };
  const a = isTile(add) ? add : { tile: {}, replicas: {} };
  b.tile = one(b.tile, a.tile);
  b.replicas ??= {};
  for (const r of new Set([...Object.keys(b.replicas), ...Object.keys(a.replicas ?? {})])) {
    b.replicas[r] = one(b.replicas[r], a.replicas?.[r]);
  }
  return b;
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

function human(v: number, unit: string, more = 0): string {
  for (const [n, s] of [[2 ** 30, "GB"], [2 ** 20, "MB"], [2 ** 10, "KB"]] as const) {
    if (Math.abs(v) >= n) return `${(v / n).toFixed(1 + more)} ${s}${unit}`;
  }
  return `${v.toFixed(more)} B${unit}`;
}

// fmt is a value as shown; more adds decimals (an axis whose ticks would
// otherwise read the same).
export function fmt(v: number | null, unit: string, more = 0): string {
  if (v == null) return "-";
  const d = (n: number): string => v.toFixed(n + more);
  const big = v >= 10 && !more;
  if (unit === "bytes" || unit === "bps") return human(v, unit === "bps" ? "/s" : "", more);
  if (unit === "rps") return `${big ? Math.round(v) : d(2)}/s`;
  if (unit === "s") return `${big ? Math.round(v) : d(1)} s`;
  return ({ pct: `${d(1)}%`, ratio: `${(v * 100).toFixed(1 + more)}%`, ms: `${d(1)} ms` } as Record<string, string>)[unit] ?? d(1);
}

// ticks are an axis's labels: as few decimals as keep every tick distinct.
export function ticks(vs: number[], unit: string): string[] {
  let out = vs.map((v) => fmt(v, unit));
  for (let more = 1; more <= 3 && new Set(out).size < out.length; more++) out = vs.map((v) => fmt(v, unit, more));
  return out;
}

// when is unix seconds in zone ("" = the browser's), 24-hour like <local-time>, for a tick step in
// seconds: "15:04", from 3 h "Jan 2 15:04" (the legend's), from a day "Jan 2".
export function when(ts: number, zone: string, step = 3 * 3600): string {
  const p: Record<string, string> = {};
  for (const x of new Intl.DateTimeFormat("en-US", { timeZone: zone || undefined, month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", hourCycle: "h23" }).formatToParts(new Date(ts * 1e3))) p[x.type] = x.value;
  return step >= 86400 ? `${p.month} ${p.day}` : step >= 3 * 3600 ? `${p.month} ${p.day} ${p.hour}:${p.minute}` : `${p.hour}:${p.minute}`;
}

// A theme token is "R G B" or a hex colour.
function token(name: string): string {
  const v = getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  return /^\d/.test(v) ? `rgb(${v.split(/\s+/).join(",")})` : v || "gray";
}

// ---- the chart library: uPlot, a classic script the element loads itself.
type UPlot = { setData(d: unknown, reset?: boolean): void; setSize(s: { width: number; height: number }): void; destroy(): void; scales: { x: { min?: number; max?: number } } };
declare const uPlot: { new (opts: object, data: unknown, el: HTMLElement): UPlot; tzDate(d: Date, zone: string): Date };
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

const sparse = (u: { data: (number | null)[][] }, i: number): boolean => u.data[i].filter((v) => v != null).length <= 100; // few points get a dot each (a run's one shows)

function draw(el: HTMLElement, ls: Line[], unit: string, xs: number[], ys: (number | null)[][]): Plot {
  const text = token("--rw-faint"), grid = token("--rw-border");
  const axis = { stroke: text, grid: { stroke: grid, width: 1 }, ticks: { stroke: grid, width: 1 } };
  const zone = document.body.dataset.tz ?? "", zoneName = new Intl.DateTimeFormat("en-GB", { timeZone: zone || undefined, timeZoneName: "short" }).formatToParts().find((x) => x.type === "timeZoneName")?.value ?? ""; // the viewer's zone, as <local-time> reads it
  const u = new uPlot(
    {
      width: el.clientWidth || 400, ...(zone ? { tzDate: (ts: number) => uPlot.tzDate(new Date(ts * 1e3), zone) } : {}), // "" is the browser's, uPlot's default
      height: 160,
      cursor: { drag: { x: true, y: false } },
      // one time alone spans the picked range (uPlot pads it by 1000 days)
      scales: { x: { time: true, range: (u: { data: number[][] }, min: number, max: number) => { const x = u.data[0]; return x.length && x[0] === x[x.length - 1] ? [x[0] - Number(el.getAttribute("range") ?? 3600), x[0]] : [min, max]; } }, y: { range: (_: unknown, _min: number, max: number) => [0, max > 0 ? max * 1.1 : 1] } },
      series: [{ label: `Time (${zoneName})`, value: (_: unknown, ts: number | null) => (ts == null ? "" : when(ts, zone)) }, ...ls.map((l, i) => ({ label: l.label, stroke: token(PALETTE[i % PALETTE.length]), width: 1.5, spanGaps: false, points: { show: sparse, size: 6, fill: token(PALETTE[i % PALETTE.length]) }, value: (_: unknown, v: number | null) => fmt(v, unit) }))],
      // space: px between time ticks, room for "Jan 2 15:04" (uPlot's 50 let labels touch)
      axes: [{ ...axis, space: 80, values: (_: unknown, vs: number[], _a: number, _s: number, incr: number) => vs.map((v) => when(v, zone, incr)) }, { ...axis, size: 70, values: (_: unknown, vs: number[]) => ticks(vs, unit) }],
    },
    [xs, ...ys],
    el,
  );
  const watch = new ResizeObserver(() => u.setSize({ width: el.clientWidth, height: 160 }));
  watch.observe(el);
  return {
    // append takes the whole window after new points: setData in place,
    // a drag-zoom kept (hidden lines stay hidden on their own).
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
// ---- end of the chart library.

class MetricChart extends HTMLElement {
  private body: Body = {};
  private plot: Plot | null = null;
  private shape = ""; // the line labels the plot was built for
  private stream: Element | null = null;
  private dropped = false;
  // The theme is a class on <html> or, with none, the OS's: either way the
  // resolved background tells a switch.
  private bg = "";
  private onTheme = (): void => {
    const bg = token("--rw-bg");
    if (bg === this.bg) return;
    this.bg = bg;
    this.plot?.destroy();
    this.plot = null;
    this.render();
  };
  private theme = new MutationObserver(this.onTheme);
  private os = matchMedia("(prefers-color-scheme: dark)");

  private onPoints = (e: Event): void => {
    const data = (e as CustomEvent<MessageEvent>).detail?.data;
    if (typeof data !== "string") return;
    this.body = merge(this.body, JSON.parse(data) as Body, Date.now() - Number(this.getAttribute("range") ?? 3600) * 1000);
    this.render();
  };
  private onDrop = (): void => {
    this.dropped = true;
  };
  private onOpen = (): void => {
    if (this.dropped) this.dispatchEvent(new CustomEvent("metrics-refill", { bubbles: true }));
  };

  connectedCallback(): void {
    const data = this.querySelector('script[type="application/json"]');
    if (data) this.body = JSON.parse(data.textContent || "{}") as Body;
    this.bg = token("--rw-bg");
    this.theme.observe(document.documentElement, { attributes: true, attributeFilter: ["class"] });
    this.os.addEventListener("change", this.onTheme);
    this.addEventListener("sse:points", this.onPoints);
    this.stream = this.closest("[sse-connect]");
    this.stream?.addEventListener("htmx:sseError", this.onDrop);
    this.stream?.addEventListener("htmx:sseOpen", this.onOpen);
    this.render();
  }

  disconnectedCallback(): void {
    this.theme.disconnect();
    this.os.removeEventListener("change", this.onTheme);
    this.removeEventListener("sse:points", this.onPoints);
    this.stream?.removeEventListener("htmx:sseError", this.onDrop);
    this.stream?.removeEventListener("htmx:sseOpen", this.onOpen);
    this.plot?.destroy();
    this.plot = null;
  }

  private async render(): Promise<void> {
    const words = (a: string): string[] => (this.getAttribute(a) ?? "").split(a === "labels" ? "," : /\s+/).map((s) => s.trim()).filter(Boolean);
    const ls = lines(this.body, words("measures"), words("labels"));
    if (!ls.length) {
      this.plot?.destroy();
      this.plot = null;
      return this.empty("No data yet.");
    }
    try {
      await loadLib();
    } catch {
      return this.empty("Could not load the chart.");
    }
    if (!this.isConnected) return;
    const [xs, ys] = align(ls);
    const shape = ls.map((l) => l.label).join("\n");
    if (this.plot && shape === this.shape) return this.plot.append(xs, ys);
    this.plot?.destroy();
    this.replaceChildren();
    this.shape = shape;
    this.plot = draw(this, ls, this.getAttribute("unit") ?? "", xs, ys);
  }

  private empty(msg: string): void {
    const p = document.createElement("p");
    p.className = "text-sm text-rw-faint py-6 text-center";
    p.textContent = msg;
    this.replaceChildren(p);
  }
}

customElements.define("metric-chart", MetricChart);
