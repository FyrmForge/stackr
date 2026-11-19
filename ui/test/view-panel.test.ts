// node --test test/ (from ui/): main.js keeps the canvas View panel open
// across an htmx swap that replaces it.
import { test } from "node:test";
import assert from "node:assert/strict";

type Fn = (e: unknown) => void;
const handlers: Record<string, Fn[]> = {};
const g = globalThis as Record<string, unknown>;
let panel: { isOpen: boolean; matches: (s: string) => boolean; showPopover: () => void } | null = null;
const mk = (isOpen: boolean) => {
  const p = { isOpen, matches: (s: string) => s === ":popover-open" && p.isOpen, showPopover: () => { p.isOpen = true; } };
  return p;
};
g.document = {
  addEventListener: (n: string, f: Fn) => (handlers[n] ??= []).push(f),
  querySelector: () => null,
  querySelectorAll: () => [],
  getElementById: (id: string) => (id === "graph-view" ? panel : null),
  body: {},
};
g.window = g;
g.htmx = { trigger: () => {} };
await import("../static/js/main.js");
const fire = (n: string) => (handlers[n] ?? []).forEach((f) => f({ detail: {}, target: {} }));

test("an open panel replaced by a swap opens again", () => {
  panel = mk(true);
  fire("htmx:beforeSwap");
  panel = mk(false); // the swap put a new, closed panel in
  fire("htmx:afterSettle");
  assert.equal(panel.isOpen, true);
});

test("a closed panel stays closed", () => {
  panel = mk(false);
  fire("htmx:beforeSwap");
  panel = mk(false);
  fire("htmx:afterSettle");
  assert.equal(panel.isOpen, false);
});
