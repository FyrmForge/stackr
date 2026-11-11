package env

import (
	"context"
	"fmt"
	"time"

	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/api/stream"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/ui/drawer/instance"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// memberRows are an instance's members as the drawer lists them.
func memberRows(vs []service.StandbyView) []instance.MemberRow {
	var out []instance.MemberRow
	for _, v := range vs {
		r := instance.MemberRow{ID: v.ID, Fenced: v.Fenced, Node: v.Node, Role: v.Role, State: v.State, Tone: "warn"}
		switch v.State {
		case "running", "streaming":
			r.Tone = "success"
		case "needs rebuild", "fenced, needs rewind":
			r.Tone = "danger"
		}
		r.CanFence = v.Role == "replica" && v.State != "removing" && v.State != "fenced, needs rewind" && v.State != "needs rebuild"
		if v.Role == "replica" && v.State == "streaming" {
			r.Lag = fmt.Sprintf("%s behind, %s", render.Size(v.LagBytes), (time.Duration(v.LagSeconds) * time.Second).String())
		}
		if v.Role == "replica" && v.SlotBytes > 0 {
			r.Slot = "slot holds " + render.Size(v.SlotBytes)
			if v.SlotCapBytes > 0 {
				r.Slot += " of " + render.Size(v.SlotCapBytes)
			}
		}
		out = append(out, r)
	}
	return out
}

// LiveEvery is how often an open drawer's stream reads the instance again
// (the canvas stream's pace: a read asks Docker).
var LiveEvery = 3 * time.Second

// pgLive fills o with the parts of a postgres overview that change by
// themselves: the pooler, the members, the failover offer and the sync
// status. The drawer renders it once and its stream renders it again.
func (h *handler) pgLive(ctx context.Context, tileID string, o instance.OverviewView) instance.OverviewView {
	if ps, err := h.orch.Pooler(ctx, tileID); err == nil {
		o.Pooler = &instance.PoolerView{
			Status: ps.Word, Host: ps.Host, Port: ps.Port,
			Active: ps.Active, Waiting: ps.Waiting, Counted: ps.Counted,
		}
	}
	if vs, err := h.orch.Standbys(ctx, tileID); err == nil && len(vs) > 1 {
		o.Members = memberRows(vs)
	}
	if sv, err := h.orch.SyncOf(ctx, tileID); err == nil && (len(sv.Standbys) > 0 || sv.Prefer) {
		o.Sync = &instance.SyncView{Prefer: sv.Prefer, Required: sv.Required, LimitMS: sv.LimitMS, Status: sv.Status}
		for _, b := range sv.Standbys {
			o.Sync.Standbys = append(o.Sync.Standbys, instance.SyncStandby{ID: b.ID, Node: b.Node, Mode: b.Mode})
		}
	}
	if fo, ok, err := h.orch.FailoverOptionsOf(ctx, tileID); err == nil && ok {
		o.Failover = &instance.FailoverView{Primary: fo.Primary, Switch: fo.Switch, Promote: fo.Promote,
			Unreachable: fo.Unreachable, InFlight: fo.InFlight, Error: fo.Error, CanAbort: fo.CanAbort}
	}
	return o
}

// GET …/-/instances/:tile/live: the open overview's stream, the same
// polling stream as the canvas's. One event per region (pooler, members,
// failover, sync): every region once at connect, then only when its markup
// changes, so a form is not redrawn under the hands for nothing.
func (h *handler) InstanceLive(c echo.Context) error {
	s := scope(c)
	v := instance.View{Base: render.EnvURL(c) + "/-/instances/" + s.Tile.Slug, ReadOnly: !can(c, s, "tile.write")}
	regions := []struct {
		name string
		part func(instance.View, instance.OverviewView) templ.Component
	}{
		{"pooler", instance.LivePooler}, {"members", instance.LiveMembers},
		{"failover", instance.LiveFailover}, {"sync", instance.LiveSync},
	}
	sent := map[string]stream.HTML{}
	var last time.Time
	return stream.Watch(c, func(ctx context.Context) ([]stream.Msg, error) {
		if time.Since(last) < LiveEvery {
			return nil, nil
		}
		last = time.Now()
		o := h.pgLive(ctx, s.Tile.ID, instance.OverviewView{})
		var out []stream.Msg
		for _, r := range regions {
			body, err := render.Event(ctx, r.part(v, o))
			if err != nil {
				return nil, err
			}
			if b, ok := sent[r.name]; !ok || b != body {
				sent[r.name] = body
				out = append(out, stream.Msg{Name: r.name, Body: body})
			}
		}
		return out, nil
	})
}
