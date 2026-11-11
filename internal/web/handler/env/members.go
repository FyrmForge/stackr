package env

import (
	"context"
	"fmt"
	"time"

	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/api/stream"
	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/ui/drawer/instance"
	"github.com/FyrmForge/stackr/internal/ui/drawer/slice"
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
		r.CanRemove = v.Role == "replica" && v.State != "removing"
		r.CanRebuild = v.Role == "replica" && (v.State == "needs rebuild" || v.State == "fenced, needs rewind")
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
func (h *handler) pgLive(ctx context.Context, by *service.Principal, canAdd bool, tileID string, o instance.OverviewView) instance.OverviewView {
	o.Unread = map[string]string{}
	fail := func(region string, err error) {
		o.Unread[region] = unreadReason(err)
	}
	if ps, err := h.orch.Pooler(ctx, tileID); err != nil {
		fail("pooler", err)
	} else {
		o.Pooler = &instance.PoolerView{
			Status: ps.Word, Host: ps.Host, Port: ps.Port,
			Active: ps.Active, Waiting: ps.Waiting, Counted: ps.Counted,
		}
	}
	vs, err := h.orch.Standbys(ctx, tileID)
	if err != nil {
		fail("members", err)
	}
	if canAdd {
		var cerr error
		if o.AddNodes, cerr = h.orch.StandbyCandidates(ctx, by, tileID); cerr != nil && err == nil {
			fail("members", cerr)
		}
	}
	if len(vs) > 1 || len(o.AddNodes) > 0 {
		o.Members = memberRows(vs)
	}
	if sv, err := h.orch.SyncOf(ctx, tileID); err != nil {
		fail("sync", err)
	} else if len(sv.Standbys) > 0 || sv.Prefer {
		o.Sync = &instance.SyncView{Prefer: sv.Prefer, Required: sv.Required, LimitMS: sv.LimitMS, Status: sv.Status}
		for _, b := range sv.Standbys {
			o.Sync.Standbys = append(o.Sync.Standbys, instance.SyncStandby{ID: b.ID, Node: b.Node, Mode: b.Mode})
		}
	}
	if fo, ok, err := h.orch.FailoverOptionsOf(ctx, tileID); err != nil {
		fail("failover", err)
	} else if ok {
		o.Failover = &instance.FailoverView{Primary: fo.Primary, Switch: fo.Switch, Promote: fo.Promote,
			Unreachable: fo.Unreachable, InFlight: fo.InFlight, Error: fo.Error, CanAbort: fo.CanAbort}
	}
	return o
}

// pgStatic fills o with the overview's endpoint, protection, image and
// slices: read again by the stream, since an update, a protection change or a
// slice cut elsewhere moves them. A failed read marks its region unread.
func (h *handler) pgStatic(ctx context.Context, s service.Scope, tileID string, o instance.OverviewView) instance.OverviewView {
	if o.Unread == nil {
		o.Unread = map[string]string{}
	}
	m, ps, err := h.orch.InstanceSlices(ctx, tileID)
	if err != nil {
		o.Unread["overview"], o.Unread["slices"] = unreadReason(err), unreadReason(err)
		return o
	}
	o.Endpoint, o.AdminUser, o.Slices = m.Endpoint, m.AdminUser, sliceRows(s, ps)
	pv, err := h.orch.PGVersion(ctx, tileID)
	if err != nil {
		o.Unread["overview"] = unreadReason(err)
		return o
	}
	o.Version = &instance.VersionView{Image: pv.Image, UpdateTo: pv.UpdateTo, EOL: pv.EOL, EOLDate: pv.EOLDate, Majors: pv.Majors}
	if ck := pv.Check; ck != nil {
		o.Version.Check = fmt.Sprintf("Last check for postgres %d: %s", ck.To, ck.Detail)
	}
	if rb := pv.Rollback; rb != nil {
		o.Version.RollbackFrom, o.Version.RollbackUntil = rb.From, rb.Until.Format("2006-01-02 15:04")
	}
	if o.Protection, err = h.orch.Protection(ctx, tileID); err != nil {
		o.Unread["overview"] = unreadReason(err)
	}
	return o
}

// sliceRows are the slices cut from an instance, as the overview lists them.
func sliceRows(s service.Scope, ps []service.InstanceSlice) []instance.SliceRow {
	var out []instance.SliceRow
	for _, p := range ps {
		r := instance.SliceRow{
			Slice:    p.Slice,
			Link:     drawerURL(s.Org.Slug, p.Stack, p.Env, p.TileID, slice.Tabs[0]),
			Name:     p.DBName,
			Bindings: p.Bindings,
			Public:   p.Public,
		}
		if p.Stack != s.Stack.Slug || p.Env != s.Env.Slug {
			r.Where = p.Stack + "/" + p.Env
		}
		out = append(out, r)
	}
	return out
}

// unreadReason is a failed read as the drawer words it: the refusal's
// message when the user can act on it, else a plain retry line.
func unreadReason(err error) string {
	if msg, ok := render.Refused(err); ok {
		return msg
	}
	return "try again shortly"
}

// unreadAfter is how many reads in a row must fail before the stream
// replaces a region's last good markup with the error line.
const unreadAfter = 3

// GET …/-/instances/:tile/live: the open overview's stream, the same
// polling stream as the canvas's. One event per region (pooler, members,
// failover, sync, job, overview, slices): every region once at connect, then only when its markup
// changes, so a form is not redrawn under the hands for nothing.
func (h *handler) InstanceLive(c echo.Context) error {
	s, by := scope(c), middleware.Principal(c)
	canAdd := can(c, s, "placement.write")
	v := instance.View{Base: render.EnvURL(c) + "/-/instances/" + s.Tile.Slug, ReadOnly: !can(c, s, "tile.write")}
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
	return stream.Watch(c, func(ctx context.Context) ([]stream.Msg, error) {
		if time.Since(last) < LiveEvery {
			return nil, nil
		}
		last = time.Now()
		o := h.pgLive(ctx, by, canAdd, s.Tile.ID, instance.OverviewView{})
		o = h.pgStatic(ctx, s, s.Tile.ID, o)
		if st, err := h.orch.TileStatus(ctx, s.Tile.ID); err != nil {
			o.Unread["job"] = unreadReason(err)
		} else if st.LastJob != nil {
			j := render.JobView(render.EnvURL(c), *st.LastJob)
			o.Job, o.JobWhen = &j, st.LastJob.CreatedAt
		}
		var out []stream.Msg
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
