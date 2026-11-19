// node --test test/ (from ui/): main.js restores the open release rows after
// an SSE swap, which sets no detail.target.
import { test } from "node:test";
import assert from "node:assert/strict";

type Fn = (e: unknown) => void;
const handlers: Record<string, Fn[]> = {};
const g = globalThis as Record<string, unknown>;
const noop = (): void => {};
g.document = {
  addEventListener: (n: string, f: Fn) => (handlers[n] ??= []).push(f),
  querySelector: () => null,
  querySelectorAll: () => [],
  getElementById: () => null,
  body: {},
};
g.window = g;
g.htmx = { trigger: noop };
await import("../static/js/main.js");

test("open release rows reopen after an SSE swap", () => {
  const row = { open: false };
  const body: Record<string, unknown> = {
    id: "rel-body",
    querySelectorAll: () => [{ dataset: { id: "r1" } }],
    querySelector: (sel: string) => (sel === 'details[data-id="r1"]' ? row : null),
  };
  body.closest = (sel: string) => (sel === "#rel-body" ? body : null);
  handlers["htmx:sseBeforeMessage"].forEach((f) => f({ target: body }));
  handlers["htmx:afterSwap"].forEach((f) => f({ target: body, detail: {} }));
  assert.equal(row.open, true);
});
