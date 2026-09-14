// Live updates over the hamr websocket hub (uses the HamrWS helper from
// ws.js). Pages declare rooms via [data-ws-room]; refreshable fragments carry
// [data-ws-refresh] and an hx-trigger that includes "refresh". Server events
// carry no payload, clients just re-fetch what the page already renders.
//
// The value of [data-ws-refresh] is a space-separated list of event names the
// region cares about ("project", "flows", "server", "containers",
// "notification"); empty means every event. Without it the traffic sampler's
// 5s "flows" tick refreshed every live region on the page, which read as a
// poll over a websocket.
(function () {
  if (!document.querySelector("[data-ws-room]")) return; // no live regions here
  var ws = new HamrWS("/ws");

  ws.onopen = function () {
    document.querySelectorAll("[data-ws-room]").forEach(function (el) {
      ws.send(JSON.stringify({ action: "join", room: el.dataset.wsRoom }));
    });
  };

  ws.onmessage = function (e) {
    var ev;
    try { ev = JSON.parse(e.data); } catch (_) { return; }
    // HTML swap events (server-rendered fragments)
    if (ev.target && ev.html) {
      var t = document.querySelector(ev.target);
      if (!t) return;
      if (ev.swap === "outerHTML") t.outerHTML = ev.html;
      else t.innerHTML = ev.html;
      return;
    }
    // data-only "changed" events: page scripts first, then nudge htmx
    window.dispatchEvent(new CustomEvent("stackr:ws", { detail: ev }));
    if (window.htmx) {
      document.querySelectorAll("[data-ws-refresh]").forEach(function (el) {
        var want = (el.dataset.wsRefresh || "").trim();
        if (want && want.split(/\s+/).indexOf(ev.type) === -1) return;
        window.htmx.trigger(el, "refresh");
      });
    }
  };
})();
