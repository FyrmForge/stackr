// Hover crosshair + tooltip for [data-chart] metric charts. Point geometry
// comes precomputed as viewBox fractions in data-points (see chart.templ).
(function () {
  function wire(chart) {
    if (chart.dataset.wired) return;
    chart.dataset.wired = "1";
    const pts = JSON.parse(chart.dataset.points || "[]");
    if (!pts.length) return;
    const unit = chart.dataset.unit || "";
    const cross = chart.querySelector("[data-crosshair]");
    const dot = chart.querySelector("[data-dot]");
    const tip = chart.querySelector("[data-tooltip]");

    chart.addEventListener("mousemove", (e) => {
      const r = chart.getBoundingClientRect();
      const fx = (e.clientX - r.left) / r.width;
      let best = 0;
      let bestD = Infinity;
      for (let i = 0; i < pts.length; i++) {
        const d = Math.abs(pts[i].x - fx);
        if (d < bestD) { bestD = d; best = i; }
      }
      const p = pts[best];
      const x = p.x * r.width;
      const y = p.y * r.height;
      cross.style.left = x + "px";
      cross.style.height = r.height * 0.875 + "px"; // stop above the x labels
      dot.style.left = x + "px";
      dot.style.top = y + "px";
      tip.textContent = p.v + unit + " · " + p.t;
      tip.style.left = Math.min(x + 10, r.width - 130) + "px";
      tip.style.top = Math.max(y - 30, 4) + "px";
      cross.classList.remove("hidden");
      dot.classList.remove("hidden");
      tip.classList.remove("hidden");
    });
    chart.addEventListener("mouseleave", () => {
      cross.classList.add("hidden");
      dot.classList.add("hidden");
      tip.classList.add("hidden");
    });
  }

  function wireAll() {
    document.querySelectorAll("[data-chart]").forEach(wire);
  }
  wireAll();
  document.body.addEventListener("htmx:afterSwap", wireAll);
})();
