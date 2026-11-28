// node --test test/ (from ui/): a request into the drawer that swapped nothing
// (204, redirect header) ends the loading text; another request's end does not.
import { test } from "node:test";
import assert from "node:assert/strict";

const g = globalThis as Record<string, unknown>;
g.HTMLElement = class {};
g.CustomEvent = class {};
const handlers: Record<string, (e: unknown) => void> = {};
g.document = { addEventListener: (n: string, f: (e: unknown) => void) => (handlers[n] = f) };
g.location = { href: "http://x/" };
g.history = { state: null, replaceState: () => {} };
let Ctor: new () => Record<string, unknown>;
g.customElements = { define: (_: string, c: typeof Ctor) => (Ctor = c) };
await import("../static/js/elements/side-drawer.js");

test("afterRequest on the drawer body closes a lone loading text", () => {
  const body = { querySelector: (s: string) => (s === "[data-loading]" ? {} : null), replaceChildren: () => {} };
  let open = true;
  const el = Object.assign(new Ctor(), {
    addEventListener: () => {},
    querySelector: (s: string) => (s === "#drawer-body" ? body : null),
    hasAttribute: () => open,
    removeAttribute: () => (open = false),
    dispatchEvent: () => {},
  });
  (el.connectedCallback as () => void).call(el);
  handlers["htmx:afterRequest"]({ detail: { target: {} } });
  assert.equal(open, true, "another request's end closed it");
  handlers["htmx:afterRequest"]({ detail: { target: body } });
  assert.equal(open, false);
});

test("expand swaps the full-screen button's label and pressed state", () => {
  const attrs: Record<string, string> = {};
  const btn = { setAttribute: (k: string, v: string) => (attrs[k] = v), title: "" };
  g.document = { ...(g.document as object), getElementById: () => null };
  const el = Object.assign(new Ctor(), {
    toggleAttribute: () => {},
    style: { setProperty: () => {} },
    querySelector: () => btn,
  });
  const expand = el.expand as (on: boolean) => void;
  expand.call(el, true);
  assert.deepEqual([attrs["aria-pressed"], attrs["aria-label"], btn.title], ["true", "Exit full screen", "Exit full screen"]);
  expand.call(el, false);
  assert.deepEqual([attrs["aria-pressed"], attrs["aria-label"], btn.title], ["false", "Full screen", "Full screen"]);
});

function drawer(opts: { dirty?: boolean; contains?: (n: unknown) => boolean; footer?: boolean } = {}) {
  const calls = { show: 0, closed: 0 };
  const dlg = { open: false, showModal: () => { calls.show++; dlg.open = true; }, close: () => { dlg.open = false; } };
  const body = { contains: opts.contains ?? (() => true), querySelector: () => null, replaceChildren: () => {} };
  let open = true;
  const el = Object.assign(new Ctor(), {
    addEventListener: () => {},
    querySelector: (s: string) => (s === "#drawer-body" ? body : s === "dialog[data-discard]" ? dlg : s.includes("form[data-hold]") && opts.footer ? {} : null),
    hasAttribute: (a: string) => (a === "open" ? open : false),
    setAttribute: () => {},
    removeAttribute: () => (open = false),
    dispatchEvent: () => { calls.closed++; },
    dirty: opts.dirty ?? false,
  });
  g.document = { addEventListener: (n: string, f: (e: unknown) => void) => (handlers[n] = f), activeElement: null };
  (el.connectedCallback as () => void).call(el);
  return { el, body, dlg, calls, isOpen: () => open };
}

test("a successful non-GET request from inside the drawer clears dirty, even with no swap", () => {
  const { el, body } = drawer({ dirty: true });
  const xhr = {}, form = {}, elt = { closest: (s: string) => (s === "form" ? form : null) };
  (el.edited as WeakSet<object>).add(form);
  handlers["htmx:beforeRequest"]({ detail: { elt, xhr, target: elt, requestConfig: { verb: "post" } } });
  handlers["htmx:afterRequest"]({ detail: { xhr, successful: true, target: body } });
  assert.equal(el.dirty, false);
});

