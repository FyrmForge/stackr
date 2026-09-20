package project

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
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// StackGraph renders the stack canvas: one card per environment, plus the
// logical databases and shared instances those environments consume.
// GET /:org/:stack
func (h *handler) StackGraph(c echo.Context) error {
	ctx := c.Request().Context()
	p, err := h.resolveStackSlugs(c)
	if err != nil {
		return err
	}
	g, _, err := h.buildStackGraph(ctx, p, arrangeStyle(c))
	if err != nil {
		return err
	}
	pendingPlan, _ := h.store.LatestConfigPlan(ctx, p.ID)
	if pendingPlan != nil && pendingPlan.Status != "pending" && pendingPlan.Status != "error" {
		pendingPlan = nil
	}
	cmp, colors, err := h.compareStack(ctx, p)
	if err != nil {
		return err
	}
	return respond.HTML(c, http.StatusOK, stackGraphPage(c, p, g, pendingPlan, cmp, colors, h.commitLog(ctx, p)))
}

// StackGraphStatus is the live snapshot the canvas re-fetches on ws events.
// GET /projects/:id/graph/status
func (h *handler) StackGraphStatus(c echo.Context) error {
	ctx := c.Request().Context()
	p, err := h.loadStack(c, c.Param("id"))
	if err != nil {
		return err
	}
	g, nodeOf, err := h.buildStackGraph(ctx, p, arrangeStyle(c))
	if err != nil {
		return err
	}
	body := map[string]any{"nodes": canvas.StatusNodes(ctx, g.Nodes)}
	if h.sampler != nil {
		body["traffic"] = graph.RollupTraffic(h.sampler.Snapshot().Pairs, nodeOf)
	}
	return c.JSON(http.StatusOK, body)
}

// SaveStackNodePosition persists dragged positions on the stack canvas.
// Mirrors SaveNodePosition (env level), same single/batch body.
// POST /projects/:id/graph/positions
func (h *handler) SaveStackNodePosition(c echo.Context) error {
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
		return echo.NewHTTPError(http.StatusBadRequest, "node_id, x, y required")
	}
	if in.NodeID != "" {
		in.Nodes = append(in.Nodes, in.pos)
	}
	p, err := h.loadStack(c, c.Param("id"))
	if err != nil {
		return err
	}
	owner := repo.GraphOwner(repo.ScopeStack, p.ID)
	ps := make([]repo.NodePosition, 0, len(in.Nodes))
	for _, q := range in.Nodes {
		ps = append(ps, repo.NodePosition{NodeID: q.NodeID, X: q.X, Y: q.Y})
	}
	if err := repo.ValidateNodePositions(owner, ps); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if err := h.graph.SavePositions(c.Request().Context(), owner, ps); err != nil {
		return err
	}
	h.notifier.Project(p.ID)
	return c.NoContent(http.StatusNoContent)
}

// SaveStackAnnotation / DeleteStackAnnotation edit the stack canvas's notes.
// POST /projects/:id/graph/annotations, /projects/:id/graph/annotations/delete
func (h *handler) SaveStackAnnotation(c echo.Context) error {
	p, err := h.loadStack(c, c.Param("id"))
	if err != nil {
		return err
	}
	if err := annotate.Save(c, h.store, repo.GraphOwner(repo.ScopeStack, p.ID)); err != nil {
		return err
	}
	h.notifier.Project(p.ID)
	return nil
}

func (h *handler) DeleteStackAnnotation(c echo.Context) error {
	p, err := h.loadStack(c, c.Param("id"))
	if err != nil {
		return err
	}
	if err := annotate.Delete(c, h.store, repo.GraphOwner(repo.ScopeStack, p.ID)); err != nil {
		return err
	}
	h.notifier.Project(p.ID)
	return nil
}

// SaveEnvAnnotation / DeleteEnvAnnotation edit the env canvas's notes.
// POST /envs/:id/graph/annotations, /envs/:id/graph/annotations/delete
func (h *handler) SaveEnvAnnotation(c echo.Context) error {
	env, stackID, err := h.loadEnvForAnnotation(c)
	if err != nil {
		return err
	}
	if err := annotate.Save(c, h.store, repo.GraphOwner(repo.ScopeEnv, env)); err != nil {
		return err
	}
	h.notifier.Project(stackID)
	return nil
}

func (h *handler) DeleteEnvAnnotation(c echo.Context) error {
	env, stackID, err := h.loadEnvForAnnotation(c)
	if err != nil {
		return err
	}
	if err := annotate.Delete(c, h.store, repo.GraphOwner(repo.ScopeEnv, env)); err != nil {
		return err
	}
	h.notifier.Project(stackID)
	return nil
}

