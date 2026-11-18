// node --test test/ (from ui/): a row is held when it differs from what the
// server stored, not from the control's default (a refused save redraws the
// form with the refused values as defaults).
import { test } from "node:test";
import assert from "node:assert/strict";

const g = globalThis as Record<string, unknown>;
g.HTMLElement = class {};
class Sel {}
g.HTMLSelectElement = Sel;
g.HTMLInputElement = class {};
g.HTMLTextAreaElement = class {};
let Read: new () => Record<string, any>;
g.customElements = { define: (_: string, c: typeof Read) => (Read = c) };
await import("../ts/settings-read.ts");

const text = (o: Record<string, unknown>) => ({ value: "", defaultValue: "", dataset: {}, ...o });

test("a refused edit, redrawn as the default, still differs from what is stored", () => {
  const r = new Read();
  const c = text({ value: "8", defaultValue: "8", dataset: { stored: "4" } });
  assert.equal(r.dirty(c), true);
  r.reset(c);
  assert.equal(c.value, "4");
  assert.equal(r.dirty(c), false);
});

test("without a stored value the default is it", () => {
  const r = new Read();
  assert.equal(r.dirty(text({ value: "x", defaultValue: "x" })), false);
  assert.equal(r.dirty(text({ value: "y", defaultValue: "x" })), true);
});

test("a select compares its value with the stored one, option 0 when none matches", () => {
  const r = new Read();
  const opts = [{ value: "" }, { value: "true" }, { value: "false" }];
  const sel = (value: string, stored: string) => Object.assign(new Sel(), { value, options: opts, dataset: { stored } });
  assert.equal(r.dirty(sel("true", "")), true);
  assert.equal(r.dirty(sel("true", "true")), false);
  assert.equal(r.dirty(sel("", "gone")), false);
});
