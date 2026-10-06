package canvas

import (
	"cmp"
	"encoding/json"
	"slices"
	"strconv"
	"strings"

	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	envui "github.com/FyrmForge/stackr/internal/ui/drawer/env"
	ui "github.com/FyrmForge/stackr/internal/ui/graph"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// syncQ is the review a request names: ?sync=<source slug>&drop=a,b on the
// env page and on its drawer route. No source, no review.
type syncQ struct {
	from string
	drop []string
}

func syncOf(c echo.Context) syncQ {
	q := syncQ{from: c.QueryParam("sync")}
	for _, s := range strings.Split(c.QueryParam("drop"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			q.drop = append(q.drop, s)
		}
	}
	return q
}

// query is the review as the env page carries it, "sync=dev&drop=a,b".
func (q syncQ) query() string {
	s := "sync=" + q.from
	if len(q.drop) > 0 {
		s += "&drop=" + strings.Join(q.drop, ",")
	}
	return s
}

// syncConfirm is the question Deploy asks, on the bar and in the drawer.
// drawer is the env's drawer route; env is the name of the env synced into.
// The bar's answer swaps nothing (the drawer it would swap may be closed):
// it posts bar=1 and the server redirects on a refusal too.
func syncConfirm(drawer, env, from string, keep []string, sig string, bar bool) *comp.ConfirmView {
	vals := map[string]any{"keep": keep, "sig": sig}
	target := "#" + comp.DrawerRoot
	if bar {
		vals["bar"], target = "1", ""
	}
	raw, _ := json.Marshal(vals)
	return &comp.ConfirmView{
		Button:  "Deploy",
		Title:   "Sync " + count(len(keep)) + " from " + from + " into " + env + "?",
		Warning: "Rollback cannot undo a sync.",
		Primary: true,
		Action:  drawer + "/sync/" + from,
		Target:  target,
		Vals:    string(raw),
	}
}

// syncBar is the strip over the canvas for a review of the plan pl. Deploy
// is the service's verdict here; the caller adds the viewer's right to it.
func syncBar(l level, pl service.EnvSyncPlan, q syncQ) *ui.SyncBar {
	b := &ui.SyncBar{
		Review: l.base + "/-/drawer?tab=sync&" + q.query(),
		Cancel: l.base,
	}
	var keep []string
	for _, t := range pl.Plan.Tiles {
		if !t.Dropped {
			keep = append(keep, t.Slug)
		}
	}
	if pl.CanDeploy {
		b.Deploy = syncConfirm(l.base+"/-/drawer", l.title, q.from, keep, pl.Plan.Sig, true)
	}
	b.Text = count(len(keep)) + " from " + q.from
	if pl.Plan.Blocked() {
		b.Text = "Sync from " + q.from + " is blocked"
	}
	return b
}

func count(n int) string {
	if n == 1 {
		return "1 tile"
	}
	return strconv.Itoa(n) + " tiles"
}

// syncButton is the env page's Sync action: for anyone who can read the
// page, on a stack that can sync at all.
func (h *handler) syncButton(c echo.Context, l level) ([]ui.Create, error) {
	if l.scope.Kind != service.CanvasEnv {
		return nil, nil
	}
	src, err := h.orch.EnvSyncSources(c.Request().Context(), l.scope.ID)
	if err != nil || !src.Can {
		return nil, err
	}
	return []ui.Create{{Label: "Sync", URL: l.base + "/-/drawer?tab=sync"}}, nil
}

// reviewOpen says an htmx request moves the review under an open drawer:
// the env page, a review on it and its own sync tab named by ?drawer=.
func reviewOpen(c echo.Context, q syncQ) bool {
	s := middleware.ScopeOf(c)
	return isHTMX(c) && q.from != "" && s.Env != nil && c.QueryParam("tab") == "sync" &&
		c.QueryParam("drawer") == "env:"+s.Env.ID
}

// syncTab is the env drawer's sync tab: the job of a deploy (?job=), else
// the source (?sync=, or the default), its plan minus ?drop= and Deploy for
// whoever may write.
func (h *handler) syncTab(c echo.Context, cd card, f *comp.DrawerView) (templ.Component, error) {
	ctx, e := c.Request().Context(), cd.s.Env
	src, err := h.orch.EnvSyncSources(ctx, e.ID)
	if err != nil {
		return nil, err
	}
	if !src.Can {
		return envui.Sync(envui.SyncView{Why: src.Why}), nil
	}
	q := syncOf(c)
	q.from = cmp.Or(c.Param("from"), q.from, src.Default)
	v := envui.SyncView{
		Pick:       urlOf(cd.s) + "?drawer=" + f.Node + "&tab=sync",
		From:       q.from,
		CanvasHref: urlOf(cd.s) + "?drawer=" + f.Node + "&tab=sync&" + q.query(),
	}
	for _, s := range src.Envs {
		v.Sources = append(v.Sources, envui.SyncSource{Slug: s.Slug, Name: s.Name, Selected: s.Slug == q.from})
	}
	// a deploy lands here with ?job=: the page has left the review
	if id := c.QueryParam("job"); id != "" {
		// only a job that locked this env: a job id from elsewhere shows the review
		if j, err := h.orch.GetJob(ctx, id); err == nil && slices.Contains(j.LockSet, "env:"+e.ID) {
			jv := render.JobView(urlOf(cd.s), j)
			jv.Refresh = f.Base + "?tab=sync"
			v.Job = &jv
			return envui.Sync(v), nil
		}
	}
	pl, err := h.orch.PlanEnvSync(ctx, e.ID, q.from, q.drop)
	if err != nil {
		return nil, err
	}
	p := pl.Plan
	v.Blockers, v.Warnings = p.Blockers, p.Warnings
	// Drop and Undo are the env page with this tile's switch flipped.
	flip := func(slug string) string {
		n := syncQ{from: q.from}
		for _, t := range p.Tiles {
			if t.Dropped != (t.Slug == slug) {
				n.drop = append(n.drop, t.Slug)
			}
		}
		return urlOf(cd.s) + "?drawer=" + f.Node + "&tab=sync&" + n.query()
	}
	idx := map[string]int{}
	var keep []string
	for _, t := range p.Tiles {
		idx[t.Slug] = len(v.Tiles)
		v.Tiles = append(v.Tiles, envui.SyncTileView{
			Slug:    t.Slug,
			Kind:    t.Kind,
			Tag:     t.Tag,
			Dropped: t.Dropped,
			Drop:    flip(t.Slug),
			Undo:    flip(t.Slug),
		})
		if !t.Dropped {
			keep = append(keep, t.Slug)
		}
	}
	for _, ch := range p.Changes {
		cv := comp.ChangeView{Kind: ch.Kind, Tile: ch.Tile, Field: ch.Field, Old: ch.Old, New: ch.New, Note: ch.Note}
		if i, ok := idx[ch.Tile]; ok {
			v.Tiles[i].Changes = append(v.Tiles[i].Changes, cv)
		} else {
			v.Other = append(v.Other, cv)
		}
	}
	if pl.CanDeploy && can(c, cd.s, "env.write") {
		v.Deploy = syncConfirm(f.Base, e.Name, q.from, keep, p.Sig, false)
	}
	return envui.Sync(v), nil
}