// SaveStackGraphGroup / DeleteStackGraphGroup edit the stack canvas's groups.
// POST /projects/:id/graph/groups, /projects/:id/graph/groups/delete
func (h *handler) SaveStackGraphGroup(c echo.Context) error {
	p, err := h.loadStack(c, c.Param("id"))
	if err != nil {
		return err
	}
	if err := annotate.SaveGroup(c, h.store, repo.GraphOwner(repo.ScopeStack, p.ID)); err != nil {
		return err
	}
	h.notifier.Project(p.ID)
	return nil
}

func (h *handler) DeleteStackGraphGroup(c echo.Context) error {
	p, err := h.loadStack(c, c.Param("id"))
	if err != nil {
		return err
	}
	if err := annotate.DeleteGroup(c, h.store, repo.GraphOwner(repo.ScopeStack, p.ID)); err != nil {
		return err
	}
	h.notifier.Project(p.ID)
	return nil
}

// SaveEnvGraphGroup / DeleteEnvGraphGroup edit the env canvas's groups.
// POST /envs/:id/graph/groups, /envs/:id/graph/groups/delete
func (h *handler) SaveEnvGraphGroup(c echo.Context) error {
	env, stackID, err := h.loadEnvForAnnotation(c)
	if err != nil {
		return err
	}
	if err := annotate.SaveGroup(c, h.store, repo.GraphOwner(repo.ScopeEnv, env)); err != nil {
		return err
	}
	h.notifier.Project(stackID)
	return nil
}

func (h *handler) DeleteEnvGraphGroup(c echo.Context) error {
	env, stackID, err := h.loadEnvForAnnotation(c)
	if err != nil {
		return err
	}
	if err := annotate.DeleteGroup(c, h.store, repo.GraphOwner(repo.ScopeEnv, env)); err != nil {
		return err
	}
	h.notifier.Project(stackID)
	return nil
}

// loadEnvForAnnotation resolves + authorizes the env in the URL, the same
// check SaveNodePosition (env level) runs.
func (h *handler) loadEnvForAnnotation(c echo.Context) (envID, stackID string, err error) {
	env, err := h.envs.Get(c.Request().Context(), c.Param("id"))
	if err != nil {
		return "", "", stackrmw.HTTP(err)
	}
	return env.ID, env.StackID, nil
}

// ResetStackNodePositions drops the stack canvas layout.
// POST /projects/:id/graph/positions/reset
func (h *handler) ResetStackNodePositions(c echo.Context) error {
	p, err := h.loadStack(c, c.Param("id"))
	if err != nil {
		return err
	}
	if err := h.graph.ResetPositions(c.Request().Context(), repo.GraphOwner(repo.ScopeStack, p.ID)); err != nil {
		return err
	}
	h.notifier.Project(p.ID)
	return c.NoContent(http.StatusNoContent)
}

// resolveStackSlugs maps /:org/:stack to its row, org access checked.
func (h *handler) resolveStackSlugs(c echo.Context) (*repo.Stack, error) {
	ctx := c.Request().Context()
	org, err := h.orgs.BySlug(ctx, c.Param("org"))
	if err != nil {
		return nil, stackrmw.HTTP(err)
	}
	p, err := h.stacks.BySlug(ctx, org.ID, c.Param("stack"))
	if err != nil {
		return nil, stackrmw.HTTP(err)
	}
	p.OrgSlug = org.Slug
	return p, nil
}

