// Package waiting is the Waiting on you page: what needs the signed-in
// user's action, kept current by its own poll, and the promote of the
// releases on it (one, or many ticked).
package waiting

import (
	"net/http"
	"strconv"
	"strings"

	hamrctx "github.com/FyrmForge/hamr/pkg/ctx"
	hamrmw "github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/ui/components"
	"github.com/FyrmForge/stackr/internal/web/render"
)

type handler struct{ orch *service.Orchestrator }

func NewHandler(orch *service.Orchestrator) *handler { return &handler{orch: orch} }

// GET /-/waiting
func (h *handler) Page(c echo.Context) error { return h.page(c, "", nil) }

// POST /-/waiting/promote: pair=<release>:<env>, one or many. Each pair is
// planned again and promoted on its own (the service checks env.write and
// refuses a plan that needs the full review); the answer is the page's rows.
func (h *handler) Promote(c echo.Context) error {
	form, _ := c.FormParams()
	var pairs []service.PromotePair
	for _, v := range form["pair"] {
		if rel, env, ok := strings.Cut(v, ":"); ok && rel != "" && env != "" {
			pairs = append(pairs, service.PromotePair{ReleaseID: rel, EnvID: env})
		}
	}
	if len(pairs) == 0 {
		return middleware.HTTPError(echo.NewHTTPError(http.StatusBadRequest, "Pick a release to promote."))
	}
	outs := h.orch.PromoteClean(c.Request().Context(), middleware.Principal(c), pairs)
	notice, status := summarize(outs)
	h.orch.ForgetWaitingCounts()
	kind := hamrmw.FlashInfo
	if len(status) > 0 {
		kind = hamrmw.FlashError
	}
	hamrctx.Set(c, hamrctx.FlashKey, any(&hamrmw.FlashMessage{Message: notice, Type: kind})) // shown in this answer, not the next page
	return h.page(c, notice, status)
}

// summarize is the notice ("1 of 3 skipped: needs full review", "2 failed")
// and each refused or failed pair's words for its row.
func summarize(outs []service.PromoteOutcome) (string, map[string]string) {
	status := map[string]string{}
	var reasons []string
	skipped, failed, queued := 0, 0, 0
	for _, o := range outs {
		key := render.PairOf(o.Pair.ReleaseID, o.Pair.EnvID)
		switch {
		case o.Err != nil:
			failed++
			msg, ok := render.Refused(o.Err)
			if !ok {
				msg = "could not queue the promote"
			}
			status[key] = "Not promoted: " + msg
		case o.Skip != "":
			skipped++
			status[key] = "Skipped: " + o.Skip
			if !contains(reasons, o.Skip) {
				reasons = append(reasons, o.Skip)
			}
		default:
			queued++
		}
	}
	var parts []string
	if skipped > 0 {
		parts = append(parts, strconv.Itoa(skipped)+" of "+strconv.Itoa(len(outs))+" skipped: "+strings.Join(reasons, " and "))
	}
	if failed > 0 {
		parts = append(parts, strconv.Itoa(failed)+" failed")
	}
	if len(parts) > 0 {
		return strings.Join(parts, ", "), status
	}
	if queued == 1 {
		return "Promoting 1 release.", status
	}
	return "Promoting " + strconv.Itoa(queued) + " releases.", status
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// page draws the Waiting page; status is a pair's refusal from the promote
// just made, notice its summary.
func (h *handler) page(c echo.Context, notice string, status map[string]string) error {
	w, err := h.orch.WaitingOnYou(c.Request().Context(), middleware.Principal(c))
	if err != nil {
		return middleware.HTTPError(err)
	}
	render.WaitingCountIs(c, w.Count())
	v := components.WaitingView{Count: w.Count(), Live: "/-/waiting", Promote: render.WaitingPromote, Notice: notice}
	var orgs []string
	for _, g := range w.Groups {
		for _, it := range g.Items {
			if it.Rel != nil {
				orgs = append(orgs, it.Rel.OrgID)
			}
		}
	}
	var who map[string]string // member names, for a release a person cut
	if len(orgs) > 0 {
		who, _ = h.orch.MemberNames(c.Request().Context(), orgs...)
	}
	row := func(it service.WaitItem) components.WaitRowView {
		r := components.WaitRowView{Title: it.Title, Sub: it.Sub, Href: it.Href, Act: it.Act}
		if it.Rel != nil {
			r.Rel = render.WaitReleaseOf(it, who, status[render.PairOf(it.Rel.ID, it.Rel.EnvID)])
		}
		return r
	}
	for _, g := range w.Groups {
		gv := components.WaitGroupView{Title: g.Title}
		for _, it := range g.Items {
			gv.Rows = append(gv.Rows, row(it))
		}
		v.Groups = append(v.Groups, gv)
	}
	for _, it := range w.Others {
		v.Others = append(v.Others, row(it))
	}
	return render.Page(c, http.StatusOK, "Waiting on you", components.Waiting(v))
}
