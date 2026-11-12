package env

import (
	"context"
	"fmt"
	"sync"
	"time"


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
		r.CanRebuild = v.Role == "replica" && (v.State == "needs rebuild" || v.State == "fenced, needs rewind" || v.Stuck)
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

// overviewMemo shares one overview read per tile among its open drawers: a
// read is a docker exec (SHOW POOLS), a container list, TileStatus and about
// eight store reads, and every viewer's stream asked for it every LiveEvery.
// ponytail: one entry per tile (and per user when the viewer can add
// standbys), reused for LiveEvery and dropped after a minute unused; one
// process only, no per-region staleness.
type overviewMemo struct {
	mu sync.Mutex
	m  map[string]*overviewEntry
}

type overviewEntry struct {
	mu   sync.Mutex // held while the read runs, so a second viewer waits for it
	at   time.Time
	used time.Time
	o    instance.OverviewView
}

// overviewOf is the tile's overview, read at most once per LiveEvery however
// many viewers watch it. A viewer that has gone does not start a read; one
// already running finishes for the others (detached from the viewer's ctx).
// o is shared: streams only read it.
func (h *handler) overviewOf(ctx context.Context, s service.Scope, by *service.Principal, canAdd bool, envURL string) (instance.OverviewView, error) {
	key := s.Tile.ID
	if canAdd { // the standby candidates depend on who asks
		key += "|" + by.User.ID
	}
	m := &h.memo
	m.mu.Lock()
	if m.m == nil {
		m.m = map[string]*overviewEntry{}
	}
	e := m.m[key]
	if e == nil {
		e = &overviewEntry{used: time.Now()}
		m.m[key] = e
		for k, x := range m.m {
			if k != key && x.mu.TryLock() {
				if time.Since(x.used) > time.Minute {
					delete(m.m, k)
				}
				x.mu.Unlock()
			}
		}
	}
	m.mu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return instance.OverviewView{}, err
	}
	e.used = time.Now()
	if !e.at.IsZero() && time.Since(e.at) < LiveEvery {
		return e.o, nil
	}
	rctx := context.WithoutCancel(ctx)
	o := h.pgLive(rctx, by, canAdd, s.Tile.ID, instance.OverviewView{})
	o = h.pgStatic(rctx, s, s.Tile.ID, o)
	if st, err := h.orch.TileStatus(rctx, s.Tile.ID); err != nil {
		o.Unread["job"] = unreadReason(err)
	} else {
		o.Word = st.Word
		if st.LastJob != nil {
			j := render.JobView(envURL, *st.LastJob)
			o.Job, o.JobWhen = &j, st.LastJob.CreatedAt
		}
	}
	e.o, e.at = o, time.Now()
	return o, nil
}
