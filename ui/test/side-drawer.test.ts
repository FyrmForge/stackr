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
    querySelector: () => body,
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
