package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/agent"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/nodes"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/placement"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/registry"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"

	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/components"
)

// The swarm half of the servers screen (docs/plans/32-multi-node-ui.md). The
// single-server page it grew out of is in handler.go and is unchanged apart
// from its header.

// List renders the servers list: one row per swarm node.
func (h *handler) List(c echo.Context) error {
	ctx := c.Request().Context()
	rows, err := h.nodes.Sync(ctx)
	if err != nil {
		return err
	}
	return respond.HTML(c, http.StatusOK, nodesPage(c, rows,
		h.rt.IsManagerNode(ctx), agent.Running(ctx, h.rt), h.panelHost(c)))
}

// panelHost is the base URL a node curls the join script from. The configured
// BASE_URL when there is one, otherwise the host this request arrived on,
// which is the only thing a LAN install has.
func (h *handler) panelHost(c echo.Context) string {
	if h.baseURL != "" {
		return strings.TrimSuffix(h.baseURL, "/")
	}
	scheme := "http"
	if c.Request().TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + c.Request().Host
}

// AddNodeForm opens the Add node modal.
func (h *handler) AddNodeForm(c echo.Context) error {
	return respond.HTML(c, http.StatusOK, addNodeModal(c))
}

// AddNode creates the pending row and its one-time key, then shows the script.
//
// The agent's global service is created here, before the node joins: swarm
// then places a task on it the moment it arrives, rather than after somebody
// notices there is no agent (docs/plans/32-multi-node-ui.md, Add node).
func (h *handler) AddNode(c echo.Context) error {
	ctx := c.Request().Context()
	sv, key, err := h.nodes.AddNode(ctx, strings.TrimSpace(c.FormValue("name")), strings.TrimSpace(c.FormValue("address")))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	agentErr := ""
	if err := h.nodeSvc.EnsureAgent(ctx); err != nil {
		// The row and the key are good; the node can still join. Say so
		// rather than rolling back a key the operator may already be pasting.
		// Logged as well as flashed: a flash is gone on the next page, and
		// this is the failure that leaves every worker without exec, stats,
		// volume browse or move.
		slog.Error("node agent service", "error", err)
		// Carried into the modal, not flashed. middleware.SetFlash writes a
		// cookie the *next* request reads, and this response is the modal
		// itself, so a flash set here was never seen at all.
		//
		// Inline rather than a toast: the toast fades after four seconds and
		// the operator is about to walk to another machine and paste this
		// script. The warning belongs beside the script it qualifies.
		agentErr = "The node agent could not be started, so exec, stats, volume browse and " +
			"volume moves will not work on this node until it is. The join below still works. " +
			err.Error()
	}
	return respond.HTML(c, http.StatusOK, joinModal(c, sv, key, h.panelHost(c), agentErr))
}

// NewJoinKey mints a fresh key for a pending row whose first one expired.
func (h *handler) NewJoinKey(c echo.Context) error {
	ctx := c.Request().Context()
	sv, err := h.store.GetServer(ctx, c.Param("id"))
	if err != nil || sv == nil {
		return echo.NewHTTPError(http.StatusNotFound, "server not found")
	}
	if _, err := h.nodes.IssueKey(ctx, sv); err != nil {
		return err
	}
	return h.List(c)
}

// JoinScript is what a joining node curls. It is the one route on the servers
// screens with no session behind it: the machine running it has no login, and
// the one-time key bound to its address is the authentication.
func (h *handler) JoinScript(c echo.Context) error {
	ctx := c.Request().Context()
	sv, err := h.nodes.Redeem(ctx, c.Param("key"), joinClientIP(c))
	if err != nil {
		// A script that prints, not a comment. This body is piped straight
		// into sh, and `# reason` is a shell comment: sh reads it, says
		// nothing, and the operator sees an empty terminal or curl's own
		// `curl: (22)`. echo to stderr survives
		// the pipe and reads the same when curl is run on its own.
		return c.String(http.StatusForbidden,
			"#!/bin/sh\necho 'stackr: "+shellQuote(err.Error())+"' >&2\nexit 1\n")
	}
	token, managerAddr, err := h.rt.JoinToken(ctx)
	if err != nil {
		return c.String(http.StatusInternalServerError, "# this server is not a swarm manager\nexit 1\n")
	}
	reg, regTLS := "", false
	if r, err := h.store.GetManagedRegistry(ctx); err == nil && r != nil {
		reg, _ = registry.PullAddr(ctx, h.rt, r)
		regTLS = r.Domain != ""
	}
	return c.String(http.StatusOK, joinScript(joinScriptData{
		ServerName:   sv.Name,
		ManagerAddr:  managerAddr,
		Token:        token,
		RegistryAddr: reg,
		RegistryTLS:  regTLS,
		PanelURL:     h.panelHost(c),
		Key:          c.Param("key"),
	}))
}