test("a failed (422) save, a GET poll and a request from outside keep dirty", () => {
  const { el } = drawer({ dirty: true });
  const bad = {}, poll = {}, out = {};
  handlers["htmx:beforeRequest"]({ detail: { elt: { closest: () => ({}) }, xhr: bad, target: {}, requestConfig: { verb: "post" } } });
  handlers["htmx:afterRequest"]({ detail: { xhr: bad, successful: false, target: {} } });
  handlers["htmx:beforeRequest"]({ detail: { elt: { closest: () => ({}) }, xhr: poll, target: {}, requestConfig: { verb: "get" } } });
  handlers["htmx:afterRequest"]({ detail: { xhr: poll, successful: true, target: {} } });
  assert.equal(el.dirty, true);
  const o = drawer({ dirty: true, contains: () => false });
  handlers["htmx:beforeRequest"]({ detail: { elt: { closest: () => ({}) }, xhr: out, target: {}, requestConfig: { verb: "post" } } });
  handlers["htmx:afterRequest"]({ detail: { xhr: out, successful: true, target: {} } });
  assert.equal(o.el.dirty, true);
});

test("a swap into a child keeps dirty; a swap into the body itself resets it", () => {
  const { el, body } = drawer({ dirty: true });
  handlers["htmx:afterSwap"]({ detail: { target: {} } });
  assert.equal(el.dirty, true);
  handlers["htmx:afterSwap"]({ detail: { target: body } });
  assert.equal(el.dirty, false);
});

test("a dirty close asks in the panel modal: Cancel keeps the drawer, Discard closes it", () => {
  const { el, dlg, calls, isOpen } = drawer({ dirty: true });
  const close = el.close as (force?: boolean) => void;
  close.call(el);
  assert.deepEqual([calls.show, isOpen()], [1, true]);
  close.call(el); // the modal is already open: no second showModal
  assert.equal(calls.show, 1);
  const click = el.onClick as (e: unknown) => void;
  const hit = (attr: string) => ({ target: { closest: (s: string) => (s.includes(attr) ? {} : null) } });
  click.call(el, hit("data-keep"));
  assert.deepEqual([dlg.open, isOpen(), el.dirty], [false, true, true]);
  close.call(el);
  click.call(el, hit("data-discard-yes"));
  assert.deepEqual([dlg.open, isOpen(), calls.closed], [false, false, 1]);
});

test("a clean or forced close never asks", () => {
  const a = drawer();
  (a.el.close as () => void).call(a.el);
  assert.deepEqual([a.calls.show, a.isOpen()], [0, false]);
  const b = drawer({ dirty: true });
  (b.el.close as (f: boolean) => void).call(b.el, true);
  assert.deepEqual([b.calls.show, b.isOpen()], [0, false]);
});

test("a staging POST (preview) or an unrelated button keeps dirty; saving the edited form clears it", () => {
  const { el, body } = drawer({ dirty: true });
  const form = {}, other = {};
  (el.edited as WeakSet<object>).add(form);
  const send = (elt: object, xhr: object) => {
    handlers["htmx:beforeRequest"]({ detail: { elt, xhr, target: elt, requestConfig: { verb: "post" } } });
    handlers["htmx:afterRequest"]({ detail: { xhr, successful: true, target: body } });
  };
  send({ closest: (s: string) => (s === "form" ? form : s === "[data-hold]" ? {} : null) }, {}); // preview
  assert.equal(el.dirty, true);
  send({ closest: (s: string) => (s === "form" ? other : null) }, {}); // Restart button, unedited form
  assert.equal(el.dirty, true);
  send({ closest: () => null }, {}); // button in no form
  assert.equal(el.dirty, true);
  send({ closest: (s: string) => (s === "form" ? form : null) }, {}); // the edited form's save
  assert.equal(el.dirty, false);
});

test("an open staging form or a pending footer makes close ask; a forced close shuts an open discard dialog", () => {
  const a = drawer({ footer: true });
  (a.el.close as () => void).call(a.el);
  assert.deepEqual([a.calls.show, a.isOpen()], [1, true]);
  (a.el.close as (f: boolean) => void).call(a.el, true);
  assert.deepEqual([a.dlg.open, a.isOpen()], [false, false]);
});

test("a pending footer's Apply clears dirty", () => {
  const { el, body } = drawer({ dirty: true });
  const xhr = {};
  const elt = { closest: (s: string) => (s.includes("-held") ? {} : null) };
  handlers["htmx:beforeRequest"]({ detail: { elt, xhr, target: elt, requestConfig: { verb: "post" } } });
  handlers["htmx:afterRequest"]({ detail: { xhr, successful: true, target: body } });
  assert.equal(el.dirty, false);
});
