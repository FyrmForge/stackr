// node --test test/ (from ui/): main.js's poll guard. The file is loaded with
// a stub document that records its listeners, then htmx:beforeRequest is
// driven with fake elements.
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

const field = (o: Record<string, unknown>) => ({ type: "text", tagName: "INPUT", value: "", defaultValue: "", ...o });
const root = (fs: unknown[]) => ({ querySelectorAll: () => fs });
const pollDirty = g.pollDirty as (r: unknown) => boolean;

test("a field that differs from what arrived is dirty", () => {
  assert.equal(pollDirty(root([field({})])), false);
  assert.equal(pollDirty(root([field({ value: "typed" })])), true);
  assert.equal(pollDirty(root([field({ type: "checkbox", checked: true, defaultChecked: false })])), true);
  assert.equal(pollDirty(root([field({ type: "hidden", value: "x" })])), false);
  const opt = (defaultSelected: boolean) => ({ defaultSelected });
  const select = (options: unknown[], selectedIndex: number) => ({ type: "select-one", tagName: "SELECT", multiple: false, options, selectedIndex });
  // no option marked selected: the browser selects option 0, which is not dirty
  assert.equal(pollDirty(root([select([opt(false), opt(false)], 0)])), false);
  assert.equal(pollDirty(root([select([opt(false), opt(false)], 1)])), true);
  assert.equal(pollDirty(root([select([opt(false), opt(true)], 1)])), false);
  assert.equal(pollDirty(root([select([opt(false), opt(true)], 0)])), true);
  assert.equal(pollDirty(null), false);
});

test("a dirty region cancels the poll and one retry is pending", () => {
  const attrs: Record<string, string> = { "data-poll-guard": "region dirty", "data-poll-retry": "sse:end" };
  const box = { querySelector: () => null, matches: () => false, querySelectorAll: () => [field({ value: "typed" })] };
  const el = {
    getAttribute: (n: string) => attrs[n] ?? null,
    closest: () => null,
    parentElement: box,
    isConnected: true,
  } as Record<string, unknown>;
  let prevented = 0;
  const run = () => handlers["htmx:beforeRequest"].forEach((f) => f({ detail: { elt: el }, preventDefault: () => prevented++ }));
  run();
  const first = el._pollRetry;
  run();
  assert.equal(prevented, 2);
  assert.ok(first);
  assert.equal(el._pollRetry, first, "a second block adds no timer");
  clearTimeout(first as ReturnType<typeof setTimeout>);
});
