// node --test test/ (from ui/): confirm-dialog enables its opener, rendered
// disabled, when the element upgrades, so an early click is never lost.
import { test } from "node:test";
import assert from "node:assert/strict";

const g = globalThis as Record<string, unknown>;
g.HTMLElement = class {};
let Ctor: new () => Record<string, unknown>;
g.customElements = { define: (_: string, c: typeof Ctor) => (Ctor = c) };
await import("../static/js/elements/confirm-dialog.js");

test("connecting enables the opener", () => {
  const opener = { disabled: true };
  const el = Object.assign(new Ctor(), {
    closest: () => null,
    addEventListener: () => {},
    hasAttribute: () => false,
    querySelectorAll: (sel: string) => (sel === "button[data-open]" ? [opener] : []),
  });
  (el.connectedCallback as () => void).call(el);
  assert.equal(opener.disabled, false);
});
