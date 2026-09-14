package org

import (
	"context"
	"net/http"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/config/varref"
	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/components/canvas"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/graph"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/handler/annotate"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Home is the root canvas: every organization the viewer belongs to, one card
// each. The top of the graph, so it gets "/" rather than a redirect into one
// org. A user in no organization sees the canvas's own empty state.
// GET /
func (h *handler) Home(c echo.Context) error {
	// Somebody whose only organization is the one they are halfway through
	// setting up is not here to look at a canvas of one tile: they are here to
	// carry on. Every other route in that org already lands them on the wizard,
	// so this is the one page that made them click through a picture of it.
	if u := stackrmw.CurrentUser(c); u != nil {
		if orgs, err := h.store.ListOrgsForUser(c.Request().Context(), u.ID); err == nil &&
			len(orgs) == 1 && orgs[0].SetupDoneAt == nil && h.ownerOf(c, orgs[0].ID) {
			return respond.Redirect(c, "/orgs/"+orgs[0].Slug+"/setup/done")
		}
	}
	g, err := h.buildOrgsGraph(c)
	if err != nil {
		return err
	}
	return respond.HTML(c, http.StatusOK, orgsGraphPage(c, g))
}

// HomeStatus is the live snapshot the root canvas re-fetches. No ws room pushes
// to it (orgs don't change under you), but the canvas polls on reconnect.
// GET /graph/status
func (h *handler) HomeStatus(c echo.Context) error {
	ctx := c.Request().Context()
	g, err := h.buildOrgsGraph(c)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{"nodes": canvas.StatusNodes(ctx, g.Nodes)})
}

// SaveHomeNodePosition persists dragged positions on the root canvas. Owned by
// the user, not an org, see repo.ScopeUser.
// POST /graph/positions
func (h *handler) SaveHomeNodePosition(c echo.Context) error {
	u := stackrmw.CurrentUser(c)
	if u == nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "not signed in")
	}
	ps, err := bindNodePositions(c)
	if err != nil {
		return err
	}
	owner := repo.GraphOwner(repo.ScopeUser, u.ID)
	if err := repo.ValidateNodePositions(owner, ps); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if err := h.store.SaveNodePositions(c.Request().Context(), owner, ps); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

// ResetHomeNodePositions drops the root canvas layout.
// POST /graph/positions/reset
func (h *handler) ResetHomeNodePositions(c echo.Context) error {
	u := stackrmw.CurrentUser(c)
	if u == nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "not signed in")
	}
	if err := h.store.DeleteNodePositions(c.Request().Context(), repo.GraphOwner(repo.ScopeUser, u.ID)); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

// SaveHomeAnnotation / DeleteHomeAnnotation edit the root canvas's notes,
// user-owned, like its layout.
// POST /graph/annotations, /graph/annotations/delete
func (h *handler) SaveHomeAnnotation(c echo.Context) error {
	u := stackrmw.CurrentUser(c)
	if u == nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "not signed in")
	}
	return annotate.Save(c, h.store, repo.GraphOwner(repo.ScopeUser, u.ID))
}

func (h *handler) DeleteHomeAnnotation(c echo.Context) error {
	u := stackrmw.CurrentUser(c)
	if u == nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "not signed in")
	}
	return annotate.Delete(c, h.store, repo.GraphOwner(repo.ScopeUser, u.ID))
}

// SaveAnnotation / DeleteAnnotation edit the org canvas's shared notes.
// POST /orgs/:id/graph/annotations, /orgs/:id/graph/annotations/delete
func (h *handler) SaveAnnotation(c echo.Context) error {
	o, err := h.loadOrg(c)
	if err != nil {
		return err
	}
	if err := annotate.Save(c, h.store, repo.GraphOwner(repo.ScopeOrg, o.ID)); err != nil {
		return err
	}
	h.notifier.Org(o.ID)
	return nil
}

func (h *handler) DeleteAnnotation(c echo.Context) error {
	o, err := h.loadOrg(c)
	if err != nil {
		return err
	}
	if err := annotate.Delete(c, h.store, repo.GraphOwner(repo.ScopeOrg, o.ID)); err != nil {
		return err
	}
	h.notifier.Org(o.ID)
	return nil
}

