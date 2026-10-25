// node --test test/ (from ui/): a search hit inside a closed "Show advanced"
// opens the <details> between it and its row, and only those.
import { test } from "node:test";
import assert from "node:assert/strict";

const g = globalThis as Record<string, unknown>;
g.HTMLElement = class {};
g.customElements = { define() {} };
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
