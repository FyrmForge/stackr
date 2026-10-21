// <local-time fmt="Jan 2 15:04"><time datetime="...Z">UTC text</time></local-time>
// rewrites its <time> in the viewer's zone: <body data-tz>, "" for the
// browser's, else an IANA name (Account, Appearance). The server's text is
// the same layout in UTC, so without this the page still reads, in UTC.
// <local-time zone> shows the zone's short name ("CEST"); a <local-time>
// around a <select> adds every zone the browser knows to it.

// fmtTime is iso in zone ("" = the browser's) laid out like Go's
// time.Format: Jan 2006 02 2 15 04 05 are the tokens, the rest is literal.
export function fmtTime(iso: string, layout: string, zone: string): string {
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "";
  const p: Record<string, string> = {};
  const f = new Intl.DateTimeFormat("en-US", {
    timeZone: zone || undefined,
    year: "numeric", month: "short", day: "numeric",
    hour: "2-digit", minute: "2-digit", second: "2-digit", hourCycle: "h23",
  });
  for (const x of f.formatToParts(d)) p[x.type] = x.value;
  const tok: Record<string, string> = {
    Jan: p.month, "2006": p.year, "02": p.day.padStart(2, "0"), "2": p.day,
    "15": p.hour, "04": p.minute, "05": p.second,
  };
  return layout.replace(/Jan|2006|02|15|04|05|2/g, (t) => tok[t]);
}

// zoneName is the zone's short name at that moment: "UTC", "CEST", or
// "GMT-4" where the zone has no common abbreviation.
export function zoneName(zone: string, at = new Date()): string {
  const f = new Intl.DateTimeFormat("en-GB", { timeZone: zone || undefined, timeZoneName: "short" });
  return f.formatToParts(at).find((x) => x.type === "timeZoneName")?.value ?? "";
}

class LocalTime extends HTMLElement {
  connectedCallback(): void {
    const zone = document.body.dataset.tz ?? "";
    if (this.hasAttribute("zone")) {
      this.textContent = zoneName(zone);
      return;
    }
    const sel = this.querySelector("select");
    if (sel) {
      const have = new Set([...sel.options].map((o) => o.value));
      for (const z of Intl.supportedValuesOf("timeZone")) if (!have.has(z)) sel.add(new Option(z, z));
      return;
    }
    const t = this.querySelector<HTMLTimeElement>("time[datetime]");
    const s = t && fmtTime(t.dateTime, this.getAttribute("fmt") ?? "", zone);
    if (t && s) t.textContent = s;
  }
}

customElements.define("local-time", LocalTime);
