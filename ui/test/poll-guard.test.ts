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

test("a response that lands while a dialog is open is not swapped", () => {
  const el = { getAttribute: (n: string) => (n === "data-poll-guard" ? "" : null), closest: () => null, parentElement: null };
  const doc = g.document as { querySelector: () => unknown };
  const run = () => {
    // htmx 2.0.4: beforeSwap's elt is the swap target; the poller rides on the xhr from beforeRequest
    const xhr: Record<string, unknown> = {};
    handlers["htmx:beforeRequest"].forEach((f) => f({ detail: { elt: el, xhr }, preventDefault: () => {} }));
    const detail = { elt: { getAttribute: () => null }, xhr, shouldSwap: true };
    doc.querySelector = open;
    handlers["htmx:beforeSwap"].forEach((f) => f({ detail }));
    doc.querySelector = () => null;
    return detail.shouldSwap;
  };
  let open: () => unknown = () => null;
  assert.equal(run(), true);
  open = () => ({});
  assert.equal(run(), false);
});

test("a response that lands after an edit started is not swapped", () => {
  const attrs: Record<string, string> = { "data-poll-guard": "region dirty" };
  const fields = [field({})];
  const box = { querySelector: () => null, matches: () => false, querySelectorAll: () => fields };
  const el = { getAttribute: (n: string) => attrs[n] ?? null, closest: () => null, parentElement: box, isConnected: true };
  const xhr: Record<string, unknown> = {};
  handlers["htmx:beforeRequest"].forEach((f) => f({ detail: { elt: el, xhr }, preventDefault: () => {} }));
  const swap = () => {
    const detail = { elt: {}, xhr, shouldSwap: true };
    handlers["htmx:beforeSwap"].forEach((f) => f({ detail }));
    return detail.shouldSwap;
  };
  assert.equal(swap(), true);
  fields[0] = field({ value: "typed" });
  assert.equal(swap(), false);
  fields[0] = field({});
  box.matches = () => true;
  assert.equal(swap(), false, "focus inside the region");
  attrs["data-poll-guard"] = "";
  assert.equal(swap(), true, "regions without the tokens keep their behaviour");
});

test("a blur-triggered request is dropped while a button of its target is pressed", () => {
  const btn = { closest: () => btn };
  handlers["pointerdown"].forEach((f) => f({ target: btn }));
  const t = { hasAttribute: (n: string) => n === "data-press-wins", contains: (x: unknown) => x === btn };
  const run = (type: string) => {
    let prevented = 0;
    handlers["htmx:beforeRequest"].forEach((f) =>
      f({ detail: { elt: { getAttribute: () => null }, target: t, requestConfig: { triggeringEvent: { type } } }, preventDefault: () => prevented++ }),
    );
    return prevented;
  };
  assert.equal(run("change"), 1);
  assert.equal(run("submit"), 0, "only a blur is dropped");
  handlers["pointerup"].forEach((f) => f({}));
  return new Promise((r) => setTimeout(r, 5)).then(() => assert.equal(run("change"), 0, "released: the blur redraws again"));
});
