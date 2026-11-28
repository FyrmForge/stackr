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

test("include posts every match, ticks only when checked", () => {
  const box = (name: string, checked: boolean) => ({  name, value: "on", type: "checkbox", checked, disabled: false, hasAttribute: () => true });
  const wrapper = {};
  const el = Object.assign(new Ctor(), {
    parentElement: wrapper,
    getAttribute: (a: string) => (a === "include" ? "input[type=checkbox]" : null),
    querySelectorAll: () => [box("stop", true), box("backup", true), box("other", false)],
  });
  const sent: string[][] = [];
  const params = { append: (k: string, v: string) => sent.push([k, v]) };
  const fire = (target: unknown) => (el.onConfig as (e: unknown) => void)({ target, detail: { parameters: params } });
  fire({}); // not the wrapper's own request
  assert.deepEqual(sent, []);
  fire(wrapper);
  assert.deepEqual(sent, [["stop", "on"], ["backup", "on"]]);
});
