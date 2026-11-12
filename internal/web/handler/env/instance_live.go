package env

import (
	"context"
	"time"

	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/api/stream"
	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/ui/drawer/instance"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// GET …/-/instances/:tile/live[?tab=slices]: the open drawer's one stream
// (the drawer frame holds the connection). One event per region: every region
// once at connect, then only when its markup changes. The header's is
// "actions" (the buttons), plus "word" when the status word changes, which
// draws the drawer again for its badge. With tab=slices (Overview) the
// overview's regions ride along: postgres reads them all (shared among
// viewers), another engine only its endpoint, slices and last job.
func (h *handler) InstanceLive(c echo.Context) error {
	s, by := scope(c), middleware.Principal(c)
	canAdd := can(c, s, "placement.write")
	envURL := render.EnvURL(c)
	v := instance.View{Base: envURL + "/-/instances/" + s.Tile.Slug, ReadOnly: !can(c, s, "tile.write")}
	overview := instance.Tab(c.QueryParam("tab")) == instance.Tabs[0]
	m, _, err := h.orch.InstanceSlices(c.Request().Context(), s.Tile.ID)
	if err != nil {
		return middleware.HTTPError(err)
	}
	pg := m.Engine == "postgres"
	regions := []struct {
		name string
		part func(instance.View, instance.OverviewView) templ.Component
	}{
		{"overview", instance.LiveOverview}, {"slices", instance.LiveSlices},
		{"pooler", instance.LivePooler}, {"members", instance.LiveMembers},
		{"failover", instance.LiveFailover}, {"sync", instance.LiveSync},
		{"job", instance.LiveJob},
	}
	sent := map[string]stream.HTML{}
	fails := map[string]int{}
	var last time.Time
	var word string
	return stream.Watch(c, func(ctx context.Context) ([]stream.Msg, error) {
		if time.Since(last) < LiveEvery {
			return nil, nil
		}
		last = time.Now()
		var o instance.OverviewView
		if overview && pg {
			if o, err = h.overviewOf(ctx, s, by, canAdd, envURL); err != nil {
				return nil, err // the viewer left before a read began
			}
		} else {
			o = h.headOf(ctx, c, overview)
		}
		var out []stream.Msg
		send := func(name string, part templ.Component) error {
			body, err := render.Event(ctx, part)
			if err != nil {
				return err
			}
			if b, ok := sent[name]; !ok || b != body {
				sent[name] = body
				out = append(out, stream.Msg{Name: name, Body: body})
			}
			return nil
		}
		if _, bad := o.Unread["job"]; o.Word != "" && !bad {
			v.Status, v.Running = o.Word, o.Word == "running"
			if word != "" && o.Word != word {
				out = append(out, stream.Msg{Name: "word", Body: stream.HTML(o.Word)})
			}
			word = o.Word
			if err := send("actions", instance.LiveActions(v)); err != nil {
				return nil, err
			}
		}
		if !overview {
			return out, nil
		}
		for _, r := range regions {
			// a failed read keeps the last good markup until it has
			// failed unreadAfter times running
			if _, bad := o.Unread[r.name]; bad {
				if fails[r.name]++; fails[r.name] < unreadAfter {
					continue
				}
			} else {
				fails[r.name] = 0
			}
			if err := send(r.name, r.part(v, o)); err != nil {
				return nil, err
			}
		}
		return out, nil
	})
}

// headOf is what a stream needs without the postgres overview: the status
// word and last job, and with overview the endpoint and slices.
func (h *handler) headOf(ctx context.Context, c echo.Context, overview bool) instance.OverviewView {
	s := scope(c)
	o := instance.OverviewView{Unread: map[string]string{}}
	if st, err := h.orch.TileStatus(ctx, s.Tile.ID); err != nil {
		o.Unread["job"] = unreadReason(err)
	} else {
		o.Word = st.Word
		if st.LastJob != nil {
			j := render.JobView(render.EnvURL(c), *st.LastJob)
			o.Job, o.JobWhen = &j, st.LastJob.CreatedAt
		}
	}
	if overview {
		if m, ps, err := h.orch.InstanceSlices(ctx, s.Tile.ID); err != nil {
			o.Unread["overview"], o.Unread["slices"] = unreadReason(err), unreadReason(err)
		} else {
			o.Endpoint, o.AdminUser, o.Slices = m.Endpoint, m.AdminUser, sliceRows(s, ps)
		}
	}
	return o
}