// ClaimNode is POST /join/:key/node, called by the join script the moment the
// swarm join succeeds. It is what keys the row on the node id instead of on an
// address the daemon may never advertise.
//
// Plain text, not HTML: the caller is curl inside a shell script, and the
// script treats a failure as non-fatal, so the address match stays the
// fallback.
//
// PanelURL cannot be runtime.PanelAlias: that name only resolves on the stkr
// overlay, and the node is not on it yet when this fires.
func (h *handler) ClaimNode(c echo.Context) error {
	err := h.nodes.Claim(c.Request().Context(), c.Param("key"),
		strings.TrimSpace(c.FormValue("node_id")), joinClientIP(c))
	if err != nil {
		return c.String(http.StatusForbidden, "stackr: "+err.Error()+"\n")
	}
	return c.String(http.StatusOK, "ok\n")
}

// joinClientIP is the address the joining machine actually came from.
//
// The panel publishes no port, so every request reaches it through traefik on
// the overlay and the socket's peer address is traefik's, not the node's.
// Echo's RealIP only reads X-Forwarded-For when TRUSTED_PROXIES names the
// proxy, which is an install-time env var nobody sets for their own bundled
// traefik, so without this the address a key is bound to could never match
// and no node could ever join.
//
// The leftmost entry is the original client. Traefik replaces any inbound
// X-Forwarded-For when it is the edge, which it is here: nothing else is
// listening. This is a safety rail on top of the key, not the authentication,
// the one-time key is that, so reading a header traefik controls is the
// right trade rather than refusing every join.
// pingText rounds a connect time for the node table. Two nodes on the same
// host answer in well under a millisecond, and a bare "0 ms" reads as "never
// measured" rather than "fast".
func pingText(ms float64) string {
	if ms < 1 {
		return "<1 ms"
	}
	return fmt.Sprintf("%.0f ms", ms)
}