// buildStackGraph rolls each environment up into one card and links it to the
// slices and shared instances its tiles reference. The second return maps
// sampler endpoints ("app:<tile>", "db:<tile>", "proxy") to the card standing
// for them here, for graph.RollupTraffic.
func (h *handler) buildStackGraph(ctx context.Context, p *repo.Stack, style graph.ArrangeStyle) (graph.Graph, map[string]string, error) {
	h.fillOrg(ctx, p)
	envs, err := h.envs.ListForStack(ctx, p.ID)
	if err != nil {
		return graph.Graph{}, nil, err
	}
	rows, err := h.graph.Positions(ctx, repo.GraphOwner(repo.ScopeStack, p.ID))
	if err != nil {
		return graph.Graph{}, nil, err
	}
	positions := make(map[string][2]float64, len(rows))
	for _, r := range rows {
		positions[r.NodeID] = [2]float64{r.X, r.Y}
	}

	// Hosts by tile, so each env card can show what of it is reachable from
	// outside (and hang off the Traefik card).
	allDomains, err := h.domains.ListAll(ctx)
	if err != nil {
		return graph.Graph{}, nil, err
	}
	hosts := make(map[string][]string, len(allDomains))
	for _, d := range allDomains {
		hosts[d.TileID] = append(hosts[d.TileID], d.Host)
	}

	// Live port-forwards per tile, a count chip per env card, the same rollup
	// DomainCount does one level up. Union of relay containers and open CLI
	// sessions, distinct per (tile, port), matching the env canvas's cards.
	forwardsPerTile := h.forwardCounts(ctx)
	colors := h.envColorsByID(ctx, p, envs)

	var (
		summaries []graph.EnvSummary
		instances []graph.StackInstance
		refs      []graph.Reference
		seenRef   = map[string]bool{}
		seenInst  = map[string]bool{}
		// traffic endpoints -> the card standing for them on this canvas
		nodeOf = map[string]string{"proxy": graph.ProxyNodeID}
	)
	for i := range envs {
		env := &envs[i]
		tiles, err := h.tiles.ListForEnv(ctx, env.ID)
		if err != nil {
			return graph.Graph{}, nil, err
		}
		staged, _ := h.store.CountStagedByEnv(ctx, env.ID)
		s := graph.EnvSummary{
			ID:     env.ID,
			Slug:   env.Slug,
			Name:   env.Name,
			Href:   envURL(p, env),
			Tiles:  len(tiles),
			Status: graph.WorstStatus(tiles),
			Staged: staged > 0,
			Color:  colors[env.ID],
		}
		for j := range tiles {
			s.Domains = append(s.Domains, hosts[tiles[j].ID]...)
			s.ForwardCount += forwardsPerTile[tiles[j].ID]
			graph.MapTile(nodeOf, tiles[j].ID, graph.EnvNodeID(env.Slug))
		}

		// The canvases form a tree and a managed tile sits at exactly one
		// level of it, its scope level. Stack-scoped instances this env
		// provisions from are this canvas's residents; slices and env-scoped
		// instances stay inside the env cards; org-scoped ones live one level
		// up and appear here only as ghost references, so the dependency the
		// org canvas draws doesn't vanish on the way down. Ghosts below cover
		// instances owned outside this stack too.
		envRes, err := h.store.ListResourcesByEnv(ctx, env.ID)
		if err != nil {
			return graph.Graph{}, nil, err
		}
		linked := map[string]bool{} // instance ids this env already has an edge to
		for _, r := range envRes {
			inst, _ := h.tiles.Get(ctx, r.ProviderTileID)
			if inst == nil || linked[inst.ID] {
				continue
			}
			switch {
			case inst.StackID == p.ID && inst.ScopeKind == "stack":
				linked[inst.ID] = true
				s.InstanceIDs = append(s.InstanceIDs, inst.ID)
				if seenInst[inst.ID] {
					continue
				}
				seenInst[inst.ID] = true
				instances = append(instances, graph.StackInstance{
					ID: inst.ID, Name: inst.Name,
					Detail: inst.Engine + " · " + graph.ScopeSuffix(inst.ScopeKind),
					Engine: inst.Engine, Status: inst.Status,
					Href:         "/dbs/" + inst.ID,
					Domains:      hosts[inst.ID],
					ExternalPort: inst.ExternalPort,
				})
			case inst.ScopeKind == "org":
				linked[inst.ID] = true
				s.RefIDs = append(s.RefIDs, inst.ID)
				if seenRef[inst.ID] {
					continue
				}
				seenRef[inst.ID] = true
				refs = append(refs, graph.Reference{
					InstanceID: inst.ID, Name: inst.Name,
					Detail: inst.Engine + " · " + graph.ScopeSuffix(inst.ScopeKind),
					Engine: inst.Engine, Href: "/dbs/" + inst.ID,
					Domains: hosts[inst.ID], ExternalPort: inst.ExternalPort,
				})
			}
		}

		// Instances owned by another environment: ghost cards, once each.
		// Stack-scoped in-stack instances are excluded, they have a real card
		// here now.
		for _, ref := range h.sharedRefs(ctx, env.ID, tiles) {
			if linked[ref.InstanceID] {
				continue // already a resident card or org-scoped ghost for this env
			}
			s.RefIDs = append(s.RefIDs, ref.InstanceID)
			if seenRef[ref.InstanceID] {
				continue
			}
			seenRef[ref.InstanceID] = true
			refs = append(refs, ref)
		}
		summaries = append(summaries, s)
	}
	// Shared instances nothing provisions from yet: still residents, they
	// live in the stack's home.
	if home, err := h.store.HomeEnvironment(ctx, p.ID); err == nil && home != nil {
		tiles, err := h.tiles.ListForEnv(ctx, home.ID)
		if err != nil {
			return graph.Graph{}, nil, err
		}
		for i := range tiles {
			inst := &tiles[i]
			if seenInst[inst.ID] || inst.ScopeKind != "stack" {
				continue
			}
			seenInst[inst.ID] = true
			instances = append(instances, graph.StackInstance{
				ID: inst.ID, Name: inst.Name,
				Detail: inst.Engine + " · " + graph.ScopeSuffix(inst.ScopeKind),
				Engine: inst.Engine, Status: inst.Status,
				Href:         "/dbs/" + inst.ID,
				Domains:      hosts[inst.ID],
				ExternalPort: inst.ExternalPort,
			})
		}
	}
	ghosts := refs[:0]
	for _, ref := range refs {
		if !seenInst[ref.InstanceID] {
			ghosts = append(ghosts, ref)
		}
	}
	// A stack-scoped instance owns its lane: traffic into its container maps
	// onto its own card, not the env card holding it.
	for _, in := range instances {
		graph.MapTile(nodeOf, in.ID, graph.DBNodeID(in.ID))
	}
	for _, ref := range ghosts {
		graph.MapTile(nodeOf, ref.InstanceID, graph.RefNodeID(ref.InstanceID))
	}
	g := graph.BuildStack(summaries, instances, ghosts, h.stackVarCards(ctx, p, envs), positions)
	g.Arrange(style, positions)
	g.Annotations, _ = h.graph.Annotations(ctx, repo.GraphOwner(repo.ScopeStack, p.ID))
	g.Groups, _ = h.graph.Groups(ctx, repo.GraphOwner(repo.ScopeStack, p.ID))
	return g, nodeOf, nil
}