// SaveHomeGraphGroup / DeleteHomeGraphGroup edit the root canvas's groups,
// user-owned, like its layout.
// POST /graph/groups, /graph/groups/delete
func (h *handler) SaveHomeGraphGroup(c echo.Context) error {
	u := stackrmw.CurrentUser(c)
	if u == nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "not signed in")
	}
	return annotate.SaveGroup(c, h.store, repo.GraphOwner(repo.ScopeUser, u.ID))
}

func (h *handler) DeleteHomeGraphGroup(c echo.Context) error {
	u := stackrmw.CurrentUser(c)
	if u == nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "not signed in")
	}
	return annotate.DeleteGroup(c, h.store, repo.GraphOwner(repo.ScopeUser, u.ID))
}

// SaveGraphGroup / DeleteGraphGroup edit the org canvas's shared groups.
// POST /orgs/:id/graph/groups, /orgs/:id/graph/groups/delete
func (h *handler) SaveGraphGroup(c echo.Context) error {
	o, err := h.loadOrg(c)
	if err != nil {
		return err
	}
	if err := annotate.SaveGroup(c, h.store, repo.GraphOwner(repo.ScopeOrg, o.ID)); err != nil {
		return err
	}
	h.notifier.Org(o.ID)
	return nil
}

func (h *handler) DeleteGraphGroup(c echo.Context) error {
	o, err := h.loadOrg(c)
	if err != nil {
		return err
	}
	if err := annotate.DeleteGroup(c, h.store, repo.GraphOwner(repo.ScopeOrg, o.ID)); err != nil {
		return err
	}
	h.notifier.Org(o.ID)
	return nil
}

// buildOrgsGraph gathers the viewer's organizations and how many stacks each
// holds. No membership check needed: the query is already scoped to the user.
func (h *handler) buildOrgsGraph(c echo.Context) (graph.Graph, error) {
	ctx := c.Request().Context()
	u := stackrmw.CurrentUser(c)
	if u == nil {
		return graph.Graph{}, echo.NewHTTPError(http.StatusUnauthorized, "not signed in")
	}
	orgs, err := h.store.ListOrgsForUser(ctx, u.ID)
	if err != nil {
		return graph.Graph{}, err
	}
	rows, err := h.store.ListNodePositions(ctx, repo.GraphOwner(repo.ScopeUser, u.ID))
	if err != nil {
		return graph.Graph{}, err
	}
	positions := make(map[string][2]float64, len(rows))
	for _, r := range rows {
		positions[r.NodeID] = [2]float64{r.X, r.Y}
	}
	summaries := make([]graph.OrgSummary, 0, len(orgs))
	for i := range orgs {
		stacks, err := h.store.ListStacksByOrg(ctx, orgs[i].ID)
		if err != nil {
			return graph.Graph{}, err
		}
		// Membership is a count on the card, not a list: who they are lives in
		// the org's own settings. A read failure leaves it at 0, which the card
		// renders as "no member count" rather than "0 members".
		members, err := h.store.ListOrgMembers(ctx, orgs[i].ID)
		if err != nil {
			return graph.Graph{}, err
		}
		summaries = append(summaries, graph.OrgSummary{
			ID: orgs[i].ID, Name: orgs[i].Name, Slug: orgs[i].Slug,
			Href: "/orgs/" + orgs[i].Slug, Stacks: len(stacks), Members: len(members),
			SetupPending: orgs[i].SetupDoneAt == nil,
		})
	}
	g := graph.BuildOrgs(summaries, positions)
	g.Annotations, _ = h.store.ListAnnotations(ctx, repo.GraphOwner(repo.ScopeUser, u.ID))
	g.Groups, _ = h.store.ListGraphGroups(ctx, repo.GraphOwner(repo.ScopeUser, u.ID))
	return g, nil
}

// bindNodePositions reads the single-node or full-snapshot body the canvas
// posts. Shared by every level's position handler.
func bindNodePositions(c echo.Context) ([]repo.NodePosition, error) {
	type pos struct {
		NodeID string  `json:"node_id"`
		X      float64 `json:"x"`
		Y      float64 `json:"y"`
	}
	var in struct {
		pos
		Nodes []pos `json:"nodes"`
	}
	if err := c.Bind(&in); err != nil || (in.NodeID == "" && len(in.Nodes) == 0) {
		return nil, echo.NewHTTPError(http.StatusBadRequest, "node_id, x, y required")
	}
	if in.NodeID != "" {
		in.Nodes = append(in.Nodes, in.pos)
	}
	ps := make([]repo.NodePosition, 0, len(in.Nodes))
	for _, p := range in.Nodes {
		ps = append(ps, repo.NodePosition{NodeID: p.NodeID, X: p.X, Y: p.Y})
	}
	return ps, nil
}

