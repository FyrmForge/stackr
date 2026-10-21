export function fmtTime(iso, layout, zone) {
    const d = new Date(iso);
    if (isNaN(d.getTime()))
        return "";
    const p = {};
    const f = new Intl.DateTimeFormat("en-US", {
        timeZone: zone || undefined,
        year: "numeric", month: "short", day: "numeric",
        hour: "2-digit", minute: "2-digit", second: "2-digit", hourCycle: "h23",
    });
    for (const x of f.formatToParts(d))
        p[x.type] = x.value;
    const tok = {
        Jan: p.month, "2006": p.year, "02": p.day.padStart(2, "0"), "2": p.day,
        "15": p.hour, "04": p.minute, "05": p.second,
    };
    return layout.replace(/Jan|2006|02|15|04|05|2/g, (t) => tok[t]);
}
export function zoneName(zone, at = new Date()) {
    const f = new Intl.DateTimeFormat("en-GB", { timeZone: zone || undefined, timeZoneName: "short" });
    return f.formatToParts(at).find((x) => x.type === "timeZoneName")?.value ?? "";
}
class LocalTime extends HTMLElement {
    connectedCallback() {
        const zone = document.body.dataset.tz ?? "";
        if (this.hasAttribute("zone")) {
            this.textContent = zoneName(zone);
            return;
        }
        const sel = this.querySelector("select");
        if (sel) {
            const have = new Set([...sel.options].map((o) => o.value));
            for (const z of Intl.supportedValuesOf("timeZone"))
                if (!have.has(z))
                    sel.add(new Option(z, z));
            return;
        }
        const t = this.querySelector("time[datetime]");
        const s = t && fmtTime(t.dateTime, this.getAttribute("fmt") ?? "", zone);
        if (t && s)
            t.textContent = s;
    }
}
customElements.define("local-time", LocalTime);