// envVarCards is the env canvas's pair of variable cards: this environment's
// own values, which are read through the stack namespace, a tile asking for
// ${{ stack.NAME }} gets the env row before the stack's, so a card here wires
// to whichever tiles name a variable this environment overrides.
func (h *handler) envVarCards(ctx context.Context, envID string, tiles []repo.Tile) graph.VarCards {
	cards := graph.VarCards{Scope: "env", Label: "Environment"}
	env, err := h.envs.Get(ctx, envID)
	if err != nil {
		return cards
	}
	p, err := h.stacks.Get(ctx, env.StackID)
	if err != nil {
		return cards
	}
	h.fillOrg(ctx, p)
	// Href is never navigated to (the cards open the drawer): the canvas
	// derives the panel URL from it by appending /panel.
	cards.Href = stackURL(p) + "/settings/environments/" + env.Slug + "/variables"
	vars, err := h.vars.List(ctx, service.EnvVars(env.ID))
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
	for i := range tiles {
		usesPlain, usesSecret := false, false
		for name := range varref.ScopeVarsUsed(ctx, h.store, "stack", []string{tiles[i].ID}) {
			s, known := secret[name]
			if !known {
				continue // this environment doesn't override that one
			}
			if s {
				usesSecret = true
			} else {
				usesPlain = true
			}
		}
		node := graph.AppNodeID(tiles[i].ID)
		if tiles[i].IsManaged() {
			node = graph.DBNodeID(tiles[i].ID)
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

// stackVarCards splits the stack's variables into plain and secret, and works
// out which environments reference each class. Secrecy comes from the variables
// table, not the varref catalogue, see orgVarCards for why.
func (h *handler) stackVarCards(ctx context.Context, p *repo.Stack, envs []repo.Environment) graph.VarCards {
	cards := graph.VarCards{Scope: "stack", Label: "Stack", Href: stackURL(p) + "/settings/variables"}
	vars, err := h.vars.List(ctx, service.StackVars(p.ID))
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
	for i := range envs {
		tiles, err := h.tiles.ListForEnv(ctx, envs[i].ID)
		if err != nil {
			continue
		}
		ids := make([]string, 0, len(tiles))
		for j := range tiles {
			ids = append(ids, tiles[j].ID)
		}
		node := graph.EnvNodeID(envs[i].Slug)
		usesPlain, usesSecret := false, false
		for name := range varref.ScopeVarsUsed(ctx, h.store, "stack", ids) {
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