// Graph renders the org canvas: stacks, the instances they share, connectors
// and a card for org-wide variables. Clicking a stack drops into its canvas.
// GET /orgs/:id
func (h *handler) Graph(c echo.Context) error {
	ctx := c.Request().Context()
	o, err := h.loadOrg(c)
	if err != nil {
		return err
	}
	g, _, err := h.buildOrgGraph(ctx, o, arrangeStyle(c))
	if err != nil {
		return err
	}
	// Only owners get the banner: the pages it leads to are owner-only, and a
	// strip that 404s is worse than no strip.
	var plan *repo.ConfigPlan
	var pending int
	if h.ownerOf(c, o.ID) {
		plan, pending = h.pendingOrgPlans(c, o)
	}
	return respond.HTML(c, http.StatusOK, orgGraphPage(c, o, g, plan, pending))
}

// GraphStatus is the live snapshot the canvas re-fetches on ws events.
// GET /orgs/:id/graph/status
func (h *handler) GraphStatus(c echo.Context) error {
	ctx := c.Request().Context()
	o, err := h.loadOrg(c)
	if err != nil {
		return err
	}
	g, nodeOf, err := h.buildOrgGraph(ctx, o, arrangeStyle(c))
	if err != nil {
		return err
	}
	body := map[string]any{"nodes": canvas.StatusNodes(ctx, g.Nodes)}
	if h.sampler != nil {
		body["traffic"] = graph.RollupTraffic(h.sampler.Snapshot().Pairs, nodeOf)
	}
	return c.JSON(http.StatusOK, body)
}

// SaveNodePosition persists dragged positions on the org canvas (single node
// mid-drag, full snapshot on drag end, see the env handler).
// POST /orgs/:id/graph/positions
func (h *handler) SaveNodePosition(c echo.Context) error {
	ps, err := bindNodePositions(c)
	if err != nil {
		return err
	}
	o, err := h.loadOrg(c)
	if err != nil {
		return err
	}
	owner := repo.GraphOwner(repo.ScopeOrg, o.ID)
	if err := repo.ValidateNodePositions(owner, ps); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if err := h.store.SaveNodePositions(c.Request().Context(), owner, ps); err != nil {
		return err
	}
	h.notifier.Org(o.ID) // other open org canvases re-fetch and move the card
	return c.NoContent(http.StatusNoContent)
}

// ResetNodePositions drops the org canvas layout.
// POST /orgs/:id/graph/positions/reset
func (h *handler) ResetNodePositions(c echo.Context) error {
	o, err := h.loadOrg(c)
	if err != nil {
		return err
	}
	if err := h.store.DeleteNodePositions(c.Request().Context(), repo.GraphOwner(repo.ScopeOrg, o.ID)); err != nil {
		return err
	}
	h.notifier.Org(o.ID)
	return c.NoContent(http.StatusNoContent)
}

// loadOrg resolves the org in the URL and authorizes against that org, not
// the one the cookie has selected: membership alone would let a viewer of
// this org rewrite its shared canvas layout as long as they hold write
// somewhere else (ReadOnlyGuard reads the active org's role). Reads are
// unaffected, RequireOrgWrite only refuses mutating methods, and it records
// write rights for the templates.
func (h *handler) loadOrg(c echo.Context) (*repo.Org, error) {
	// Slug first, id as fallback, same contract as settingsOrg, so the
	// canvas and settings pages agree on what /orgs/<x> means.
	ctx := c.Request().Context()
	key := orgKey(c)
	o, err := h.store.GetOrgBySlug(ctx, key)
	if err != nil {
		return nil, err
	}
	if o == nil {
		if o, err = h.store.GetOrg(ctx, key); err != nil {
			return nil, err
		}
	}
	if o == nil {
		return nil, echo.NewHTTPError(http.StatusNotFound, "org not found")
	}
	if err := stackrmw.RequireOrgWrite(c, h.store, o.ID); err != nil {
		return nil, err
	}
	return o, nil
}

