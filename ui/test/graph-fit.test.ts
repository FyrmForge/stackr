// node --test test/ (from ui/): the cluster map's fit math. The element
// file defines a custom element on load, so the DOM globals it touches are
// stubbed first.
import { test } from "node:test";
import assert from "node:assert/strict";

const g = globalThis as Record<string, unknown>;
g.HTMLElement = class {};
g.customElements = { define() {} };
const { fitView } = await import("../ts/graph-canvas.ts");

test("a small graph centres at the initial scale", () => {
  const [px, py, s] = fitView({ x: 0, y: 0, w: 100, h: 100 }, 1000, 800, 0.8);
  assert.equal(s, 0.8);
  assert.equal(px, (1000 - 80) / 2);
  assert.equal(py, (800 - 80) / 2);
});

test("a big graph zooms out until it fits, controls and legends kept clear", () => {
  const b = { x: -200, y: 100, w: 4000, h: 3000 };
  const [vw, vh, right, bottom] = [390, 700, 52, 120];
  const [px, py, s] = fitView(b, vw, vh, 0.8, right, bottom);
  assert.ok(s < 0.5);
  // every edge of the world box lands inside the clear area
  assert.ok(px + b.x * s >= 24 - 1e-9);
  assert.ok(px + (b.x + b.w) * s <= vw - 24 - right + 1e-9);
  assert.ok(py + b.y * s >= 24 - 1e-9);
  assert.ok(py + (b.y + b.h) * s <= vh - 24 - bottom + 1e-9);
});

test("the scale never drops under the absolute floor", () => {
  const [px, py, s] = fitView({ x: 10, y: 20, w: 1e6, h: 1e6 }, 400, 400, 0.8);
  assert.equal(s, 0.05);
  assert.equal(px, 24 - 10 * s); // from the top left
  assert.equal(py, 24 - 20 * s);
});
