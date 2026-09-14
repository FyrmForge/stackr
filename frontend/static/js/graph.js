// Project topology canvas. Performance model: pan/zoom apply a single
// transform to #graph-world (one GPU-composited layer), so children never
// re-layout while interacting. Node drags move one card and only re-point the
// edges touching it. No framework, no per-frame reconciliation.
(function () {
  const viewport = document.getElementById("graph-viewport");
  const world = document.getElementById("graph-world");
  if (!viewport || !world) return;

  // Card geometry comes from the server (data-* on #graph-viewport, fed by the
  // constants in internal/graph): this file drags, nudges and snaps cards that
  // the server placed, so a second opinion about their size would show up as
  // cards that overlap on one side and not the other. The fallbacks only cover
  // a stale cached page.
  const num = (name, dflt) => Number(viewport.dataset[name]) || dflt;
  const NODE_W = num("cardW", 220), NODE_H = num("cardH", 96);
  const GUTTER_X = num("cardGapX", 40); // clear space a placement keeps around a card
  // edge fan-out (see spreadAnchors): ideal gap between two anchors on one card
  // side, and the fraction of that side they may spread across. Declared here
  // because redrawEdges runs during init, above their use in source order.
  const FAN_GAP = 14, FAN_SPAN = 0.6;
  const CARD_GAP = 12; // clear space kept between cards by settings.nooverlap
  // how far a sub-tile is dealt out from under its card, must match
  // subtilePeek/subtileOffset in components/canvas/canvas.templ
  const SUBTILE_PEEK = 28, SUBTILE_OFFSET = 8;
  // dot spacing of the .graph-grid background, also the snap grid
  const GRID_PX = num("gridPx", 22);
  const saveURL = viewport.dataset.saveUrl;
  const csrf = viewport.dataset.csrf;

  // --- shared helpers: the patterns everything below used to repeat ---
  // left/top of an absolutely-positioned element, in world px
  const leftOf = (el) => parseFloat(el.style.left) || 0;
  const topOf = (el) => parseFloat(el.style.top) || 0;

  // Server geometry arrives as a stylesheet (geomCSS rules keyed by
  // data-node-id / data-sub-id) so the markup carries no inline styles.
  // Everything below reads and writes el.style directly, copy the computed
  // values there once, before anything looks. Inline wins over the sheet, so
  // drags keep overriding cleanly afterwards.
  for (const el of world.querySelectorAll("[data-node-id],[data-sub-id]")) {
    if (el.style.left) continue;
    const cs = getComputedStyle(el);
    el.style.left = cs.left;
    el.style.top = cs.top;
    el.style.width = cs.width;
    el.style.height = cs.height;
  }
  // JSON request with the CSRF header; callers keep their own ordering chains
  function postJSON(url, body, method = "POST") {
    return fetch(url, {
      method,
      headers: { "Content-Type": "application/json", "X-CSRF-Token": csrf },
      body: JSON.stringify(body),
    });
  }
  // One drag gesture: capture the pointer, stream moves, always unhook,
  // including pointercancel, which the hand-rolled copies forgot. Pan, marquee
  // and node drag keep their own wiring: they interleave with the two-finger
  // pinch state and their listeners are persistent by design.
  function track(el, e, onMove, onUp) {
    const done = (ev) => {
      el.removeEventListener("pointermove", onMove);
      el.removeEventListener("pointerup", done);
      el.removeEventListener("pointercancel", done);
      if (onUp) onUp(ev);
    };
    el.addEventListener("pointermove", onMove);
    el.addEventListener("pointerup", done);
    el.addEventListener("pointercancel", done);
    try { el.setPointerCapture(e.pointerId); } catch (_) {}
  }
  // Every "close things on Escape" handler in one listener, fired in
  // registration order (same order the standalone listeners ran in).
  const escapeFns = [];
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape") escapeFns.forEach((f) => f());
  });

  // --- canvas settings (per-user, localStorage as offline fallback) ---
  const SETTINGS_KEY = "stackr.graph.settings";
  const PREFS_URL = "/account/graph-prefs";
  let settings = {
    snap: true, system: true, badges: true, divider: true, net: true,
    refs: true, legend: true, fan: true, nooverlap: false, edges: "curved",
    arrows: true, focus: true, arrange: "clusters", startup: true,
  };
  function mergeSettings(source) {
    if (!source || typeof source !== "object") return;
    Object.keys(settings).forEach((key) => {
      if (Object.prototype.hasOwnProperty.call(source, key)) settings[key] = source[key];
    });
  }
  try { mergeSettings(JSON.parse(localStorage.getItem(SETTINGS_KEY))); } catch (_) {}
  function saveSettings() {
    try { localStorage.setItem(SETTINGS_KEY, JSON.stringify(settings)); } catch (_) {}
    postJSON(PREFS_URL, settings, "PUT").catch(() => {});
  }
  // Profile wins over the local copy once it arrives; localStorage only covers
  // the first paint. Runs after init (applySettings is hoisted).
  fetch(PREFS_URL)
    .then((r) => (r.ok ? r.json() : {}))
    .then((prefs) => {
      if (!prefs || typeof prefs !== "object") return;
      mergeSettings(prefs);
      try { localStorage.setItem(SETTINGS_KEY, JSON.stringify(settings)); } catch (_) {}
      document.querySelectorAll("[data-gs]").forEach((cb) => { cb.checked = settings[cb.dataset.gs]; });
      document.querySelectorAll("[data-gs-choice]").forEach((rb) => {
        rb.checked = settings[rb.dataset.gsChoice] === rb.value;
      });
      applySettings();
      redrawEdges();
    })
    .catch(() => {});

  // Edges a card references rather than edges the platform imposes: hiding
  // these leaves the structural lines (ingress, published port, config repo,
  // a db's own volume) in place, which is the point of the toggle.
  const REF_KINDS = ["ref", "shared", "source"];
  // base path -> its traffic lane/label elements (see syncLanes)
  const laneReg = new Map(); // basePath -> [{el, label, side}]
  // card currently under the pointer (see setFocus). Declared up here because
  // applySettings clears the focus and runs during init, above setFocus in
  // source order, a `let` beside the function would still be in its dead zone.
  let focusNode = null;
  // every server-rendered edge; traffic lanes are clones and excluded
  const BASE_EDGES = "path[data-edge-kind]:not(.edge-lane)";
  const SYSTEM_PREFIXES = ["proxy:", "host:"];

  // dashed vertical line between the system (server) column and whatever this
  // canvas is showing; sits behind the nodes and follows their positions.
  // Only the server side is labeled: the other side is stacks, environments or
  // tiles depending on the level, and often several of those at once.
  function updateDivider() {
    let div = world.querySelector(".graph-divider");
    const sys = [...world.querySelectorAll(".graph-node-system")];
    const work = [...world.querySelectorAll(".graph-node:not(.graph-node-system)")];
    if (!settings.divider || !settings.system || !sys.length || !work.length) {
      div?.remove();
      return;
    }
    // anchored to the system zone only, moving stack cards must not move
    // the server boundary
    const right = Math.max(...sys.map((n) => (leftOf(n)) + NODE_W));
    const ys = [...sys, ...work].map((n) => topOf(n));
    const top = Math.min(...ys) - 120;
    const bottom = Math.max(...ys) + NODE_H + 120;
    if (!div) {
      div = document.createElement("div");
      div.className = "graph-divider";
      div.innerHTML = '<span class="graph-divider-label" style="right:10px">server</span>';
      world.prepend(div);
    }
    div.style.left = right + 60 + "px";
    div.style.top = top + "px";
    div.style.height = bottom - top + "px";
  }

  // One pass decides each edge's visibility: several toggles can hide the same
  // line (a proxy->app reference edge answers to both "system nodes" and
  // "reference edges"), and one rule per pass would let the last one win.
  function edgeHidden(p) {
    const kind = p.dataset.edgeKind;
    const ends = (p.dataset.edgeFrom || "") + " " + (p.dataset.edgeTo || "");
    if (!settings.system && SYSTEM_PREFIXES.some((s) => ends.includes(s))) return true;
    if (!settings.refs && REF_KINDS.includes(kind)) return true;
    if (!settings.net && kind === "traffic") return true;
    if (!settings.startup && kind === "startup") return true;
    return false;
  }

  function applySettings() {
    world.querySelectorAll(".graph-node-system").forEach((n) => {
      n.style.display = settings.system ? "" : "none";
    });
    world.querySelectorAll(BASE_EDGES).forEach((p) => {
      p.style.display = edgeHidden(p) ? "none" : "";
    });
    world.querySelectorAll(".node-exposure").forEach((b) => {
      b.style.display = settings.badges ? "" : "none";
    });
    world.querySelectorAll(".edge-net-label").forEach((b) => {
      b.style.display = settings.net ? "" : "none";
    });
    if (!settings.net) {
      world.querySelectorAll(".edge-pulse-fwd, .edge-pulse-rev").forEach((p) => {
        p.classList.remove("edge-pulse-fwd", "edge-pulse-rev");
        p.style.strokeWidth = "";
      });
    }
    world.classList.toggle("graph-no-arrows", !settings.arrows);
    if (!settings.focus) setFocus(null);
    updateDivider();
    buildLegend();
  }

  // Legend rows come from the edges actually on this canvas and still visible,
  // so no level has to declare its own: an org canvas legends config/source
  // lines, an env canvas legends crons and volumes. Each swatch copies its
  // stroke off a real path of that kind, so canvas.templ stays the only place
  // an edge's look is defined.
  const EDGE_LABELS = {
    ingress: "public route",
    port: "published port",
    config: "config as code",
    source: "source repo",
    shared: "managed instance",
    ref: "reference",
    volume: "volume",
    traffic: "observed flow",
    cron: "schedule",
    forward: "live forward",
    startup: "startup order",
  };
  function buildLegend() {
    const box = document.getElementById("graph-legend");
    if (!box) return;
    box.textContent = "";
    if (!settings.legend) { box.hidden = true; return; }
    const sample = {};
    world.querySelectorAll(BASE_EDGES).forEach((p) => {
      if (!edgeHidden(p)) sample[p.dataset.edgeKind] ||= p;
    });
    // fixed order, so toggling something doesn't reshuffle the rows
    for (const kind of Object.keys(EDGE_LABELS)) {
      const p = sample[kind];
      if (!p) continue;
      const row = document.createElement("div");
      row.className = "graph-legend-row";
      const sw = document.createElementNS("http://www.w3.org/2000/svg", "svg");
      sw.setAttribute("width", "26");
      sw.setAttribute("height", "8");
      sw.setAttribute("aria-hidden", "true");
      const line = document.createElementNS("http://www.w3.org/2000/svg", "line");
      line.setAttribute("x1", "1"); line.setAttribute("y1", "4");
      line.setAttribute("x2", "25"); line.setAttribute("y2", "4");
      for (const a of ["stroke", "stroke-width", "stroke-dasharray", "stroke-opacity"]) {
        const v = p.getAttribute(a);
        if (v) line.setAttribute(a, v);
      }
      // Some edges paint from CSS rather than attributes (.edge-busy animates,
      // .edge-forward carries the whole stroke), so the swatch inherits the
      // path's classes as well as its attributes.
      if (p.className.baseVal) line.setAttribute("class", p.className.baseVal);
      sw.appendChild(line);
      row.appendChild(sw);
      row.appendChild(document.createTextNode(EDGE_LABELS[kind]));
      box.appendChild(row);
    }
    // the animated lanes are the one thing on the canvas nothing else explains
    if (settings.net && world.querySelector(".edge-lane")) {
      const row = document.createElement("div");
      row.className = "graph-legend-row";
      row.innerHTML =
        '<svg width="26" height="8" aria-hidden="true">' +
        '<line x1="1" y1="4" x2="25" y2="4" stroke="var(--rw-teal)" stroke-width="2" class="edge-pulse-fwd"></line>' +
        "</svg>live traffic";
      box.appendChild(row);
    }
    box.hidden = !box.firstChild;
  }

  // Canvases open a little zoomed out: at 1:1 a busy graph fills the viewport
  // edge to edge and gives no sense of what is off-screen. Also what the reset
  // control returns to.
  const START_SCALE = 0.8;
  let panX = 0, panY = 0, scale = START_SCALE;

  function applyTransform() {
    world.style.transform = `translate(${panX}px, ${panY}px) scale(${scale})`;
    // The dotted grid is a background on the viewport, which does not move with
    // the world, so pan it and scale its spacing by hand. Without this the dots
    // sit still while the cards slide over them and the canvas reads as a
    // picture being dragged rather than a surface. The dots themselves stay 1px
    // (the gradient's colour stop is absolute), so they stay crisp at any zoom.
    viewport.style.backgroundPosition = `${panX}px ${panY}px`;
    viewport.style.backgroundSize = `${GRID_PX * scale}px ${GRID_PX * scale}px`;
  }

  // bounding box of all nodes, in world coordinates
  function contentBounds() {
    const nodes = world.querySelectorAll(".graph-node");
    if (!nodes.length) return null;
    let minX = Infinity, minY = Infinity, maxX = -Infinity, maxY = -Infinity;
    nodes.forEach((n) => {
      const x = leftOf(n);
      const y = topOf(n);
      minX = Math.min(minX, x); minY = Math.min(minY, y);
      maxX = Math.max(maxX, x + NODE_W); maxY = Math.max(maxY, y + NODE_H);
    });
    return { minX, minY, maxX, maxY };
  }

  // pan/zoom can never send the graph out of bounds: a solid chunk of the
  // content (a full node's worth, or the whole bbox if smaller) must stay
  // inside the viewport on both axes
  function clampPan() {
    const b = contentBounds();
    if (!b) return;
    const rect = viewport.getBoundingClientRect();
    const keepX = Math.min((b.maxX - b.minX) * scale, NODE_W * scale + 80);
    const keepY = Math.min((b.maxY - b.minY) * scale, NODE_H * scale + 80);
    panX = Math.min(panX, rect.width - keepX - b.minX * scale);
    panX = Math.max(panX, keepX - b.maxX * scale);
    panY = Math.min(panY, rect.height - keepY - b.minY * scale);
    panY = Math.max(panY, keepY - b.maxY * scale);
  }

  // center the nodes' bounding box in the viewport (saved positions may sit
  // anywhere, including off-canvas)
  function fitToContent() {
    const b = contentBounds();
    if (!b) return;
    const rect = viewport.getBoundingClientRect();
    panX = (rect.width - (b.maxX - b.minX) * scale) / 2 - b.minX * scale;
    panY = (rect.height - (b.maxY - b.minY) * scale) / 2 - b.minY * scale;
    // Content taller or wider than the viewport centres with its first row (or
    // column) past the top/left edge, clipped away. Pull it back so the first
    // card starts a margin in: the topmost card's screen position is
    // panY + minY*scale, which must be >= margin.
    const margin = 24;
    panX = Math.max(panX, margin - b.minX * scale);
    panY = Math.max(panY, margin - b.minY * scale);
  }
  fitToContent();
  applyTransform();
  window.addEventListener("resize", () => { clampPan(); applyTransform(); });

  const clampScale = (s) => Math.min(2.5, Math.max(0.3, s));

  // zoom toward a viewport-relative point, keeping it fixed under the cursor
  function zoomAt(cx, cy, factor) {
    const newScale = clampScale(scale * factor);
    panX = cx - (cx - panX) * (newScale / scale);
    panY = cy - (cy - panY) * (newScale / scale);
    scale = newScale;
    clampPan();
    applyTransform();
  }

  // --- zoom & pan: wheel ---
  // Three gestures arrive as one event type. A trackpad pinch is certain: the
  // browser marks it with ctrlKey. Telling a mouse wheel from a two-finger
  // trackpad scroll is not, neither sets a flag and only the shape of the
  // delta differs. A wheel notch is a big whole number on one axis; trackpad
  // scrolling is small, often fractional, and usually carries some deltaX.
  // Mouse wheels keep zooming, which is what this canvas has always done;
  // only trackpad scrolling pans, which is what stopped it fighting the pinch.
  // a hard vertical trackpad flick can pass for a notch and zoom a
  // step. Telling those apart properly needs delta history, not worth it.
  function isMouseWheel(e) {
    if (e.deltaMode !== 0) return true; // line/page deltas only come from a wheel
    return e.deltaX === 0 && Number.isInteger(e.deltaY) && Math.abs(e.deltaY) >= 100;
  }
  viewport.addEventListener("wheel", (e) => {
    e.preventDefault();
    const rect = viewport.getBoundingClientRect();
    const cx = e.clientX - rect.left, cy = e.clientY - rect.top;
    if (e.ctrlKey || e.metaKey) {
      // a pinch streams many small deltas, so scale smoothly with them
      zoomAt(cx, cy, Math.exp(-Math.max(-20, Math.min(20, e.deltaY)) * 0.01));
    } else if (isMouseWheel(e)) {
      zoomAt(cx, cy, e.deltaY < 0 ? 1.1 : 1 / 1.1); // unchanged notch feel
    } else {
      panX -= e.deltaX;
      panY -= e.deltaY;
      clampPan();
      applyTransform();
    }
  }, { passive: false });

  // --- zoom: floating control buttons ---
  function zoomCenter(factor) {
    const rect = viewport.getBoundingClientRect();
    zoomAt(rect.width / 2, rect.height / 2, factor);
  }
  viewport.querySelector("[data-zoom-in]")?.addEventListener("click", () => zoomCenter(1.2));
  viewport.querySelector("[data-zoom-out]")?.addEventListener("click", () => zoomCenter(1 / 1.2));
  viewport.querySelector("[data-zoom-reset]")?.addEventListener("click", () => {
    panX = 0; panY = 0; scale = START_SCALE; fitToContent(); applyTransform();
  });

  // --- selection: shift+drag marquees, plain drag pans (Figma's split) ---
  //
  // Plain drag stays pan because that is what every canvas here has always
  // done, and panning is the far more common gesture.
  const selected = new Set();
  function clearSelection() {
    selected.forEach((n) => n.classList.remove("is-selected"));
    selected.clear();
  }
  function selectNode(node) {
    selected.add(node);
    node.classList.add("is-selected");
  }
  // Screen point → world coordinates, the inverse of applyTransform.
  function toWorld(clientX, clientY) {
    const rect = viewport.getBoundingClientRect();
    return [(clientX - rect.left - panX) / scale, (clientY - rect.top - panY) / scale];
  }

  let marquee = null; // {el, x0, y0} in world coordinates
  function marqueeUpdate(e) {
    const [x, y] = toWorld(e.clientX, e.clientY);
    const left = Math.min(x, marquee.x0), top = Math.min(y, marquee.y0);
    const w = Math.abs(x - marquee.x0), h = Math.abs(y - marquee.y0);
    Object.assign(marquee.el.style, {
      left: left + "px", top: top + "px", width: w + "px", height: h + "px",
    });
    // Intersect, not contain: a marquee that only counts fully-enclosed cards
    // makes you drag past the edge of the screen to catch the last one.
    clearSelection();
    world.querySelectorAll(".graph-node:not([data-ephemeral])").forEach((n) => {
      const nx = leftOf(n), ny = topOf(n);
      if (nx < left + w && left < nx + NODE_W && ny < top + h && top < ny + NODE_H) selectNode(n);
    });
    // annotations (labels, boxes) select and move like cards; their size is
    // their own, not the card constants
    world.querySelectorAll(".graph-anno").forEach((n) => {
      const nx = leftOf(n), ny = topOf(n);
      const aw = n.offsetWidth, ah = n.offsetHeight;
      if (nx < left + w && left < nx + aw && ny < top + h && top < ny + ah) selectNode(n);
    });
  }
  function endMarquee() {
    if (!marquee) return;
    marquee.el.remove();
    marquee = null;
  }

  // --- pan: drag empty canvas ---
  let panning = false, startX = 0, startY = 0, startPanX = 0, startPanY = 0;
  viewport.addEventListener("pointerdown", (e) => {
    if (e.target.closest("a, .graph-node, .graph-ctrl, #graph-menu, #env-compare")) return; // anchors (staging/plan pills) navigate, don't pan
    if (e.button === 2) return; // right-click is the context menu's, must not clear the selection it acts on
    e.preventDefault();
    if (e.shiftKey && e.pointerType !== "touch") {
      const [x0, y0] = toWorld(e.clientX, e.clientY);
      const el = document.createElement("div");
      el.className = "graph-marquee";
      world.appendChild(el);
      marquee = { el, x0, y0 };
      marqueeUpdate(e);
      try { viewport.setPointerCapture(e.pointerId); } catch (_) {}
      return;
    }
    clearSelection(); // a click on bare canvas means "never mind"
    setEntered(null); // …and steps back out of an entered group
    panning = true;
    startX = e.clientX; startY = e.clientY;
    startPanX = panX; startPanY = panY;
    viewport.style.cursor = "grabbing";
    try { viewport.setPointerCapture(e.pointerId); } catch (_) {}
  });
  viewport.addEventListener("pointermove", (e) => {
    if (marquee) { marqueeUpdate(e); return; }
    if (!panning || pinchDist > 0) return;
    panX = startPanX + (e.clientX - startX);
    panY = startPanY + (e.clientY - startY);
    clampPan();
    applyTransform();
  });
  function endPan(e) {
    endMarquee();
    panning = false;
    viewport.style.cursor = "";
    try { viewport.releasePointerCapture(e.pointerId); } catch (_) {}
  }
  viewport.addEventListener("pointerup", endPan);
  viewport.addEventListener("pointercancel", endPan);

  escapeFns.push(clearSelection);

  // --- touch: two-finger pinch zoom + pan (capture phase so node handlers
  // can't swallow the second finger; two fingers never mean node drag) ---
  const activeTouches = new Map();
  let pinchDist = 0, pinchMid = null;
  viewport.addEventListener("pointerdown", (e) => {
    if (e.pointerType !== "touch") return;
    activeTouches.set(e.pointerId, [e.clientX, e.clientY]);
    if (activeTouches.size === 2) {
      const [a, b] = [...activeTouches.values()];
      pinchDist = Math.hypot(a[0] - b[0], a[1] - b[1]);
      pinchMid = [(a[0] + b[0]) / 2, (a[1] + b[1]) / 2];
      panning = false;
      // a started node drag is void, put every card in it back where it was
      if (drag && drag.moved) {
        drag.origins.forEach(([n, ox, oy]) => {
          n.style.left = ox + "px";
          n.style.top = oy + "px";
        });
        redrawEdges();
      }
      drag = null;
    }
  }, true);
  viewport.addEventListener("pointermove", (e) => {
    if (!activeTouches.has(e.pointerId)) return;
    activeTouches.set(e.pointerId, [e.clientX, e.clientY]);
    if (activeTouches.size !== 2) return;
    const [a, b] = [...activeTouches.values()];
    const d = Math.hypot(a[0] - b[0], a[1] - b[1]);
    const mid = [(a[0] + b[0]) / 2, (a[1] + b[1]) / 2];
    if (pinchDist > 0 && d > 0) {
      const rect = viewport.getBoundingClientRect();
      // midpoint travel pans; distance change zooms around the midpoint
      panX += mid[0] - pinchMid[0];
      panY += mid[1] - pinchMid[1];
      zoomAt(mid[0] - rect.left, mid[1] - rect.top, d / pinchDist);
    }
    pinchDist = d;
    pinchMid = mid;
  }, true);
  const dropTouch = (e) => {
    if (!activeTouches.delete(e.pointerId)) return;
    if (activeTouches.size < 2) {
      pinchDist = 0;
      pinchMid = null;
      // the finger left over from a pinch must not pan or drag
      panning = false;
      drag = null;
    }
  };
  viewport.addEventListener("pointerup", dropTouch, true);
  viewport.addEventListener("pointercancel", dropTouch, true);

  // --- groups: cards/annotations that move as one ---
  // Persisted per canvas like annotations: members are "node:<id>" /
  // "anno:<id>" keys, saved whole on every change. Dragging any member moves
  // the set; double-click enters a group to move one member; Escape or a
  // bare-canvas click leaves it.
  const groupsURL = saveURL && saveURL.replace(/\/positions$/, "/groups");
  let groupList = [];
  try { groupList = JSON.parse(document.getElementById("graph-groups-data")?.textContent || "[]") || []; } catch (_) {}
  // set by the annotations block below: rides its save chain so a group save
  // never overtakes the id-minting save of a member annotation
  let annoQueue = (fn) => { Promise.resolve().then(fn).catch(() => {}); };
  let annoCreate = null; // (kind, x, y), also set by the annotations block

  function keyOf(el) {
    if (el.dataset.tucked || el.dataset.ephemeral) return null; // ride-alongs can't be grouped
    if (el.classList.contains("graph-node")) return "node:" + el.dataset.nodeId;
    // an annotation drawn moments ago has no server id yet, it just doesn't join
    if (el.dataset.annoId) return "anno:" + el.dataset.annoId;
    return null;
  }
  function elOfKey(k) {
    if (k.startsWith("node:")) return nodeByID(k.slice(5));
    if (k.startsWith("anno:")) return world.querySelector(`.graph-anno[data-anno-id="${cssEsc(k.slice(5))}"]`);
    return null;
  }
  function groupOf(el) {
    const k = keyOf(el);
    return (k && groupList.find((g) => g.members.includes(k))) || null;
  }
  // The set a drag actually moves: each element joined by its group, unless
  // that group is the entered one (then its members move alone).
  function expandDragSet(els) {
    const out = new Set(els);
    els.forEach((el) => {
      const g = groupOf(el);
      if (!g || g.id === entered) return;
      g.members.forEach((k) => {
        const m = elOfKey(k);
        if (m) out.add(m);
      });
    });
    return [...out];
  }

  function saveGroup(g) {
    annoQueue(() =>
      postJSON(groupsURL, { id: g.id, members: g.members })
        .then((r) => (r.ok ? r.json() : null))
        .then((d) => { if (d && d.id) g.id = d.id; })
    );
  }
  function removeGroup(g) {
    if (entered === g.id) setEntered(null);
    groupList = groupList.filter((x) => x !== g);
    annoQueue(() => { if (g.id) return postJSON(groupsURL + "/delete", { id: g.id }); });
  }
  // membership is exclusive: joining a new group leaves the old one, and a
  // group thinned below two members stops being a group
  function removeMember(k) {
    const g = groupList.find((x) => x.members.includes(k));
    if (!g) return;
    g.members = g.members.filter((m) => m !== k);
    if (g.members.length < 2) removeGroup(g);
    else saveGroup(g);
  }
  function groupSelection() {
    const keys = [...selected].map(keyOf).filter(Boolean);
    if (keys.length < 2) return;
    keys.forEach(removeMember);
    const g = { id: "", members: keys };
    groupList.push(g);
    saveGroup(g);
    clearSelection();
  }

  // --- entered group: double-click in, Escape / bare canvas out ---
  let entered = null;
  function setEntered(id) {
    entered = id;
    world.querySelectorAll(".graph-group-open").forEach((n) => n.classList.remove("graph-group-open"));
    const g = id && groupList.find((x) => x.id === id);
    if (g) g.members.forEach((k) => elOfKey(k)?.classList.add("graph-group-open"));
  }
  world.addEventListener("dblclick", (e) => {
    const el = e.target.closest(".graph-node, .graph-anno");
    const g = el && groupOf(el);
    if (g && g.id) {
      setEntered(g.id);
      closeDrawer(); // the dblclick's first click opened the card's drawer, entering means arranging, not reading
    }
  });
  escapeFns.push(() => setEntered(null));

  // --- right-click menu ---
  const menu = document.getElementById("graph-menu");
  let menuTarget = null, menuAt = [0, 0];
  function hideMenu() {
    if (menu) menu.hidden = true;
  }
  viewport.addEventListener("contextmenu", (e) => {
    if (!menu || e.target.closest(".graph-ctrl, #graph-menu")) return;
    e.preventDefault(); // the canvas owns right-click; an empty menu just doesn't show
    const el = e.target.closest(".graph-node, .graph-anno");
    const show = {
      group: selected.size >= 2,
      ungroup: !!(el && groupOf(el)),
      "add-text": !el,
      "add-box": !el,
    };
    if (!Object.values(show).some(Boolean)) return;
    menu.querySelectorAll("[data-menu]").forEach((b) => { b.hidden = !show[b.dataset.menu]; });
    const rect = viewport.getBoundingClientRect();
    menu.style.left = e.clientX - rect.left + "px";
    menu.style.top = e.clientY - rect.top + "px";
    menu.hidden = false;
    menuTarget = el;
    menuAt = toWorld(e.clientX, e.clientY);
  });
  menu?.addEventListener("click", (e) => {
    const b = e.target.closest("[data-menu]");
    if (!b) return;
    hideMenu();
    switch (b.dataset.menu) {
      case "group": groupSelection(); break;
      case "ungroup": { const g = menuTarget && groupOf(menuTarget); if (g) removeGroup(g); break; }
      case "add-text": annoCreate?.("text", menuAt[0], menuAt[1]); break;
      case "add-box": annoCreate?.("box", menuAt[0], menuAt[1]); break;
    }
  });
  // any press outside the menu dismisses it, before the press does its own work
  document.addEventListener("pointerdown", (e) => {
    if (!e.target.closest("#graph-menu")) hideMenu();
  }, true);
  escapeFns.push(hideMenu);

  // Ctrl+G groups the selection, Ctrl+Shift+G ungroups whatever is selected.
  document.addEventListener("keydown", (e) => {
    if (!(e.metaKey || e.ctrlKey) || e.key.toLowerCase() !== "g") return;
    const t = e.target;
    if (t && (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName))) return;
    e.preventDefault();
    if (e.shiftKey) new Set([...selected].map(groupOf).filter(Boolean)).forEach(removeGroup);
    else groupSelection();
  });

  // Nodes persist through node_positions, annotations through their own
  // endpoint, a moved mixed set (group / marquee drag, undo) splits here.
  function saveMoved(els) {
    persistNodes(els.filter((n) => n.classList.contains("graph-node")));
    els.forEach((n) => { if (n.classList.contains("graph-anno")) annoPersist(n); });
  }
  let annoPersist = () => {}; // set by the annotations block below

  // --- node drag ---
  let drag = null;
  // system cards move freely within the server zone but never past the
  // divider into the stack's side
  function maxSystemX() {
    const work = [...world.querySelectorAll(".graph-node:not(.graph-node-system)")];
    if (!work.length) return Infinity;
    const left = Math.min(...work.map((n) => leftOf(n)));
    return left - NODE_W - GUTTER_X;
  }
  // ...and the mirror: workload cards never cross left past the divider into
  // the server zone
  function minWorkloadX() {
    const sys = [...world.querySelectorAll(".graph-node-system")];
    if (!sys.length) return -Infinity;
    const right = Math.max(...sys.map((n) => (leftOf(n)) + NODE_W));
    return right + GUTTER_X;
  }
  // the divider is a hard wall in both directions; every code path that places
  // a card goes through here
  function clampX(node, x) {
    if (!node.classList.contains("graph-node")) return x; // annotations roam free of the divider
    return node.classList.contains("graph-node-system")
      ? Math.min(x, maxSystemX())
      : Math.max(x, minWorkloadX());
  }

  // --- keep cards from overlapping (opt-in, applied on drop only) ---
  function overlaps(x, y, self) {
    return [...world.querySelectorAll(".graph-node")].some((o) => {
      if (o === self) return false;
      const ox = leftOf(o), oy = topOf(o);
      return x < ox + NODE_W + CARD_GAP && ox < x + NODE_W + CARD_GAP &&
        y < oy + NODE_H + CARD_GAP && oy < y + NODE_H + CARD_GAP;
    });
  }
  // move a card and everything that follows it: its sub-tiles, the edges,
  // the divider. The sub-tile offsets must match subtileTop() in
  // components/canvas/canvas.templ, same deck, drawn twice.
  function placeCard(node, x, y) {
    node.style.left = x + "px";
    node.style.top = y + "px";
    world.querySelectorAll('[data-parent="' + node.dataset.nodeId + '"]').forEach((v, k) => {
      v.style.left = (x + (k + 1) * SUBTILE_OFFSET) + "px";
      v.style.top = (y + (k + 1) * SUBTILE_PEEK) + "px";
    });
    redrawEdges();
    updateDivider();
  }

  const NUDGE_DIRS = [[1, 0], [0, 1], [-1, 0], [0, -1], [1, 1], [1, -1], [-1, 1], [-1, -1]];
  // Nearest free spot for a card just dropped on top of another, searched
  // outward so it lands as close to where it was let go as possible. Only the
  // dropped card ever moves, the rest of the layout is the user's.
  // gives up after 12 rings and leaves the card where it fell, rather
  // than flinging it off to the empty edge of a crowded canvas.
  function freeSpot(node, x, y) {
    if (!settings.nooverlap || !overlaps(x, y, node)) return [x, y];
    const step = settings.snap ? GRID_PX : 20; // the grid, so snapping still holds
    for (let ring = 1; ring <= 12; ring++) {
      for (const [sx, sy] of NUDGE_DIRS) {
        const nx = clampX(node, x + sx * ring * step), ny = y + sy * ring * step;
        if (!overlaps(nx, ny, node)) return [nx, ny];
      }
    }
    return [x, y];
  }
  // Sub-tiles are drawings under a card, not cards: they get no node
  // behaviour, just this one gesture, open whatever the sub-tile stands for.
  // Delegated, so it survives the poller rewriting a card's insides.
  function openSubtile(e) {
    const sub = e.target.closest("[data-subtile-panel]");
    if (!sub) return false;
    e.stopPropagation();
    openDrawer(sub.dataset.subtilePanel);
    return true;
  }
  world.addEventListener("click", openSubtile);
  world.addEventListener("keydown", (e) => {
    if (e.key !== "Enter" && e.key !== " ") return;
    if (openSubtile(e)) e.preventDefault();
  });

  world.querySelectorAll(".graph-node").forEach((node) => {
    // belt-and-suspenders against native link drag-and-drop
    node.addEventListener("dragstart", (e) => e.preventDefault());
    node.addEventListener("pointerdown", (e) => {
      if (e.button === 2) return; // right-click opens the context menu, never a drag
      e.stopPropagation();
      if (activeTouches.size >= 2) return; // pinching, not dragging
      if (node.dataset.tucked) return; // attached volumes ride their parent
      if (node.dataset.ephemeral) return; // forward cards sit where their target is; not draggable
      // Dragging a selected card moves the whole selection; dragging an
      // unselected one is a plain single-card drag and drops the selection,
      // so a stale marquee can never drag cards you forgot were picked.
      let group;
      if (selected.has(node)) {
        group = [...selected];
      } else {
        clearSelection();
        group = [node];
      }
      group = expandDragSet(group); // grouped members ride along
      drag = {
        node,
        id: node.dataset.nodeId,
        group,
        // Where each card started, so the whole set moves by one delta.
        origins: group.map((n) => [n, leftOf(n), topOf(n)]),
        moved: false,
        // fingers jitter, require real travel before the card moves
        slop: e.pointerType === "touch" ? 10 : 3,
        startX: e.clientX,
        startY: e.clientY,
        origX: leftOf(node),
        origY: topOf(node),
      };
      try { node.setPointerCapture(e.pointerId); } catch (_) {}
    });
    node.addEventListener("pointermove", (e) => {
      if (!drag || drag.node !== node || pinchDist > 0) return;
      const dx = (e.clientX - drag.startX) / scale;
      const dy = (e.clientY - drag.startY) / scale;
      if (Math.abs(dx) > drag.slop || Math.abs(dy) > drag.slop) drag.moved = true;
      if (!drag.moved) return; // below slop: treat as a tap, don't move the card
      // snapshot before the first throttled share below: once the server sees a
      // single saved card it re-anchors the still-unsaved neighbours, and the
      // ws echo would scramble the canvas mid-drag (see persistAll)
      if (!drag.snapshotted) {
        drag.snapshotted = true;
        pushHistory(snapshot(drag.group)); // undo restores where the drag began
        persistAll();
      }
      // snap to the dotted background grid
      let x = drag.origX + dx, y = drag.origY + dy;
      if (settings.snap) {
        x = Math.round(x / GRID_PX) * GRID_PX;
        y = Math.round(y / GRID_PX) * GRID_PX;
      }
      x = clampX(node, x); // never past the divider
      // The grabbed card decides the delta, so snapping stays predictable and
      // the group keeps its internal spacing exactly.
      const gdx = x - drag.origX, gdy = y - drag.origY;
      drag.origins.forEach(([n, ox, oy]) => {
        placeCard(n, n === node ? x : clampX(n, ox + gdx), n === node ? y : oy + gdy);
      });
      // live-share the drag: throttled saves push over the ws room so other
      // open canvases watch the card move in real time
      const now = Date.now();
      if (drag.moved && (!drag.lastShare || now - drag.lastShare > 250)) {
        drag.lastShare = now;
        persist(drag.id, x, y);
      }
    });
    node.addEventListener("pointerup", (e) => {
      if (!drag || drag.node !== node) return;
      try { node.releasePointerCapture(e.pointerId); } catch (_) {}
      if (drag.moved) {
        node.dataset.suppressClick = "1"; // the click event fires right after; swallow it
        drag.group.forEach((n) => { n.dataset.saved = "1"; });
        if (drag.group.length === 1) {
          // nudge off any card it landed on before saving, so what gets persisted
          // is what the user is looking at
          const [x, y] = freeSpot(node, leftOf(node), topOf(node));
          placeCard(node, x, y);
        }
        // No nudging on a group drop: the selection's internal layout is
        // deliberate, and shoving members apart to satisfy no-overlap would
        // destroy the arrangement the user just moved.
        saveMoved(drag.group);
        persistAll();
      }
      drag = null;
    });
    // a genuine click (no drag) opens the service drawer
    node.addEventListener("click", (e) => {
      // nav cards are real <a href> links: hand ctrl/cmd/shift/middle-click
      // straight to the browser so "open in new tab" works as it looks like
      // it should.
      if (node.dataset.navUrl && (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button !== 0)) {
        return;
      }
      if (node.dataset.suppressClick === "1") {
        node.dataset.suppressClick = "0";
        e.preventDefault(); // the drag moved the card; don't also follow the href
        return;
      }
      if (e.target.closest("a[data-domain-link]")) return; // navigating, not opening
      if (node.dataset.navUrl) {
        rememberDeck(node, node.dataset.navUrl); // the destination deals out of this card
        return; // real <a href>, the browser navigates, the view transition zooms
      }
      if (node.dataset.panelUrl) openDrawer(node.dataset.panelUrl);
    });
    node.addEventListener("keydown", (e) => {
      if (e.key !== "Enter" && e.key !== " ") return;
      // Nav cards are anchors, Enter activates them natively. Space is the
      // gap (anchors ignore it), so synthesise a click: one code path keeps
      // the modifier handling in the click listener authoritative.
      if (node.dataset.navUrl) {
        if (e.key === " ") { e.preventDefault(); node.click(); }
        return;
      }
      e.preventDefault();
      if (node.dataset.panelUrl) openDrawer(node.dataset.panelUrl);
    });
  });

  // --- right service drawer ---
  const drawer = document.getElementById("drawer");
  const drawerBody = document.getElementById("drawer-body");
  const backdrop = document.getElementById("drawer-backdrop");

  // The open is async (htmx fetch): a close issued while it's in flight must
  // win, or a dblclick that enters a group gets the drawer landing on top of
  // it anyway. The counter invalidates stale opens.
  let drawerGen = 0;
  function openDrawer(url) {
    if (!url || !drawer || !window.htmx) return;
    const gen = ++drawerGen;
    window.htmx.ajax("GET", url, { target: "#drawer-body", swap: "innerHTML" }).then(() => {
      if (gen !== drawerGen) return; // closed while loading
      drawer.classList.add("open");
      backdrop.classList.add("open");
    });
  }
  function closeDrawer() {
    drawerGen++;
    drawer?.classList.remove("open");
    document.getElementById("graph-settings-drawer")?.classList.remove("open");
    backdrop?.classList.remove("open");
  }
  backdrop?.addEventListener("click", closeDrawer);
  // --- "+ Create" modal: tabs, source switching, repo picker autofill ---
  const createMenu = document.getElementById("create-menu");
  document.addEventListener("click", (e) => {
    if (e.target.closest("[data-create-toggle]")) {
      createMenu?.classList.toggle("hidden");
      return;
    }
    if (e.target.closest("[data-create-close]")) {
      createMenu?.classList.add("hidden");
      return;
    }
    const tabBtn = e.target.closest("[data-ctab]");
    if (tabBtn && createMenu) {
      createMenu.querySelectorAll("[data-ctab]").forEach((b) => {
        const on = b === tabBtn;
        b.classList.toggle("text-rw-text", on);
        b.classList.toggle("border-rw-accent", on);
        b.classList.toggle("text-rw-muted", !on);
        b.classList.toggle("border-transparent", !on);
      });
      createMenu.querySelectorAll("[data-cpane]").forEach((p) => {
        p.classList.toggle("hidden", p.dataset.cpane !== tabBtn.dataset.ctab);
      });
    }
  });
  escapeFns.push(() => createMenu?.classList.add("hidden"));
  createMenu?.addEventListener("change", (e) => {
    const form = e.target.closest("form");
    // source select shows only its field groups
    if (e.target.matches("[data-create-source]")) {
      form.querySelectorAll("[data-src]").forEach((d) => {
        d.classList.toggle("hidden", !d.dataset.src.split(" ").includes(e.target.value));
      });
      if (e.target.value !== "git") {
        form.querySelector("[name=connector_id]").value = "";
        const pick = form.querySelector("[data-repo-pick]");
        if (pick) pick.value = "";
      }
    }
    // repo pick fills connector, url, branch, and a default name
    if (e.target.matches("[data-repo-pick]") && e.target.value) {
      const [connID, url, branch] = e.target.value.split("|");
      form.querySelector("[name=connector_id]").value = connID;
      form.querySelector("[name=git_url]").value = url;
      form.querySelector("[name=git_branch]").value = branch || "main";
      const name = form.querySelector("[name=name]");
      if (!name.value) name.value = url.split("/").pop();
    }
  });

  // --- canvas settings drawer (same sliding pane as the service panel) ---
  const settingsDrawer = document.getElementById("graph-settings-drawer");
  if (settingsDrawer) {
    settingsDrawer.querySelectorAll("[data-gs]").forEach((cb) => {
      cb.checked = settings[cb.dataset.gs];
      cb.addEventListener("change", () => {
        settings[cb.dataset.gs] = cb.checked;
        saveSettings();
        applySettings();
        if (cb.dataset.gs === "fan") redrawEdges();
      });
    });
    // radio groups (edge shape): the value is the setting, not a boolean
    settingsDrawer.querySelectorAll("[data-gs-choice]").forEach((rb) => {
      rb.checked = settings[rb.dataset.gsChoice] === rb.value;
      rb.addEventListener("change", () => {
        if (!rb.checked) return;
        settings[rb.dataset.gsChoice] = rb.value;
        saveSettings();
        redrawEdges();
      });
    });
    document.addEventListener("click", (e) => {
      if (e.target.closest("[data-graph-settings-toggle]")) {
        closeDrawer(); // never two panes at once
        settingsDrawer.classList.add("open");
        backdrop?.classList.add("open");
      }
    });
    settingsDrawer.querySelector("[data-gs-reset]")?.addEventListener("click", async () => {
      // a just-flipped arrange radio races its own fire-and-forget save,
      // make sure the server has the style before it re-lays the canvas out
      await postJSON(PREFS_URL, settings, "PUT").catch(() => {});
      await fetch(saveURL + "/reset", { method: "POST", headers: { "X-CSRF-Token": csrf } });
      location.reload();
    });
  }
  redrawEdges(); // the server renders plain curves; honour the saved edge settings
  applySettings();

  document.addEventListener("click", (e) => {
    if (e.target.closest("[data-drawer-close]")) closeDrawer();

    // Variables-tab reveal/edit toggles live in main.js: they are needed on the
    // standalone app page too, where graph.js never loads.
  });
  escapeFns.push(closeDrawer);

  function nodeByID(id) {
    return world.querySelector(`.graph-node[data-node-id="${cssEsc(id)}"]`);
  }
  // Server-rendered edges touching one node, either end. Lane clones never
  // match: applyTraffic strips their data-edge-from/to exactly so id lookups
  // like this one can't pick them up.
  function edgesOf(id) {
    return world.querySelectorAll(
      `path[data-edge-from="${cssEsc(id)}"], path[data-edge-to="${cssEsc(id)}"]`
    );
  }

  // Anchors are side-picked from the cards' relative positions: left/right when
  // the run is mostly horizontal, top/bottom when mostly vertical. Mirrors
  // bezier() in graph.templ, which is what the server renders.
  function edgeEnds(p) {
    const a = nodeByID(p.dataset.edgeFrom), b = nodeByID(p.dataset.edgeTo);
    if (!a || !b) return null;
    const ax = leftOf(a), ay = topOf(a);
    const bx = leftOf(b), by = topOf(b);
    const dx = (bx + NODE_W / 2) - (ax + NODE_W / 2);
    const dy = (by + NODE_H / 2) - (ay + NODE_H / 2);
    const horiz = Math.abs(dx) >= Math.abs(dy);
    const e = { p, horiz };
    if (horiz) {
      const back = dx < 0;
      e.x1 = back ? ax : ax + NODE_W;
      e.x2 = back ? bx + NODE_W : bx;
      e.y1 = ay + NODE_H / 2;
      e.y2 = by + NODE_H / 2;
      e.k = Math.max(Math.abs(dx) / 2, 60) * (back ? -1 : 1);
      e.sideA = back ? "L" : "R";
      e.sideB = back ? "R" : "L";
      // where the far end sits along the side, so a fanned bundle doesn't cross
      e.alongA = by;
      e.alongB = ay;
    } else {
      const up = dy < 0;
      e.y1 = up ? ay : ay + NODE_H;
      e.y2 = up ? by + NODE_H : by;
      e.x1 = ax + NODE_W / 2;
      e.x2 = bx + NODE_W / 2;
      e.k = Math.max(Math.abs(dy) / 2, 60) * (up ? -1 : 1);
      e.sideA = up ? "T" : "B";
      e.sideB = up ? "B" : "T";
      e.alongA = bx;
      e.alongB = ax;
    }
    e.keyA = p.dataset.edgeFrom + e.sideA;
    e.keyB = p.dataset.edgeTo + e.sideB;
    return e;
  }

  // Every edge meeting one side of a card anchors at that side's midpoint, so a
  // card with several of them (the proxy in front of five services, one
  // database behind three apps) draws them all out of the same point and the
  // lines overlap into a single stripe near the card. Deal those anchors out
  // along the side instead, ordered by where each far end sits so the fanned
  // lines don't cross each other.
  function spreadAnchors(edges) {
    const groups = new Map();
    for (const e of edges) {
      for (const end of ["A", "B"]) {
        const k = e["key" + end];
        const g = groups.get(k) || [];
        g.push([e, end]);
        groups.set(k, g);
      }
    }
    for (const g of groups.values()) {
      if (g.length < 2) continue;
      g.sort((m, n) => m[0]["along" + m[1]] - n[0]["along" + n[1]]);
      // one side only has so much room to give: a card is NODE_H tall
      const span = (g[0][0].horiz ? NODE_H : NODE_W) * FAN_SPAN;
      const gap = Math.min(FAN_GAP, span / (g.length - 1));
      g.forEach(([e, end], i) => {
        const off = (i - (g.length - 1) / 2) * gap;
        if (e.horiz) e[end === "A" ? "y1" : "y2"] += off;
        else e[end === "A" ? "x1" : "x2"] += off;
      });
    }
  }

  function pathOf(e) {
    const r = (v) => Math.round(v * 10) / 10;
    if (settings.edges === "straight") {
      return `M ${r(e.x1)} ${r(e.y1)} L ${r(e.x2)} ${r(e.y2)}`;
    }
    if (e.horiz) {
      return `M ${r(e.x1)} ${r(e.y1)} C ${r(e.x1 + e.k)} ${r(e.y1)}, ${r(e.x2 - e.k)} ${r(e.y2)}, ${r(e.x2)} ${r(e.y2)}`;
    }
    return `M ${r(e.x1)} ${r(e.y1)} C ${r(e.x1)} ${r(e.y1 + e.k)}, ${r(e.x2)} ${r(e.y2 - e.k)}, ${r(e.x2)} ${r(e.y2)}`;
  }

  // Redraw every edge. Needed on load as well as after a drag or a settings
  // change: the server renders plain unfanned curves, so straight mode and
  // anchor spreading only exist once this has run.
  function redrawEdges() {
    const edges = [];
    world.querySelectorAll(BASE_EDGES).forEach((p) => {
      const e = edgeEnds(p);
      if (e) edges.push(e);
    });
    if (settings.fan) spreadAnchors(edges);
    for (const e of edges) {
      e.p.setAttribute("d", pathOf(e));
      syncLanes(e.p);
    }
  }

  // --- hover focus: light one card's edges, drop everything else back ---
  //
  // Pure DOM classes, no redraw: a canvas can hold a few hundred paths and
  // re-pathing them on every pointer move would be the one thing that makes
  // this canvas feel slow. Delegated on the world rather than bound per card so
  // it survives the poller adding and removing ephemeral cards.
  function setFocus(node) {
    if (node === focusNode) return;
    focusNode = node;
    world.querySelectorAll(".is-focus, .is-linked, .is-lit").forEach((el) => {
      el.classList.remove("is-focus", "is-linked", "is-lit");
    });
    world.classList.toggle("graph-focusing", !!node);
    if (!node) return;
    node.classList.add("is-focus");
    const id = node.dataset.nodeId;
    const near = new Set([id]);
    edgesOf(id).forEach((p) => {
      if (edgeHidden(p)) return; // hidden by a toggle: stays hidden
      p.classList.add("is-lit");
      // A busy edge's traffic lanes and rate label are separate elements, so
      // they need lighting too or the edge dims to a bright outline with a
      // faded pulse riding it.
      (laneReg.get(p) || []).forEach((l) => {
        l.el.classList.add("is-lit");
        l.label.classList.add("is-lit");
      });
      near.add(p.dataset.edgeFrom);
      near.add(p.dataset.edgeTo);
    });
    near.forEach((n) => {
      const el = n && n !== id ? nodeByID(n) : null;
      if (el) el.classList.add("is-linked");
    });
    // Sub-tiles are drawn under their card; dimming the card and not its deck
    // would peel them apart.
    near.forEach((n) => {
      world.querySelectorAll(`.graph-node-sub[data-parent="${cssEsc(n)}"]`)
        .forEach((s) => s.classList.add("is-linked"));
    });
  }
  world.addEventListener("pointerover", (e) => {
    // Not while dragging: the whole canvas dimming under a card being moved is
    // noise, and the drop lands on a card the user can no longer see.
    if (!settings.focus || drag) { setFocus(null); return; }
    setFocus(e.target.closest(".graph-node"));
  });
  world.addEventListener("pointerleave", () => setFocus(null));

  // Deep-link focus (?focus=<node id>): center the card and light it exactly
  // as hovering it would. Search results land here. Runs regardless of the
  // hover-focus setting, the centering is the point; the first real pointer
  // move takes over the highlight either way.
  (function () {
    const id = new URLSearchParams(location.search).get("focus");
    if (!id) return;
    const el = nodeByID(id);
    if (!el) return;
    const rect = viewport.getBoundingClientRect();
    const cx = (leftOf(el)) + NODE_W / 2;
    const cy = (topOf(el)) + NODE_H / 2;
    panX = rect.width / 2 - cx * scale;
    panY = rect.height / 2 - cy * scale;
    clampPan();
    applyTransform();
    setFocus(el);
  })();

  // Position saves go out strictly in order. A drag fires a throttled save
  // mid-flight and another on drop; as independent fetches those can land in
  // either order, and when the stale one lands last the card snaps back to
  // where it was passing through. Chaining them costs nothing here, saves are
  // small, rare, and already fire-and-forget.
  let postChain = Promise.resolve();
  function post(body) {
    postChain = postChain.then(() => postJSON(saveURL, body).catch(() => {}));
  }
  function persist(nodeID, x, y) {
    post({ node_id: nodeID, x, y });
  }

  // On a canvas that still holds auto-laid-out cards, the first drag must save
  // EVERY card, not just the dragged one: the server's layout can't tell "never
  // dragged" from "created later", so a lone saved card makes it re-anchor its
  // neighbours beside it (the first-move jump). Tucked volumes are excluded,
  // they always ride their parent, saved positions are ignored for them.
  function persistAll() {
    // ephemeral (forward) cards take no node_positions row, see AddForwards
    const nodes = [...world.querySelectorAll(".graph-node:not([data-ephemeral])")];
    if (nodes.every((n) => n.dataset.saved === "1")) return;
    nodes.forEach((n) => { n.dataset.saved = "1"; });
    post({
      nodes: nodes.map((n) => ({
        node_id: n.dataset.nodeId,
        x: leftOf(n),
        y: topOf(n),
      })),
    });
  }

  // Save an explicit set of cards in one request, the group-drag and undo
  // path. persistAll can't stand in: it no-ops once every card is marked
  // saved, which is exactly when undo needs to write.
  function persistNodes(nodes) {
    const live = nodes.filter((n) =>
      n.isConnected && n.classList.contains("graph-node") && !n.dataset.tucked && !n.dataset.ephemeral);
    if (!live.length) return;
    post({
      nodes: live.map((n) => ({
        node_id: n.dataset.nodeId,
        x: leftOf(n),
        y: topOf(n),
      })),
    });
  }

  // --- undo/redo, positions only ---
  //
  // Moving a card is the only mutation the canvas itself owns, creating,
  // deleting and configuring all go through drawers and the staging flow, and
  // undoing those belongs to staging, not here. So the history is just
  // positions, in memory, and it dies with the page.
  const history = [], future = [];
  const HISTORY_MAX = 50;
  function snapshot(nodes) {
    return nodes.map((n) => ({ node: n, x: leftOf(n), y: topOf(n) }));
  }
  function pushHistory(snap) {
    history.push(snap);
    if (history.length > HISTORY_MAX) history.shift();
    future.length = 0; // a fresh move forks the timeline
  }
  function restore(snap) {
    const live = snap.filter((s) => s.node.isConnected);
    live.forEach((s) => placeCard(s.node, s.x, s.y));
    // Re-post rather than trust the server: pollStatus overwrites positions
    // from the server on ws events, so an un-posted undo would be undone.
    saveMoved(live.map((s) => s.node));
  }
  function undo() {
    const past = history.pop();
    if (!past) return;
    future.push(snapshot(past.map((s) => s.node)));
    restore(past);
  }
  function redo() {
    const next = future.pop();
    if (!next) return;
    history.push(snapshot(next.map((s) => s.node)));
    restore(next);
  }
  document.addEventListener("keydown", (e) => {
    if (!(e.metaKey || e.ctrlKey) || e.key.toLowerCase() !== "z") return;
    // Never steal the shortcut from a field the user is typing in.
    const t = e.target;
    if (t && (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName))) return;
    e.preventDefault();
    if (e.shiftKey) redo(); else undo();
  });

  // --- cross-document view transitions (Chrome/Safari; others ignore) ---
  // css/input.css opts navigations in with @view-transition. Here we pick the
  // "drill" pair: whichever page has a card that navigates to the other page
  // names the card, and the other page names its whole viewport, so drilling
  // in morphs the card into the canvas, and going back (including the browser
  // back button) shrinks the canvas into its card.
  function nameDrill(otherURL) {
    viewport.style.viewTransitionName = "";
    world.querySelectorAll("[style*='view-transition-name']").forEach((n) => {
      n.style.viewTransitionName = "";
    });
    let path;
    try { path = new URL(otherURL, location.href).pathname; } catch (_) { return; }
    const card = world.querySelector(`.graph-node[data-nav-url="${cssEsc(path)}"]`);
    (card || viewport).style.viewTransitionName = "drill";
    return card || viewport;
  }
  window.addEventListener("pageswap", (e) => {
    if (!e.viewTransition || !e.activation) return;
    const el = nameDrill(e.activation.entry.url);
    // named the viewport rather than a card: nothing here points at where we're
    // going, so we're heading up a level and the arriving page will zoom out.
    // Hand it our cards so it can gather them into the one they came from.
    if (el === viewport) nameCardsForGather(e.activation.entry.url);
  });
  window.addEventListener("pagereveal", (e) => {
    if (!e.viewTransition || !window.navigation?.activation?.from) return;
    const el = nameDrill(navigation.activation.from.url);
    if (el && el !== viewport) gatherToDeck(e.viewTransition, el);
    // stale names would pair the wrong elements on the NEXT navigation
    if (el) e.viewTransition.finished.finally(() => { el.style.viewTransitionName = ""; });
  });

  // --- deal-out: arriving from a card click, this canvas's cards slide out of
  // where that card sat, the way cards come off a deck in Gwent or Hearthstone.
  // The source page stores the clicked card's screen rect; the destination
  // animates its own cards from that rect out to their real positions, exact,
  // not approximate, because only the destination knows its own layout. Runs
  // alongside the view-transition zoom, and on its own where that's unsupported.
  const DECK_KEY = "stackr.graph.deck";
  const DEAL_MS = 320, DEAL_STEP = 30, DEAL_STAGGER_MAX = 300;

  function rememberDeck(node, url) {
    const r = node.getBoundingClientRect();
    try {
      sessionStorage.setItem(DECK_KEY, JSON.stringify({
        url: new URL(url, location.href).pathname,
        x: r.x, y: r.y, w: r.width, h: r.height,
      }));
    } catch (_) {} // private mode / quota: no deal, just a plain arrival
  }

  function dealFromDeck() {
    let deck = null;
    try {
      deck = JSON.parse(sessionStorage.getItem(DECK_KEY) || "null");
      sessionStorage.removeItem(DECK_KEY); // one click, one deal
    } catch (_) { return; }
    // the URL guard keeps a stale rect from dealing a page the user reached
    // some other way (back button, reload, typed address)
    if (!deck || deck.url !== location.pathname) return;
    if (window.matchMedia("(prefers-reduced-motion: reduce)").matches) return;

    const cx = deck.x + deck.w / 2, cy = deck.y + deck.h / 2;
    // deltas are divided by scale: a child's transform is in world px, which
    // the world's own scale() multiplies on screen
    const cards = [...world.querySelectorAll(".graph-node")].map((c) => {
      const r = c.getBoundingClientRect();
      return {
        el: c,
        dx: (cx - (r.x + r.width / 2)) / scale,
        dy: (cy - (r.y + r.height / 2)) / scale,
        k: r.width ? deck.w / r.width : 1,
        dist: Math.hypot(cx - (r.x + r.width / 2), cy - (r.y + r.height / 2)),
      };
    });
    cards.sort((a, b) => a.dist - b.dist); // nearest the deck comes off it first
    cards.forEach((c, i) => {
      c.el.animate(
        [
          { transform: `translate(${c.dx}px, ${c.dy}px) scale(${c.k}) rotate(-4deg)`, opacity: 0.5 },
          { transform: "none", opacity: 1 },
        ],
        {
          duration: DEAL_MS,
          delay: Math.min(i * DEAL_STEP, DEAL_STAGGER_MAX),
          easing: "cubic-bezier(0.2, 0, 0.2, 1)",
          fill: "backwards", // hold at the deck through the stagger delay
        },
      );
    });
    // edges are already drawn at the cards' final positions, so they'd hang in
    // space pointing at nothing: hold them back until the cards are nearly home
    const svg = world.querySelector("svg");
    if (svg && cards.length) {
      svg.animate([{ opacity: 0 }, { opacity: 0 }, { opacity: 1 }], {
        duration: Math.min((cards.length - 1) * DEAL_STEP, DEAL_STAGGER_MAX) + DEAL_MS,
        easing: "linear",
      });
    }
  }

  if ("onpagereveal" in window) {
    window.addEventListener("pagereveal", dealFromDeck);
  } else {
    dealFromDeck(); // no view transitions: the deal is the whole animation
  }

  // --- gather: the deal in reverse, for going back up a level ---
  // The departing page can't do this itself, an animation started in pageswap
  // never paints, since that is the moment the old page is snapshotted. So the
  // departing page instead names every card, making each a separate snapshot,
  // and records where it sat. The arriving page is the only side that knows
  // where the deck card is, so it animates those snapshots into it, in step
  // with the canvas shrinking into the same card.
  const GATHER_KEY = "stackr.graph.gather";
  // travel + stagger is kept to the 300ms of ::view-transition-group(drill) in
  // css/input.css: cards must be in the deck by the time the canvas is
  const GATHER_MS = 240, GATHER_STEP = 20, GATHER_STAGGER_MAX = 60;

  function nameCardsForGather(destURL) {
    let url;
    try { url = new URL(destURL, location.href).pathname; } catch (_) { return; }
    const rects = [...world.querySelectorAll(".graph-node")].map((c, i) => {
      const r = c.getBoundingClientRect();
      const name = "gather" + i;
      c.style.viewTransitionName = name; // nameDrill clears these next time
      return { name, x: r.x, y: r.y, w: r.width, h: r.height };
    });
    try {
      sessionStorage.setItem(GATHER_KEY, JSON.stringify({ url, rects }));
    } catch (_) {} // private mode / quota: the canvas still zooms out
  }

  function gatherToDeck(vt, deckEl) {
    let g = null;
    try {
      g = JSON.parse(sessionStorage.getItem(GATHER_KEY) || "null");
      sessionStorage.removeItem(GATHER_KEY); // one departure, one gather
    } catch (_) { return; }
    if (!g || g.url !== location.pathname || !g.rects?.length) return;
    if (window.matchMedia("(prefers-reduced-motion: reduce)").matches) return;

    // screen coordinates on both sides: transition snapshots sit in the
    // viewport, outside #graph-world, so the world's scale doesn't apply
    const d = deckEl.getBoundingClientRect();
    const cx = d.x + d.width / 2, cy = d.y + d.height / 2;
    const cards = g.rects
      .map((r) => {
        const dx = cx - (r.x + r.w / 2), dy = cy - (r.y + r.h / 2);
        return { name: r.name, dx, dy, k: r.w ? d.width / r.w : 1, dist: Math.hypot(dx, dy) };
      })
      .sort((a, b) => b.dist - a.dist); // farthest sets off first, lands with the rest

    vt.ready.then(() => {
      cards.forEach((c, i) => {
        try {
          document.documentElement.animate(
            [
              { transform: "none", opacity: 1 },
              { transform: `translate(${c.dx}px, ${c.dy}px) scale(${c.k}) rotate(-4deg)`, opacity: 0 },
            ],
            {
              duration: GATHER_MS,
              delay: Math.min(i * GATHER_STEP, GATHER_STAGGER_MAX),
              easing: "cubic-bezier(0.2, 0, 0.2, 1)",
              fill: "both", // stay in the deck until the transition ends
              pseudoElement: `::view-transition-group(${c.name})`,
            },
          );
        } catch (_) {} // no pseudo-element animation: the card just fades out
      });
    }).catch(() => {}); // a skipped transition rejects ready; nothing to undo
  }

  function cssEsc(s) {
    return window.CSS && CSS.escape ? CSS.escape(s) : s.replace(/["\\]/g, "\\$&");
  }

  // --- live status: patch card footers in place on every ws nudge ---
  // No interval: the server pushes on deploy, stop/start, and the 30s metrics
  // reconciler (see the stackr:ws binding at the bottom of this file).
  // Markup mirrors nodeStatus/cronStatus in graph.templ, keep in sync.
  const statusURL = saveURL.replace(/\/positions$/, "/status");
  const esc = (s) => String(s ?? "").replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
  function fmtRate(bps) {
    if (bps >= 1 << 20) return (bps / (1 << 20)).toFixed(1) + " MB/s";
    if (bps >= 1 << 10) return (bps / (1 << 10)).toFixed(1) + " KB/s";
    return bps.toFixed(0) + " B/s";
  }
  // Directional traffic pulses: dashes flow along the edge in the direction
  // of the observed bytes. Path geometry runs edge.from -> edge.to, so a flow
  // matching that order animates forward, the reverse flow animates backward.
  // Two lanes per edge, one per direction: each pulses toward its
  // destination with its own rate label. The base edge stays as the anchor.
  // laneReg maps a base path to its lane/label elements so a dragged node's
  // lanes follow the edge live (syncLanes) instead of being left behind.
  // Declared at the top of the IIFE: syncLanes runs during init (redrawEdges),
  // which is before this point in source order.
  function syncLanes(path) {
    const lanes = laneReg.get(path);
    if (!lanes) return;
    let p0, p1, mid, len;
    try {
      len = path.getTotalLength();
      p0 = path.getPointAtLength(0);
      p1 = path.getPointAtLength(len);
      mid = path.getPointAtLength(len / 2);
    } catch (_) { return; }
    const dx = p1.x - p0.x, dy = p1.y - p0.y;
    const d = Math.hypot(dx, dy) || 1;
    const nx = -dy / d, ny = dx / d;
    for (const l of lanes) {
      l.el.setAttribute("d", path.getAttribute("d"));
      l.el.setAttribute("transform", `translate(${nx * 4 * l.side}, ${ny * 4 * l.side})`);
      l.label.setAttribute("x", mid.x + nx * 14 * l.side);
      l.label.setAttribute("y", mid.y + ny * 14 * l.side + 3);
    }
  }
  function applyTraffic(traffic) {
    world.querySelectorAll(".edge-lane, .edge-net-label").forEach((l) => l.remove());
    laneReg.clear();
    if (!settings.net) return;
    // group directional pairs onto their rendered edge
    const edges = new Map(); // path -> {fwd, rev}
    for (const t of traffic || []) {
      if (!t.bps) continue;
      let path = world.querySelector(`path[data-edge-from="${cssEsc(t.from)}"][data-edge-to="${cssEsc(t.to)}"]`);
      let forward = true;
      if (!path) {
        path = world.querySelector(`path[data-edge-from="${cssEsc(t.to)}"][data-edge-to="${cssEsc(t.from)}"]`);
        forward = false;
      }
      // a hidden edge (system nodes or reference edges toggled off) gets no lane
      if (!path || path.classList.contains("edge-lane") || path.style.display === "none") continue;
      const e = edges.get(path) || { fwd: 0, rev: 0 };
      if (forward) e.fwd += t.bps; else e.rev += t.bps;
      edges.set(path, e);
    }
    for (const [path, e] of edges) {
      let p0, p1, mid, len;
      try {
        len = path.getTotalLength();
        p0 = path.getPointAtLength(0);
        p1 = path.getPointAtLength(len);
        mid = path.getPointAtLength(len / 2);
      } catch (_) { continue; }
      const dx = p1.x - p0.x, dy = p1.y - p0.y;
      const d = Math.hypot(dx, dy) || 1;
      const nx = -dy / d, ny = dx / d; // unit normal
      // A ws tick rebuilds lanes and labels from scratch mid-hover, so the
      // focus class must be carried over by hand: the lane clone inherits
      // is-lit from its base path, the label (a fresh element) does not, and
      // without this it sits dimmed on a lit edge until the pointer moves.
      const lit = path.classList.contains("is-lit");
      const lane = (bps, dir, side) => {
        if (!bps) return;
        const c = path.cloneNode();
        c.classList.add("edge-lane", dir === 1 ? "edge-pulse-fwd" : "edge-pulse-rev");
        c.style.strokeWidth = Math.min(4, 1.5 + Math.log10(1 + bps / 1024)) + "px";
        c.setAttribute("transform", `translate(${nx * 4 * side}, ${ny * 4 * side})`);
        c.removeAttribute("data-edge-from");
        c.removeAttribute("data-edge-to");
        path.parentNode.appendChild(c);
        const label = document.createElementNS("http://www.w3.org/2000/svg", "text");
        label.setAttribute("class", lit ? "edge-net-label is-lit" : "edge-net-label");
        label.setAttribute("x", mid.x + nx * 14 * side);
        label.setAttribute("y", mid.y + ny * 14 * side + 3);
        label.textContent = (dir === 1 ? "\u2192 " : "\u2190 ") + fmtRate(bps);
        path.parentNode.appendChild(label);
        const reg = laneReg.get(path) || [];
        reg.push({ el: c, label, side });
        laneReg.set(path, reg);
      };
      lane(e.fwd, 1, 1);
      lane(e.rev, -1, -1);
    }
    buildLegend(); // the "live traffic" row exists only while lanes do
  }
  // --- ephemeral forward cards: one per live proxyrelay container ---
  // A port-forward card exists only while its relay container does, so it is
  // patched in here between page loads rather than by reload. Its markup,
  // wrapper class and height all come from the server (canvas.StatusNodes):
  // this used to be a second, hand-written copy of the templ card, and since
  // it only ran with a live tunnel open, any drift was invisible until a user
  // hit it.

  // The forward node id is "forward:<tile8>:<port>"; its target is the tile
  // card whose uuid starts with tile8. Stroke styling lives in .edge-forward
  // (css/input.css), shared with the server-rendered path.
  function addForwardEdge(id) {
    const tile8 = id.split(":")[1] || "";
    const target = [...world.querySelectorAll(".graph-node")]
      .map((e) => e.dataset.nodeId)
      .find((x) => x && /^(app|db):/.test(x) && x.split(":")[1].startsWith(tile8));
    if (!target) return;
    const svg = world.querySelector("svg");
    if (!svg) return;
    const p = document.createElementNS("http://www.w3.org/2000/svg", "path");
    p.setAttribute("data-edge-from", id);
    p.setAttribute("data-edge-to", target);
    p.setAttribute("data-edge-kind", "forward");
    p.setAttribute("fill", "none");
    p.classList.add("edge-busy", "edge-forward");
    svg.appendChild(p);
  }
  // Diff the DOM's ephemeral cards against the payload's. Returns whether
  // anything appeared, moved or vanished, so the caller redraws edges once.
  function syncEphemerals(list) {
    let changed = false;
    const want = new Set(list.map((n) => n.id));
    world.querySelectorAll(".graph-node[data-ephemeral]").forEach((el) => {
      if (want.has(el.dataset.nodeId)) return;
      edgesOf(el.dataset.nodeId).forEach((p) => p.remove());
      // If the pointer was resting on this card, removing it fires no pointer
      // event, without this the canvas stays dimmed around a card that no
      // longer exists until the mouse happens to move.
      if (el === focusNode) setFocus(null);
      el.remove();
      changed = true;
    });
    for (const n of list) {
      let el = nodeByID(n.id);
      if (!el) {
        el = document.createElement("div");
        el.className = n.class || "";
        el.dataset.nodeId = n.id;
        el.dataset.ephemeral = "1";
        el.style.width = NODE_W + "px";
        el.style.height = (n.height || 0) + "px";
        world.appendChild(el);
        addForwardEdge(n.id);
        changed = true;
      }
      const cx = parseFloat(el.style.left), cy = parseFloat(el.style.top);
      // inverted so a fresh card (NaN position) always falls through to move
      if (!(Math.abs(cx - n.x) <= 0.5 && Math.abs(cy - n.y) <= 0.5)) {
        el.style.left = n.x + "px";
        el.style.top = n.y + "px";
        changed = true;
      }
      // Only rewrite on change: innerHTML recreates the avatar <img>s, and an
      // unconditional rewrite refetched them on every poll tick.
      const face = n.html || "";
      if (el.dataset.face !== face) {
        el.dataset.face = face;
        el.innerHTML = face;
      }
    }
    return changed;
  }

  // One status fetch at a time: ws events can arrive faster than a rebuild
  // returns, and overlapping responses fight over card positions.
  let polling = false;
  async function pollStatus() {
    if (document.hidden || polling) return;
    polling = true;
    try {
      const res = await fetch(statusURL, { headers: { Accept: "application/json" } });
      if (!res.ok) return;
      const body = await res.json();
      const all = (Array.isArray(body) ? body : body.nodes) || [];
      // Ephemeral cards (live port-forwards) come and go without a page
      // change: they are diffed here instead of tripping the count check.
      const nodes = all.filter((n) => !n.ephemeral);
      const cards = world.querySelectorAll(".graph-node:not([data-ephemeral])");
      if (nodes.length !== cards.length) { location.reload(); return; }
      let movedAny = syncEphemerals(all.filter((n) => n.ephemeral));
      for (const n of nodes) {
        const el = world.querySelector(`[data-node-id="${cssEsc(n.id)}"]`);
        if (!el) { location.reload(); return; }
        // server truth about which cards have a saved position, a reset
        // elsewhere makes the canvas auto-laid-out again, and the next drag
        // has to snapshot everything afresh (see persistAll)
        if (n.saved) el.dataset.saved = "1";
        else delete el.dataset.saved;
        // remote layout changes: move the card unless we're dragging it, the
        // whole selection during a group drag, not just the grabbed card, or
        // a poll mid-drag snaps the rest of the group back
        if (!(drag && drag.group.includes(el))) {
          const cx = parseFloat(el.style.left), cy = parseFloat(el.style.top);
          if (Math.abs(cx - n.x) > 0.5 || Math.abs(cy - n.y) > 0.5) {
            el.style.left = n.x + "px";
            el.style.top = n.y + "px";
            movedAny = true; // edges are redrawn once, after the whole batch
          }
        }
        // The card's bottom strip, status light, cron result, live forward
        // count, comes rendered from the server (canvas.NodeFooter). Swapped
        // only when it actually changed, so a poll that says nothing new
        // touches no DOM.
        const footer = el.querySelector(".node-footer");
        if (!footer || !n.footer || footer.dataset.rendered === n.footer) continue;
        footer.dataset.rendered = n.footer;
        footer.innerHTML = n.footer;
      }
      if (movedAny) { redrawEdges(); updateDivider(); }
      if (!Array.isArray(body)) applyTraffic(body.traffic);
    } catch (_) { /* transient network errors: try again next tick */ } finally {
      polling = false;
    }
  }
  // Refresh is websocket-driven (live.js dispatches stackr:ws): the stack room
  // covers layout/status changes, the flow-sampler room covers traffic lanes.
  // No per-client timer, but not free either, a canvas in the flow room
  // refetches on every sampler tick, so an open canvas still costs a request
  // every few seconds.
  window.addEventListener("stackr:ws", pollStatus);
  // pollStatus no-ops while hidden, so a backgrounded tab misses every event:
  // catch up once on return.
  document.addEventListener("visibilitychange", () => {
    if (!document.hidden) pollStatus();
  });

  // --- annotations: shared per-canvas notes (text labels + boxes) ---
  // Same persistence idea as positions: the server stores them per canvas
  // owner, everyone with the canvas open sees them on next load.
  // no live sync mid-edit, the ws room already re-fetches statuses,
  // wiring annotations into that poll can come when someone asks for it.
  (function annotations() {
    const layer = document.getElementById("graph-annotations");
    const dataEl = document.getElementById("graph-annotations-data");
    if (!layer || !saveURL) return;
    const annoURL = saveURL.replace(/\/positions$/, "/annotations");
    let mode = null; // null | "text" | "box"

    // All annotation requests ride one chain: the server mints the id on
    // create, so a note's first save must come back (and stamp the id on the
    // element) before any later save or delete for it goes out.
    let annoChain = Promise.resolve();
    function save(el) {
      annoChain = annoChain
        .then(() => postJSON(annoURL, model(el)))
        .then((res) => (res.ok ? res.json() : null))
        .then((d) => {
          if (d && d.id) el.dataset.annoId = d.id;
        })
        .catch(() => {});
    }
    function destroy(el) {
      if (el.dataset.annoId) removeMember("anno:" + el.dataset.annoId); // don't leave a dangling group key
      el.remove();
      annoChain = annoChain
        .then(() => {
          if (!el.dataset.annoId) return; // never saved, nothing server-side
          return postJSON(annoURL + "/delete", { id: el.dataset.annoId });
        })
        .catch(() => {});
    }
    function model(el) {
      return {
        id: el.dataset.annoId,
        kind: el.dataset.annoKind,
        body: el.querySelector(".graph-anno-body").innerText.trim(),
        x: leftOf(el),
        y: topOf(el),
        w: parseFloat(el.style.width) || 0,
        h: parseFloat(el.style.height) || 0,
      };
    }

    function edit(el) {
      const body = el.querySelector(".graph-anno-body");
      body.contentEditable = "true";
      body.focus();
      // caret at the end, not a full select: less destructive on a mis-click
      const r = document.createRange();
      r.selectNodeContents(body);
      r.collapse(false);
      const sel = window.getSelection();
      sel.removeAllRanges();
      sel.addRange(r);
      const done = () => {
        body.contentEditable = "false";
        body.removeEventListener("blur", done);
        // an emptied text note is a deleted one; boxes may be label-less
        if (el.dataset.annoKind === "text" && !body.innerText.trim()) destroy(el);
        else save(el);
      };
      body.addEventListener("blur", done);
      body.addEventListener("keydown", (e) => {
        e.stopPropagation(); // typing must not trigger canvas shortcuts
        if (e.key === "Escape" || (e.key === "Enter" && !e.shiftKey)) {
          e.preventDefault();
          body.blur();
        }
      });
    }

    function render(a) {
      const el = document.createElement("div");
      // literal class names, Tailwind only emits component classes it can see
      el.className = a.kind === "box" ? "graph-anno graph-anno-box" : "graph-anno graph-anno-text";
      el.dataset.annoId = a.id;
      el.dataset.annoKind = a.kind;
      el.style.left = a.x + "px";
      el.style.top = a.y + "px";
      if (a.kind === "box") {
        el.style.width = (a.w || 200) + "px";
        el.style.height = (a.h || 140) + "px";
      }
      const body = document.createElement("div");
      body.className = "graph-anno-body";
      body.innerText = a.body || "";
      el.appendChild(body);
      const del = document.createElement("button");
      del.type = "button";
      del.className = "graph-anno-del";
      del.setAttribute("aria-label", "Delete annotation");
      del.textContent = "×";
      // preventDefault keeps a mid-edit body focused: no blur -> no save racing
      // the delete below, which could resurrect the note server-side.
      del.addEventListener("pointerdown", (e) => {
        e.stopPropagation();
        e.preventDefault();
      });
      del.addEventListener("click", (e) => {
        e.stopPropagation();
        destroy(el);
      });
      el.appendChild(del);

      // drag to move, the node pattern: stopPropagation keeps the canvas
      // from panning underneath. Selected or grouped neighbours ride along,
      // same as a card drag.
      el.addEventListener("pointerdown", (e) => {
        if (body.isContentEditable) return;
        if (e.button !== 0) return; // right-click is the context menu's
        if (e.shiftKey && e.pointerType !== "touch") return; // shift+drag marquees, even started on a box
        e.stopPropagation();
        e.preventDefault();
        const [wx, wy] = toWorld(e.clientX, e.clientY);
        const set = expandDragSet(selected.has(el) ? [...selected] : [el]);
        const origins = set.map((n) => [n, leftOf(n), topOf(n)]);
        const ox0 = leftOf(el), oy0 = topOf(el);
        const move = { dx: wx - ox0, dy: wy - oy0, moved: false };
        track(el, e, (ev) => {
          const [mx, my] = toWorld(ev.clientX, ev.clientY);
          let x = mx - move.dx, y = my - move.dy;
          if (settings.snap) {
            x = Math.round(x / GRID_PX) * GRID_PX;
            y = Math.round(y / GRID_PX) * GRID_PX;
          }
          if (!move.moved && (Math.abs(x - ox0) > 2 || Math.abs(y - oy0) > 2)) {
            move.moved = true;
            pushHistory(snapshot(set)); // undo restores where the drag began
          }
          if (!move.moved) return;
          const gdx = x - ox0, gdy = y - oy0;
          origins.forEach(([n, nx, ny]) => {
            placeCard(n, n === el ? x : clampX(n, nx + gdx), n === el ? y : ny + gdy);
          });
        }, () => { if (move.moved) saveMoved(set); });
      });
      el.addEventListener("dblclick", (e) => {
        e.stopPropagation();
        edit(el);
      });

      if (a.kind === "box") {
        const grip = document.createElement("div");
        grip.className = "graph-anno-grip";
        grip.addEventListener("pointerdown", (e) => {
          e.stopPropagation();
          e.preventDefault();
          track(grip, e, (ev) => {
            const [mx, my] = toWorld(ev.clientX, ev.clientY);
            el.style.width = Math.max(80, mx - leftOf(el)) + "px";
            el.style.height = Math.max(50, my - topOf(el)) + "px";
          }, () => save(el));
        });
        el.appendChild(grip);
      }
      layer.appendChild(el);
      return el;
    }

    let existing = [];
    try { existing = JSON.parse(dataEl?.textContent || "[]") || []; } catch (_) {}
    existing.forEach(render);

    // --- placement modes ---
    const buttons = document.querySelectorAll("[data-anno-add]");
    function setMode(m) {
      mode = m;
      viewport.classList.toggle("graph-annotating", !!mode);
      buttons.forEach((b) => b.classList.toggle("graph-ctrl-active", b.dataset.annoAdd === mode));
    }
    buttons.forEach((b) => {
      b.addEventListener("click", () => setMode(mode === b.dataset.annoAdd ? null : b.dataset.annoAdd));
    });
    escapeFns.push(() => { if (mode) setMode(null); });

    // Capture phase so an active placement mode wins over pan/marquee.
    viewport.addEventListener("pointerdown", (e) => {
      if (!mode || e.target.closest(".graph-ctrl, .graph-node, .graph-anno, a")) return;
      e.stopPropagation();
      e.preventDefault();
      const [x, y] = toWorld(e.clientX, e.clientY);
      // id: "", the server mints it on first save and it comes back stamped
      if (mode === "text") {
        const el = render({ id: "", kind: "text", body: "", x, y, w: 0, h: 0 });
        setMode(null);
        edit(el); // saved on blur; emptied = deleted
        return;
      }
      // box: drag out the rectangle
      const el = render({ id: "", kind: "box", body: "", x, y, w: 0, h: 0 });
      el.style.width = "0px";
      el.style.height = "0px";
      track(viewport, e, (ev) => {
        const [mx, my] = toWorld(ev.clientX, ev.clientY);
        el.style.width = Math.max(0, mx - x) + "px";
        el.style.height = Math.max(0, my - y) + "px";
      }, () => {
        setMode(null);
        // a stray click without a real drag leaves no box behind
        if ((parseFloat(el.style.width) || 0) < 40 || (parseFloat(el.style.height) || 0) < 30) {
          el.remove();
          return;
        }
        save(el);
      });
    }, true);

    // hooks for the groups/context-menu code above: same save chain (so a
    // group save never overtakes the id-minting save of a member note), same
    // renderers for the menu's "add here" actions.
    annoPersist = save;
    annoQueue = (fn) => { annoChain = annoChain.then(fn).catch(() => {}); };
    annoCreate = (kind, x, y) => {
      if (kind === "text") {
        edit(render({ id: "", kind: "text", body: "", x, y, w: 0, h: 0 })); // saved on blur; emptied = deleted
      } else {
        save(render({ id: "", kind: "box", body: "", x, y, w: 280, h: 180 }));
      }
    };
  })();

  // --- the environments pill (stack canvas): remember open/closed, and ring
  // the env card whose column header is under the pointer ---
  const COMPARE_KEY = "stackr.envcompare.open";
  const TAB_KEY = "stackr.envcompare.tab";
  function initCompare() {
    const d = document.querySelector("[data-env-compare]");
    if (!d) return;
    try {
      d.open = localStorage.getItem(COMPARE_KEY) === "1";
      const tab = d.querySelector(`input[name="env-tab"][value="${localStorage.getItem(TAB_KEY)}"]`);
      if (tab && !tab.disabled) tab.checked = true;
    } catch (_) {}
  }
  initCompare();
  document.addEventListener("toggle", (e) => {
    if (!e.target.matches || !e.target.matches("[data-env-compare]")) return;
    try { localStorage.setItem(COMPARE_KEY, e.target.open ? "1" : "0"); } catch (_) {}
  }, true);
  // Grow and shrink the card: a <details> snaps open, so the body's height is
  // animated by hand. Closing runs the animation first, then closes.
  const REDUCED = matchMedia("(prefers-reduced-motion: reduce)");
  const GROW = { duration: 220, easing: "cubic-bezier(.2,.7,.2,1)" };
  document.addEventListener("click", (e) => {
    const sum = e.target.closest && e.target.closest("[data-env-compare] > summary");
    if (!sum || REDUCED.matches) return;
    const d = sum.parentElement;
    const body = d.querySelector(".env-tabs");
    if (!body || body.dataset.animating) return;
    e.preventDefault();
    body.dataset.animating = "1";
    body.style.overflow = "hidden";
    const done = () => { body.style.height = ""; body.style.overflow = ""; delete body.dataset.animating; };
    if (d.open) {
      const h = body.getBoundingClientRect().height;
      body.animate([{ height: h + "px", opacity: 1 }, { height: "0px", opacity: 0 }], GROW)
        .onfinish = () => { d.open = false; done(); };
    } else {
      d.open = true;
      const h = body.getBoundingClientRect().height;
      body.animate([{ height: "0px", opacity: 0 }, { height: h + "px", opacity: 1 }], GROW).onfinish = done;
    }
  });
  document.addEventListener("change", (e) => {
    if (!e.target.matches || !e.target.matches('input[name="env-tab"]')) return;
    try { localStorage.setItem(TAB_KEY, e.target.value); } catch (_) {}
  });
  document.body.addEventListener("htmx:afterSwap", (e) => {
    if (e.detail && e.detail.target && e.detail.target.id === "env-compare") initCompare();
  });
  function ringEnv(e, on) {
    const h = e.target.closest && e.target.closest("[data-env-node]");
    if (!h) return;
    const card = world.querySelector(`[data-node-id="${cssEsc(h.dataset.envNode)}"]`);
    if (card) card.classList.toggle("env-ring", on);
  }
  document.addEventListener("mouseover", (e) => ringEnv(e, true));
  document.addEventListener("mouseout", (e) => ringEnv(e, false));
})();