func joinClientIP(c echo.Context) string {
	if xff := c.Request().Header.Get("X-Forwarded-For"); xff != "" {
		if first, _, found := strings.Cut(xff, ","); found {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(xff)
	}
	return c.RealIP()
}

// --- node actions ---------------------------------------------------------

// DrainForm opens the Drain modal, which splits what swarm moves on its own
// from what it never will.
func (h *handler) DrainForm(c echo.Context) error {
	ctx := c.Request().Context()
	sv, err := h.store.GetServer(ctx, c.Param("id"))
	if err != nil || sv == nil {
		return echo.NewHTTPError(http.StatusNotFound, "server not found")
	}
	stateless, pinned, err := h.tasksOn(ctx, sv.NodeID)
	if err != nil {
		return err
	}
	return respond.HTML(c, http.StatusOK, drainModal(c, sv, stateless, pinned))
}

// RemoveForm opens the Remove modal, which also names the volumes that stay
// on the machine and become unreachable.
func (h *handler) RemoveForm(c echo.Context) error {
	ctx := c.Request().Context()
	sv, err := h.store.GetServer(ctx, c.Param("id"))
	if err != nil || sv == nil {
		return echo.NewHTTPError(http.StatusNotFound, "server not found")
	}
	stateless, pinned, err := h.tasksOn(ctx, sv.NodeID)
	if err != nil {
		return err
	}
	// Display only, and the node being removed is often the one already
	// gone: bounded, so the dialogue opens and the remove still runs.
	vctx, cancel := components.PageCtx(ctx)
	defer cancel()
	vols, volErr := h.clus.ListVolumes(vctx, sv.NodeID)
	msg := ""
	if volErr != nil {
		msg = volErr.Error()
	}
	return respond.HTML(c, http.StatusOK, removeModal(c, sv, stateless, pinned, vols, msg))
}

// tasksOn asks the node service for the split and wraps the pinned tiles in
// the row type the two modals render.
func (h *handler) tasksOn(ctx context.Context, nodeID string) (stateless []runtime.NodeTaskInfo, pinned []pinnedTile, err error) {
	stateless, tiles, err := h.nodeSvc.Tasks(ctx, nodeID)
	if err != nil {
		return nil, nil, err
	}
	for _, t := range tiles {
		pinned = append(pinned, pinnedTile{Tile: t})
	}
	return stateless, pinned, nil
}

// Drain moves stateless tasks off and leaves the node in the swarm.
func (h *handler) Drain(c echo.Context) error {
	return h.nodeAction(c, "Draining. Stateless tasks are moving off; the node stays in the swarm.",
		func(ctx context.Context, sv *repo.Server) error { return h.nodeSvc.Drain(ctx, sv) })
}

// confirmed reports whether the operator typed the word the modal asked for.
// The box is client-side validated too, which is a convenience; this is the
// check.
// confirmed is components.Confirmed, kept as a local name so the call sites
// below read the same as they always did.
func confirmed(c echo.Context, want string) bool { return components.Confirmed(c, want) }

// Activate puts a drained node back into service.
func (h *handler) Activate(c echo.Context) error {
	return h.nodeAction(c, "Node is active again.",
		func(ctx context.Context, sv *repo.Server) error { return h.nodeSvc.Activate(ctx, sv) })
}

// nodeAction is the shape every node button shares: load the row, call the
// service, flash and come back to the node.
func (h *handler) nodeAction(c echo.Context, msg string, do func(context.Context, *repo.Server) error) error {
	ctx := c.Request().Context()
	sv, err := h.store.GetServer(ctx, c.Param("id"))
	if err != nil || sv == nil {
		return echo.NewHTTPError(http.StatusNotFound, "server not found")
	}
	if err := do(ctx, sv); err != nil {
		return stackrmw.HTTP(err)
	}
	middleware.SetFlash(c, msg, middleware.FlashSuccess)
	return respond.Redirect(c, "/servers/"+sv.ID)
}

// Remove drains the node and takes it out of the swarm. The volumes on its
// disk go with it, which is what the modal made the operator type a name to
// acknowledge.
func (h *handler) Remove(c echo.Context) error {
	ctx := c.Request().Context()
	sv, err := h.store.GetServer(ctx, c.Param("id"))
	if err != nil || sv == nil {
		return echo.NewHTTPError(http.StatusNotFound, "server not found")
	}
	if err := h.nodeSvc.Remove(ctx, sv, confirmed(c, sv.Name)); err != nil {
		return stackrmw.HTTP(err)
	}
	middleware.SetFlash(c, sv.Name+" removed from the swarm.", middleware.FlashSuccess)
	// The pending row's Remove is an htmx swap of the table, not a navigation:
	// a redirect there would replace the table with a whole page.
	if c.Request().Header.Get("HX-Request") == "true" && c.Request().Header.Get("HX-Target") == "nodes-live" {
		return h.List(c)
	}
	return respond.Redirect(c, "/servers")
}

// SaveGroup writes the node's group label, which is what a tile's node_group
// is matched against.
func (h *handler) SaveGroup(c echo.Context) error {
	ctx := c.Request().Context()
	sv, err := h.store.GetServer(ctx, c.Param("id"))
	if err != nil || sv == nil {
		return echo.NewHTTPError(http.StatusNotFound, "server not found")
	}
	group := strings.TrimSpace(c.FormValue("group"))
	if err := h.nodeSvc.SetGroup(ctx, sv, group); err != nil {
		return stackrmw.HTTP(err)
	}
	if group == "" {
		middleware.SetFlash(c, "Group cleared. Tiles with no group requirement can run here.", middleware.FlashSuccess)
	} else {
		middleware.SetFlash(c, "Group set to "+group+". It takes effect on each tile's next deploy.", middleware.FlashSuccess)
	}
	return respond.Redirect(c, "/servers/"+sv.ID)
}

// --- move -----------------------------------------------------------------

// MoveForm opens the Move modal, or reopens it on a move already running.
func (h *handler) MoveForm(c echo.Context) error {
	ctx := c.Request().Context()
	t, err := h.store.GetTile(ctx, c.Param("id"))
	if err != nil || t == nil {
		return echo.NewHTTPError(http.StatusNotFound, "tile not found")
	}
	if mv := h.mover.ForTile(ctx, t.ID); mv != nil {
		return respond.HTML(c, http.StatusOK, moveModal(c, t, nil, nil, 0, "", mv))
	}
	targets, err := h.moveTargets(ctx, t)
	if err != nil {
		return err
	}
	vols, total, sizeErr := h.mover.Estimate(ctx, t)
	msg := ""
	if sizeErr != nil {
		msg = sizeErr.Error()
	}
	return respond.HTML(c, http.StatusOK, moveModal(c, t, targets, vols, total, msg, nil))
}

// moveTargets is the ready nodes a tile may move to: inside its node group
// when it has one, and never the node it is already on.
func (h *handler) moveTargets(ctx context.Context, t *repo.Tile) ([]runtime.Node, error) {
	all, err := h.rt.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	plan, err := placement.For(ctx, h.store, h.rt, t)
	if err != nil {
		// A tile whose home node has left the swarm still has to be movable
		// somewhere; the group is all that matters here.
		plan.Group = t.NodeGroup
	}
	var out []runtime.Node
	for _, n := range all {
		if n.ID == t.HomeNode || n.Status() != "ready" {
			continue
		}
		if plan.Group != "" && n.Group != plan.Group {
			continue
		}
		out = append(out, n)
	}
	return out, nil
}

// Move starts the copy. The modal turns into a progress bar and polls.
func (h *handler) Move(c echo.Context) error {
	ctx := c.Request().Context()
	t, err := h.store.GetTile(ctx, c.Param("id"))
	if err != nil || t == nil {
		return echo.NewHTTPError(http.StatusNotFound, "tile not found")
	}
	target := c.FormValue("target")
	// Checked against the list that decided the dropdown, not just handed to
	// the mover. Without this the node group and the ready check live only in
	// the rendered <select>, so a stale or hand-made POST moves the volume to
	// a node the tile is not allowed on and placement then refuses to deploy
	// it at all.
	targets, err := h.moveTargets(ctx, t)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(targets, func(n runtime.Node) bool { return n.ID == target }) {
		return h.moveError(c, t, targets, target)
	}
	mv, err := h.mover.Start(ctx, t, target)
	if err != nil {
		return h.moveError(c, t, targets, err.Error())
	}
	return respond.HTML(c, http.StatusOK, moveModal(c, t, nil, nil, 0, "", mv))
}

// moveError re-renders the modal with the reason in it. A bare HTTPError here
// answers with a whole error page, and the form's hx-target swaps that into
// the modal host, a nested document where the dialog was.
func (h *handler) moveError(c echo.Context, t *repo.Tile, targets []runtime.Node, msg string) error {
	if msg == "" {
		msg = "pick a node to move " + t.Name + " to"
	} else if !strings.Contains(msg, " ") {
		msg = "that is not a node " + t.Name + " can move to. Pick one from the list"
	}
	vols, total, sizeErr := h.mover.Estimate(c.Request().Context(), t)
	if sizeErr != nil {
		vols, total = nil, 0
	}
	return respond.HTML(c, http.StatusOK, moveModal(c, t, targets, vols, total, msg, nil))
}

var _ = nodes.LocalID

// shellQuote makes a string safe inside the single quotes of the refusal
// script above. Only one character matters in single quotes, and the standard
// escape for it is to close the quote, emit a literal quote, and reopen.
//
// The strings involved are the panel's own error text today, but they carry
// the address a request came from, so they are not entirely ours.
func shellQuote(s string) string {
	return strings.ReplaceAll(s, "'", `'\''`)
}
