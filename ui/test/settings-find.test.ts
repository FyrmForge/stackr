// node --test test/ (from ui/): a search hit inside a closed "Show advanced"
// opens the <details> between it and its row, and only those.
import { test } from "node:test";
import assert from "node:assert/strict";

const g = globalThis as Record<string, unknown>;
g.HTMLElement = class {};
let Find: new () => Record<string, any>;
g.customElements = { define: (_: string, c: typeof Find) => (Find = c) };
g.document = { createTreeWalker: () => ({ nextNode: () => false }) }; // no text to mark
g.NodeFilter = { SHOW_TEXT: 4 };
const { openTo, pattern, matches } = await import("../ts/settings-find.ts");

test("the live region counts sections in plain words", () => {
  assert.equal(matches(0), "No settings match.");
  assert.equal(matches(1), "1 section matches.");
  assert.equal(matches(3), "3 sections match.");
});

type Fake = { tagName: string; open?: boolean; parentElement: Fake | null };

test("a hit opens the closed details above it, below its row", () => {
  const row: Fake = { tagName: "DETAILS", open: false, parentElement: null };
  const adv: Fake = { tagName: "DETAILS", open: false, parentElement: row };
  const already: Fake = { tagName: "DETAILS", open: true, parentElement: adv };
  const div: Fake = { tagName: "DIV", parentElement: already };
  const label: Fake = { tagName: "LABEL", parentElement: div };
  const got = openTo(label as unknown as Element, row as unknown as Element);
  assert.deepEqual(got, [adv]);
  assert.equal(adv.open, true);
  assert.equal(row.open, false);
});

test("color and colour find each other, anything else is literal", () => {
  assert.equal(pattern("color").exec("tile colour")?.[0], "colour");
  assert.equal(pattern("colour").exec("env color")?.[0], "color");
  assert.equal(pattern("a.b").test("axb"), false);
  assert.equal(pattern("(x").exec("f(x)")?.index, 1);
  assert.equal(pattern("port").test("no match"), false);
});

// one row holding fields, each with a labelled element and maybe a key, run through the real onInput
type Item = { attrs: Record<string, string>; field: { hidden: boolean }; parentElement: unknown };
function run(q: string, only: boolean, fields: { key?: string; label?: string; changed?: boolean; inAdvanced?: boolean }[]) {
  const adv = { tagName: "DETAILS", open: false, parentElement: null as unknown };
  const row: Record<string, any> = { dataset: { acc: "r" }, open: false, hidden: false, removeAttribute() {}, matches: () => false };
  const items: Item[] = [];
  const flds = fields.map((f) => {
    const field = { hidden: false, changed: !!f.changed, hasAttribute: () => !!f.changed };
    const el = { attrs: {} as Record<string, string>, field, parentElement: f.inAdvanced ? adv : row };
    if (f.key) el.attrs["data-find-key"] = f.key;
    if (f.label) el.attrs["aria-label"] = f.label;
    items.push(el);
    return field;
  });
  row.querySelector = () => (fields.some((f) => f.changed) ? {} : null);
  row.querySelectorAll = (sel: string) =>
    sel === "[data-field]" ? flds : items.map((i) => ({ matches: () => false, getAttribute: (n: string) => i.attrs[n] ?? null, closest: () => (i.field.hidden ? i.field : null), parentElement: i.parentElement, textContent: "" }));
  adv.parentElement = row;
  const el = new Find();
  const box = { value: q, matches: () => true, checked: only };
  el.querySelector = (sel: string) => (sel === "[data-find]" ? box : sel === "[data-only-changed]" ? box : null);
  el.querySelectorAll = (sel: string) => (sel === "details[data-acc]" ? [row] : []);
  el.onInput({ target: box });
  return { row, adv };
}

test("a key finds its row, _ and space alike, case aside", () => {
  for (const q of ["mem_limit_mb", "mem limit", "Mem_Limit", "limit mb"]) assert.equal(run(q, false, [{ key: "mem_limit_mb" }]).row.hidden, false, q);
  assert.equal(run("cpu", false, [{ key: "mem_limit_mb" }]).row.hidden, true);
});

test("a key inside Show advanced opens it", () => {
  const { row, adv } = run("shm_size_mb", false, [{ key: "shm_size_mb", inAdvanced: true }]);
  assert.equal(row.hidden, false);
  assert.equal(adv.open, true);
});

test("a label still matches, and a field with no key is unaffected", () => {
  assert.equal(run("memory", false, [{ label: "Memory limit" }]).row.hidden, false);
  assert.equal(run("name", false, [{ label: "Rename" }, { key: "mem_limit_mb" }]).row.hidden, false);
  assert.equal(run("name", false, [{}]).row.hidden, true);
  assert.equal(run("name_x", false, [{ label: "name x" }]).row.hidden, true); // _ is a space for keys only
});

test("only changed hides an unchanged field's key hit", () => {
  assert.equal(run("mem", true, [{ key: "mem_limit_mb" }]).row.hidden, true);
  assert.equal(run("mem", true, [{ key: "mem_limit_mb", changed: true }]).row.hidden, false);
});