// buildOrgGraph gathers the org's stacks and what they share. Org variables are
// counted from the variables table directly: the varref catalogue never
// suggests scope-shared entries (internal/varref/catalogue.go), so it would
// under-report here. The second return maps sampler endpoints ("app:<tile>",
// "db:<tile>", "proxy") to their card here, for graph.RollupTraffic.
// arrangeStyle reads the signed-in user's layout engine choice out of their
// graph prefs; anonymous or unset falls back to the default.
func arrangeStyle(c echo.Context) graph.ArrangeStyle {
	prefs := ""
	if u := stackrmw.CurrentUser(c); u != nil {
		prefs = u.GraphPrefs
	}
	return graph.StyleFromPrefs(prefs)
}

func (h *handler) buildOrgGraph(ctx context.Context, o *repo.Org, style graph.ArrangeStyle) (graph.Graph, map[string]string, error) {
	stacks, err := h.store.ListStacksByOrg(ctx, o.ID)
	if err != nil {
		return graph.Graph{}, nil, err
	}
	rows, err := h.store.ListNodePositions(ctx, repo.GraphOwner(repo.ScopeOrg, o.ID))
	if err != nil {
		return graph.Graph{}, nil, err
	}
	positions := make(map[string][2]float64, len(rows))
	for _, r := range rows {
		positions[r.NodeID] = [2]float64{r.X, r.Y}
	}

	// How much of each stack is reachable from outside, counted here, listed
	// one level down.
	allDomains, err := h.store.ListDomains(ctx)
	if err != nil {
		return graph.Graph{}, nil, err
	}
	domainsPerTile := make(map[string]int, len(allDomains))
	domainsByTile := make(map[string][]string, len(allDomains))
	for _, d := range allDomains {
		domainsPerTile[d.TileID]++
		domainsByTile[d.TileID] = append(domainsByTile[d.TileID], d.Host)
	}

	// Live port-forwards per tile, rolled into a count chip per stack card,
	// same idea as DomainCount. Union of relay containers and open CLI
	// sessions, distinct per (tile, port), matching the env canvas's cards.
	type fwdKey struct {
		tile string
		port int
	}
	fwdSeen := map[fwdKey]bool{}
	if h.rt != nil {
		if relays, err := h.rt.ListProxyRelays(ctx); err == nil {
			for _, r := range relays {
				fwdSeen[fwdKey{r.TileID, r.Port}] = true
			}
		}
	}
	if h.forwards != nil {
		for _, s := range h.forwards.Sessions() {
			fwdSeen[fwdKey{s.TileID, s.Port}] = true
		}
	}
	forwardsPerTile := map[string]int{}
	for k := range fwdSeen {
		forwardsPerTile[k.tile]++
	}

	// Tiles shared with the whole org are cards of their own; every stack that
	// provisions from one gets an edge to it.
	shared := map[string]repo.Tile{}
	// traffic endpoints -> the card standing for them on this canvas
	nodeOf := map[string]string{"proxy": graph.ProxyNodeID}
	var summaries []graph.StackSummary
	for i := range stacks {
		st := &stacks[i]
		envs, err := h.store.ListEnvironmentsByStack(ctx, st.ID)
		if err != nil {
			return graph.Graph{}, nil, err
		}
		tiles, err := h.store.ListTilesByStack(ctx, st.ID)
		if err != nil {
			return graph.Graph{}, nil, err
		}
		s := graph.StackSummary{
			ID:     st.ID,
			Slug:   st.Slug,
			Name:   st.Name,
			Href:   "/" + o.Slug + "/" + st.Slug,
			Envs:   len(envs),
			Status: graph.WorstStatus(tiles),
		}
		// Only a stack with both a connector and a repo is actually defined as
		// code; a half-configured one must not claim the config line.
		if st.ConfigManaged() {
			s.ConfigConnectorID = st.ConfigConnectorID
		}
		for j := range tiles {
			t := &tiles[j]
			s.DomainCount += domainsPerTile[t.ID]
			s.ForwardCount += forwardsPerTile[t.ID]
			graph.MapTile(nodeOf, t.ID, graph.StackNodeID(st.Slug))
			if t.ScopeKind == "org" {
				shared[t.ID] = *t
			}
			if t.ConnectorID != "" && !contains(s.SourceConnectorIDs, t.ConnectorID) {
				s.SourceConnectorIDs = append(s.SourceConnectorIDs, t.ConnectorID)
			}
		}
		// Instances this stack consumes that are shared org-wide, including
		// ones owned by another stack.
		for j := range tiles {
			ps, err := h.store.ListProvisionsByConsumer(ctx, tiles[j].ID)
			if err != nil {
				continue
			}
			for _, p := range ps {
				inst, _ := h.store.GetTile(ctx, p.InstanceTileID)
				if inst == nil || inst.ScopeKind != "org" {
					continue
				}
				shared[inst.ID] = *inst
				if !contains(s.InstanceIDs, inst.ID) {
					s.InstanceIDs = append(s.InstanceIDs, inst.ID)
				}
			}
		}
		summaries = append(summaries, s)
	}

	instances := make([]graph.OrgInstance, 0, len(shared))
	for id, t := range shared {
		// The org instance is its own card, so its flows leave the stack that
		// happens to host the container, the stack->instance edge is the lane.
		graph.MapTile(nodeOf, id, graph.DBNodeID(id))
		detail := t.Engine
		if detail == "" {
			detail = t.SourceType
		}
		inst := graph.OrgInstance{
			ID: id, Name: t.Name, Detail: detail + " · managed", Engine: t.Engine, Status: t.Status,
			Href: "/dbs/" + id, ExternalPort: t.ExternalPort,
		}
		inst.Domains = append(inst.Domains, domainsByTile[id]...)
		instances = append(instances, inst)
	}

	var connectors []graph.OrgConnector
	if cs, err := h.store.ListConnectorsByOrg(ctx, o.ID); err == nil {
		for _, cn := range cs {
			connectors = append(connectors, graph.OrgConnector{
				ID: cn.ID, Name: cn.Name, Detail: cn.Provider, Provider: cn.Provider,
				Href: "/orgs/" + o.Slug + "/settings/connectors",
			})
		}
	}

	// No card for the org itself, it's the canvas you're on. Administration is
	// the header's Settings link (graph.templ).
	g := graph.BuildOrg(summaries, instances, connectors, h.orgVarCards(ctx, o, stacks), positions)
	g.Arrange(style, positions)
	g.Annotations, _ = h.store.ListAnnotations(ctx, repo.GraphOwner(repo.ScopeOrg, o.ID))
	g.Groups, _ = h.store.ListGraphGroups(ctx, repo.GraphOwner(repo.ScopeOrg, o.ID))
	return g, nodeOf, nil
}

