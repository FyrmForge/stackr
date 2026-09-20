// Package search is the global search palette's backend: one endpoint that
// substring-matches everything the viewer can navigate to and returns an HTML
// fragment of result rows.
//
// matching is in-memory over the existing listers, no FTS, no
// index. Every keystroke re-reads whole tables, which is microseconds at this
// scale; add SQLite FTS if a table ever reaches tens of thousands of rows.
package search

import (
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/graph"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

type handler struct {
	store repo.Store
	envs  *service.EnvironmentService
}

func NewHandler(store repo.Store, envs *service.EnvironmentService) *handler {
	return &handler{store: store, envs: envs}
}

// result is one palette row.
type result struct {
	Kind    string // chip label: org, stack, environment, service, ...
	Title   string
	Context string // where it lives, e.g. "acme / shop / production"
	Href    string
	rank    int // 0 = prefix match, 1 = substring
}

const maxResults = 20

// Search answers GET /search?q= with result rows. Everything is scoped to the
// orgs OrgContext resolved for this user, admins see all orgs there, so they
// search everything.
func (h *handler) Search(c echo.Context) error {
	q := strings.ToLower(strings.TrimSpace(c.QueryParam("q")))
	if q == "" {
		return respond.HTML(c, http.StatusOK, resultList(nil, ""))
	}
	ctx := c.Request().Context()

	orgByID := map[string]repo.Org{}
	for _, o := range middleware.Orgs(c) {
		orgByID[o.ID] = o
	}

	var rs []result
	// add records a row when q matches the title or any extra match string.
	add := func(kind, title, context, href string, extra ...string) {
		best := -1
		for _, s := range append([]string{title}, extra...) {
			if r := fuzzyRank(s, q); r >= 0 && (best < 0 || r < best) {
				best = r
			}
		}
		if best < 0 {
			return
		}
		rs = append(rs, result{Kind: kind, Title: title, Context: context, Href: href, rank: best})
	}
	focus := func(base, nodeID string) string { return base + "?focus=" + url.QueryEscape(nodeID) }

	// Orgs, cards on the root canvas.
	for _, o := range orgByID {
		add("org", o.Name, "organization", focus("/", graph.OrgNodeID(o.ID)), o.Slug)
	}

	// Stacks, and the maps everything below resolves through.
	stacks, err := h.store.ListStacks(ctx)
	if err != nil {
		return err
	}
	stackByID := map[string]repo.Stack{}
	stackPath := map[string]string{} // stack id -> "/org/stack"
	for _, s := range stacks {
		o, ok := orgByID[s.OrgID]
		if !ok {
			continue
		}
		stackByID[s.ID] = s
		stackPath[s.ID] = "/" + o.Slug + "/" + s.Slug
		add("stack", s.Name, o.Slug, focus("/orgs/"+o.Slug, graph.StackNodeID(s.Slug)), s.Slug)
	}

	// Environments, per-stack lister; also the env map tiles resolve through.
	envByID := map[string]repo.Environment{}
	envPath := map[string]string{} // env id -> "/org/stack/env"
	for id, sp := range stackPath {
		envs, err := h.envs.ListForStack(ctx, id)
		if err != nil {
			return err
		}
		for _, e := range envs {
			envByID[e.ID] = e
			envPath[e.ID] = sp + "/" + e.Slug
			add("environment", e.Name, strings.TrimPrefix(sp, "/"), focus(sp, graph.EnvNodeID(e.Slug)), e.Slug)
		}
	}

	// Managed-resource slices, gathered before tiles so a slice-hosting
	// instance (which draws no card of its own) can focus its first slice.
	slicesOf := map[string][]repo.ManagedResource{} // provider tile id -> slices
	for id := range envByID {
		res, err := h.store.ListResourcesByEnv(ctx, id)
		if err != nil {
			return err
		}
		for _, r := range res {
			slicesOf[r.ProviderTileID] = append(slicesOf[r.ProviderTileID], r)
			add("slice", r.Slug, strings.TrimPrefix(envPath[r.EnvironmentID], "/"),
				focus(envPath[r.EnvironmentID], graph.ResourceNodeID(r.ID)))
		}
	}

	// Tiles. The focus target depends on how the canvas draws the tile: an
	// attached volume rides under its service, a slice-hosting instance is
	// replaced by its slices, everything else has its own card.
	tiles, err := h.store.ListTiles(ctx)
	if err != nil {
		return err
	}
	tileByID := map[string]repo.Tile{}
	tileFocus := map[string]string{} // tile id -> canvas URL focusing its card
	for _, t := range tiles {
		if _, ok := stackByID[t.StackID]; !ok {
			continue
		}
		base, node := tileTarget(t, envPath, slicesOf)
		if base == "" {
			continue
		}
		tileByID[t.ID] = t
		tileFocus[t.ID] = focus(base, node)
		add(tileKind(t), t.Name, strings.TrimPrefix(base, "/"), tileFocus[t.ID], t.Slug)
	}

	// Domains, land on the owning tile's card.
	domains, err := h.store.ListDomains(ctx)
	if err != nil {
		return err
	}
	for _, d := range domains {
		t, ok := tileByID[d.TileID]
		if !ok {
			continue
		}
		add("domain", d.Host, t.Name, tileFocus[d.TileID])
	}

	// Connectors, cards on the org canvas.
	connectors, err := h.store.ListConnectors(ctx)
	if err != nil {
		return err
	}
	for _, cn := range connectors {
		o, ok := orgByID[cn.OrgID]
		if !ok {
			continue
		}
		add("connector", cn.Name, cn.Provider+" · "+o.Slug,
			focus("/orgs/"+o.Slug, graph.ConnectorNodeID(cn.ID)), cn.Provider)
	}

	// Variable names, never values (see ListVariableNames).
	vars, err := h.store.ListVariableNames(ctx)
	if err != nil {
		return err
	}
	for _, v := range vars {
		kind := "variable"
		if v.Secret {
			kind = "secret"
		}
		switch v.OwnerKind {
		case repo.OwnerOrg:
			if o, ok := orgByID[v.OwnerID]; ok {
				add(kind, v.Name, o.Slug, "/orgs/"+o.Slug+"/settings/variables")
			}
		case repo.OwnerStack:
			if sp, ok := stackPath[v.OwnerID]; ok {
				add(kind, v.Name, strings.TrimPrefix(sp, "/"), sp+"/settings/variables")
			}
		case repo.OwnerEnv:
			if e, ok := envByID[v.OwnerID]; ok {
				sp := stackPath[e.StackID]
				add(kind, v.Name, strings.TrimPrefix(envPath[v.OwnerID], "/"), sp+"/settings/variables")
			}
		case repo.OwnerTile:
			if t, ok := tileByID[v.OwnerID]; ok {
				add(kind, v.Name, t.Name, envPath[t.EnvironmentID]+"/"+t.Slug+"?tab=variables")
			}
		}
	}

	// Backups, named by what they back up.
	backups, err := h.store.ListBackups(ctx)
	if err != nil {
		return err
	}
	for _, b := range backups {
		t, ok := tileByID[b.TileID.String]
		if !ok {
			continue // includes kind "stackr" (panel db, admin-only, no tile)
		}
		add("backup", t.Name+" "+b.Kind+" backup", strings.TrimPrefix(envPath[t.EnvironmentID], "/"),
			envPath[t.EnvironmentID]+"/"+t.Slug+"?tab=backups")
	}

	// Members and config plans, per-org / per-stack queries, both small.
	for _, o := range orgByID {
		members, err := h.store.ListOrgMembers(ctx, o.ID)
		if err != nil {
			return err
		}
		for _, m := range members {
			add("member", m.Name, m.Email+" · "+o.Slug, "/orgs/"+o.Slug+"/settings/members", m.Email)
		}
	}
	for id, s := range stackByID {
		if !s.ConfigManaged() {
			continue
		}
		plans, err := h.store.ListConfigPlans(ctx, id, 10)
		if err != nil {
			return err
		}
		for _, cp := range plans {
			add("plan", cp.Summary, cp.Status+" · "+strings.TrimPrefix(stackPath[id], "/"),
				"/projects/"+id+"/config/plans/"+cp.ID, cp.CommitSHA)
		}
	}

	// Settings and fixed pages.
	for _, o := range orgByID {
		for _, sub := range []string{"general", "members", "invites", "connectors", "variables", "domains", "backups"} {
			add("page", sub+" settings", o.Slug, "/orgs/"+o.Slug+"/settings/"+sub)
		}
	}
	for id, sp := range stackPath {
		_ = id
		for _, sub := range []string{"general", "config", "variables", "domains", "environments", "pr"} {
			add("page", sub+" settings", strings.TrimPrefix(sp, "/"), sp+"/settings/"+sub)
		}
	}
	add("page", "account", "", "/account")
	add("page", "notifications", "", "/notifications")
	if middleware.IsAdmin(c) {
		add("page", "admin", "", "/admin")
		add("page", "containers", "", "/containers")
		add("page", "servers", "", "/servers")
	}

	sort.SliceStable(rs, func(i, j int) bool { return rs[i].rank < rs[j].rank })
	if len(rs) > maxResults {
		rs = rs[:maxResults]
	}
	return respond.HTML(c, http.StatusOK, resultList(rs, q))
}

// fuzzyRank scores how well q (already lowercased) matches s, lower is
// better: 0 = prefix, 1 = substring elsewhere, 2+gaps = fzf-style in-order
// subsequence ("sdb" hits "site-db", tighter spreads rank higher), -1 = no
// match. Bytewise on the lowered strings, entity names here are ASCII slugs
// and hostnames, so no rune handling.
func fuzzyRank(s, q string) int {
	s = strings.ToLower(s)
	switch i := strings.Index(s, q); {
	case i == 0:
		return 0
	case i > 0:
		return 1
	}
	first, last, pos := -1, -1, 0
	for i := 0; i < len(s) && pos < len(q); i++ {
		if s[i] == q[pos] {
			if first < 0 {
				first = i
			}
			last = i
			pos++
		}
	}
	if pos < len(q) {
		return -1
	}
	return 2 + (last - first + 1 - len(q)) // + how many gap chars the match spans
}

// tileTarget names the canvas a tile's card lives on and the node id to focus.
func tileTarget(t repo.Tile, envPath map[string]string, slicesOf map[string][]repo.ManagedResource) (base, node string) {
	base = envPath[t.EnvironmentID]
	switch t.ScopeKind {
	case "org", "stack", "server":
		// Shared instances draw on the org/stack canvas; the env canvas only
		// holds their ghosts. Focusing the ghost of the env it consumes from
		// would be arbitrary, so land where the real card is, but resolving
		// scope id -> canvas URL is caller work we skip: the env path is ""
		// for these anyway, and their slices (searched separately) carry the
		// navigation weight. revisit if shared instances multiply.
		if base == "" {
			return "", ""
		}
	}
	if base == "" {
		return "", ""
	}
	switch {
	case t.Kind == "volume" && t.AttachedTileID != "":
		return base, graph.AppNodeID(t.AttachedTileID) // rides under its service
	case t.IsManaged():
		if sl := slicesOf[t.ID]; len(sl) > 0 {
			return base, graph.ResourceNodeID(sl[0].ID) // its slices stand in for it
		}
		return base, graph.DBNodeID(t.ID)
	default:
		return base, graph.AppNodeID(t.ID)
	}
}

func tileKind(t repo.Tile) string {
	switch {
	case t.IsManaged():
		return "database"
	case t.Kind == "cron":
		return "cron"
	case t.Kind == "function":
		return "function"
	case t.Kind == "volume":
		return "volume"
	default:
		return "service"
	}
}
