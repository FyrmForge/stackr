// node --test test/ (from ui/): what <metric-chart> draws from a metrics
// body. DOM globals are stubbed so the element file loads.
import { test } from "node:test";
import assert from "node:assert/strict";

const g = globalThis as Record<string, unknown>;
g.HTMLElement = class {};
g.customElements = { define() {} };
const { lines, align, fmt, merge, ticks } = await import("../ts/metric-chart.ts");

const at = (s: number, v: number) => ({ t: new Date(s * 1000).toISOString(), v });

test("a round appends its new points, drops what is out of range, keeps the rest", () => {
  const node = { cpu_pct: [at(0, 1), at(30, 2)], mem_used: [at(0, 5)] };
  const got = merge(node, { cpu_pct: [at(30, 9), at(60, 3)] }, 10_000) as Record<string, { v: number }[]>;
  assert.deepEqual(got.cpu_pct.map((p) => p.v), [2, 3]); // 0 s trimmed, 30 s not doubled
  assert.deepEqual(got.mem_used, []);
  const tile = { tile: { mem: [at(0, 1)] }, replicas: { "a-1": { mem: [at(0, 1)] } } };
  const t = merge(tile, { tile: { mem: [at(30, 2)] }, replicas: { "a-1": { mem: [at(30, 3)] }, "b-1": { mem: [at(30, 4)] } } }, 0) as {
    tile: Record<string, { v: number }[]>;
    replicas: Record<string, Record<string, { v: number }[]>>;
  };
  assert.deepEqual(t.tile.mem.map((p) => p.v), [1, 2]);
  assert.deepEqual(t.replicas["a-1"].mem.map((p) => p.v), [1, 3]);
  assert.deepEqual(t.replicas["b-1"].mem.map((p) => p.v), [4]);
});

test("a tile body draws one line per replica, its total when no replica has the measure", () => {
  const body = { tile: { cpu_pct: [at(0, 3)], net_rx_bps: [at(0, 9)] }, replicas: { "web-2": { cpu_pct: [at(0, 2)] }, "web-1": { cpu_pct: [at(0, 1)] } } };
  assert.deepEqual(lines(body, ["cpu_pct"], []).map((l) => l.label), ["web-1", "web-2"]);
  assert.deepEqual(lines(body, ["net_rx_bps", "net_tx_bps"], ["in", "out"]).map((l) => l.label), ["in"]);
  assert.deepEqual(lines({ rtt_ms: [at(0, 4)] }, ["rtt_ms"], ["round trip"]).map((l) => l.label), ["round trip"]);
  assert.deepEqual(lines({}, ["cpu_pct"], []), []);
});

test("lines share one time axis, gaps are null", () => {
  const [xs, ys] = align([{ label: "a", points: [at(0, 1), at(30, 2)] }, { label: "b", points: [at(30, 5), at(60, 6)] }]);
  assert.deepEqual(xs, [0, 30, 60]);
  assert.deepEqual(ys, [[1, 2, null], [null, 5, 6]]);
});

test("values read in their unit", () => {
  assert.equal(fmt(12.345, "pct"), "12.3%");
  assert.equal(fmt(0.25, "ratio"), "25.0%");
  assert.equal(fmt(1536, "bytes"), "1.5 KB");
  assert.equal(fmt(3 * 2 ** 20, "bps"), "3.0 MB/s");
  assert.equal(fmt(null, "ms"), "-");
  assert.equal(fmt(0.5, "rps"), "0.50/s");
  assert.equal(fmt(120.4, "rps"), "120/s");
  assert.equal(fmt(42.6, "s"), "43 s");
  assert.equal(fmt(0.42, "s"), "0.4 s");
  // an axis never repeats a label: 0.05% and 0.1% both read 0.1% at one decimal
  assert.deepEqual(ticks([0, 0.05, 0.1], "pct"), ["0.00%", "0.05%", "0.10%"]);
  assert.deepEqual(ticks([0, 0.5, 1], "bps"), ["0.0 B/s", "0.5 B/s", "1.0 B/s"]);
  assert.deepEqual(ticks([0, 20, 40], "s"), ["0.0 s", "20 s", "40 s"]);
});
