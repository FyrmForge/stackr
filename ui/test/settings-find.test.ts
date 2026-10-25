// node --test test/ (from ui/): a search hit inside a closed "Show advanced"
// opens the <details> between it and its row, and only those.
import { test } from "node:test";
import assert from "node:assert/strict";

const g = globalThis as Record<string, unknown>;
g.HTMLElement = class {};
g.customElements = { define() {} };
const { openTo } = await import("../ts/settings-find.ts");

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