// orgVarCards splits the org's variables into plain and secret, and works out
// which stacks reference each class. Secrecy comes from the variables table:
// the varref catalogue never lists scope-shared entries
// (internal/varref/catalogue.go), so counting from it would under-report.
func (h *handler) orgVarCards(ctx context.Context, o *repo.Org, stacks []repo.Stack) graph.VarCards {
	// Href points at the org's variables section. It used to point at the
	// global settings page, which has never had a variables section, clicking
	// an org variables card was a dead end.
	cards := graph.VarCards{Scope: "org", Label: "Organization", Href: "/orgs/" + o.Slug + "/settings/variables"}
	vars, err := h.store.ListVariables(ctx, repo.OwnerOrg, o.ID)
	if err != nil || len(vars) == 0 {
		return cards
	}
	secret := make(map[string]bool, len(vars))
	for _, v := range vars {
		secret[v.Name] = v.Secret
		if v.Secret {
			cards.Secret++
		} else {
			cards.Plain++
		}
	}
	for i := range stacks {
		tiles, err := h.store.ListTilesByStack(ctx, stacks[i].ID)
		if err != nil {
			continue
		}
		ids := make([]string, 0, len(tiles))
		for j := range tiles {
			ids = append(ids, tiles[j].ID)
		}
		node := graph.StackNodeID(stacks[i].Slug)
		usesPlain, usesSecret := false, false
		for name := range varref.ScopeVarsUsed(ctx, h.store, "org", ids) {
			s, known := secret[name]
			if !known {
				continue // references a variable that doesn't exist; deploy will say so
			}
			if s {
				usesSecret = true
			} else {
				usesPlain = true
			}
		}
		if usesPlain {
			cards.PlainBy = append(cards.PlainBy, node)
		}
		if usesSecret {
			cards.SecretBy = append(cards.SecretBy, node)
		}
	}
	return cards
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
