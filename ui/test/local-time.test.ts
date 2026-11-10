// node --test test/ (from ui/): how <local-time> reads a time in each zone.
// DOM globals are stubbed so the element file loads.
import { test } from "node:test";
import assert from "node:assert/strict";

const g = globalThis as Record<string, unknown>;
g.HTMLElement = class {};
g.customElements = { define() {} };
const { fmtTime, zoneName, elapsed } = await import("../ts/local-time.ts");

const at = "2026-07-01T22:00:05Z";

test("a time reads in the picked zone, the browser's when none is picked", () => {
  assert.equal(fmtTime(at, "Jan 2 15:04", "UTC"), "Jul 1 22:00");
  assert.equal(fmtTime(at, "Jan 02 15:04:05", "Europe/Berlin"), "Jul 02 00:00:05");
  assert.equal(fmtTime(at, "Jan 2, 2006", "Europe/Berlin"), "Jul 2, 2026");
  assert.equal(fmtTime("2026-01-05T00:30:00Z", "15:04", "UTC"), "00:30"); // never 24:30
  const browser = Intl.DateTimeFormat().resolvedOptions().timeZone;
  assert.equal(fmtTime(at, "Jan 2 15:04:05", ""), fmtTime(at, "Jan 2 15:04:05", browser));
  assert.equal(fmtTime("not a time", "15:04", "UTC"), "");
});

test("the zone label is its short name", () => {
  const d = new Date(at);
  assert.equal(zoneName("UTC", d), "UTC");
  assert.equal(zoneName("Europe/Berlin", d), "CEST");
  assert.equal(zoneName("Europe/Berlin", new Date("2026-01-01T00:00:00Z")), "CET");
});

test("a clock reads mm:ss, h:mm:ss from an hour", () => {
  assert.equal(elapsed(560_000), "09:20");
  assert.equal(elapsed(-5), "00:00");
  assert.equal(elapsed(3_725_000), "1:02:05");
});
