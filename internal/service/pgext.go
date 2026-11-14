package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/deploy"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/credential"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/opmember"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Postgres extensions on operated instances: an instance lists the extensions
// its slices may use and the startup modules it loads; a slice lists the ones
// its database holds. A change of image or startup modules is a kindPGExt job
// with a persisted cursor (pgext.<instance id>), so a panel restart resumes
// it, and a server that does not start gets the old values back.

const (
	kindPGExt jobs.Kind = "pg-extensions"

	// pgextKey is an instance's extensions change in flight: pgext.<instance
	// id>. A cursor in the "due" phase only says the values are stored and a
	// reconcile is owed at the next start; it does not block new requests.
	pgextKey = "pgext."
)

// The phases of a change's cursor, in order.
const (
	pgExtQueued  = "queued"
	pgExtCheck   = "check"
	pgExtStore   = "store"
	pgExtStart   = "start"
	pgExtPutBack = "putback"
	pgExtDue     = "due"
)

// PGExtIn is a request to change an instance's image, startup modules or
// extensions; nil leaves a field as it is.
type PGExtIn struct {
	Image               *string
	Preload, Extensions *[]string
}

// PGExtView is an instance's extensions as the drawer, API and CLI show them.
// Items is empty when the instance is stopped; Plan is the message 14 text;
// Busy is the phase of the change in flight, "" for none.
type PGExtView struct {
	Image               string
	Official, Running   bool
	Preload, Extensions []string
	Items               []PGExtItem
	Slices              []string
	Plan                string
	Busy                string
}

// PGExtItem is one extension of an instance: installed and available
// version, whether its list names it, whether it is a startup module, and the
// slices that use it.
type PGExtItem struct {
	Name, Installed, Available string
	Listed, Startup            bool
	UsedBy                     []string
}

// SliceExtView is a slice's extensions: the instance it sits on, whether it
// is provisioned, its list, what the instance allows (the instance's list),
// and each name's state.
type SliceExtView struct {
	Instance            string
	Provisioned         bool
	Extensions, Allowed []string
	Items               []SliceExtItem
}

// SliceExtItem is one extension of a slice's database; AddedWith names the
// extension that pulled it in (CASCADE).
type SliceExtItem struct {
	Name, Installed, Available, AddedWith string
	Listed                                bool
}

// pgExtVals is the part of an instance a change moves: image and both lists.
type pgExtVals struct {
	Image      string
	Preload    []string
	Extensions []string
}

// pgExtJob is a kindPGExt payload: the instance tile and the values the
// request read (Old, which the job revalidates when it starts) and wants
// (New).
type pgExtJob struct {
	TileID   string
	Old, New pgExtVals
}

// pgExtState is the change's persisted cursor, settings key pgext.<instance
// id>: Phase is one of the pgExt* phases, Due says a reconcile is owed, and
// Resumes counts automatic resumes after a panel restart.
type pgExtState struct {
	Phase    string
	Old, New pgExtVals
	Due      bool
	Resumes  int
}

// ---- cursor ----

func (o *Orchestrator) pgExtOf(ctx context.Context, instanceID string) (pgExtState, bool) {
	var st pgExtState
	v, ok, err := o.store.Settings.Get(ctx, pgextKey+instanceID)
	if err != nil || !ok || json.Unmarshal([]byte(v), &st) != nil {
		return pgExtState{}, false
	}
	return st, true
}

func (o *Orchestrator) setPGExtState(ctx context.Context, instanceID string, st pgExtState) error {
	b, _ := json.Marshal(st)
	return o.store.Settings.Set(ctx, pgextKey+instanceID, string(b))
}

func (o *Orchestrator) delPGExtState(ctx context.Context, instanceID string) {
	if err := o.store.Settings.Delete(ctx, pgextKey+instanceID); err != nil {
		slog.Warn("pg extensions: cursor not deleted", "instance", instanceID, "err", err)
	}
}

// pgExtBusy is the error while a change is in flight (message 29): a cursor in
// any phase but due.
func (o *Orchestrator) pgExtBusy(ctx context.Context, t Tile, m OperatedInstance) error {
	if st, ok := o.pgExtOf(ctx, m.ID); ok && st.Phase != pgExtDue {
		return errs.Conflictf("a change to the extensions of %s is in progress; wait for it to end.", t.Slug)
	}
	return nil
}

func pgExtValsOf(t Tile, m OperatedInstance) pgExtVals {
	return pgExtVals{Image: t.ImageRef, Preload: slices.Clone([]string(m.Preload)), Extensions: slices.Clone([]string(m.Extensions))}
}

func (v pgExtVals) equal(w pgExtVals) bool {
	return v.Image == w.Image && slices.Equal(v.Preload, w.Preload) && slices.Equal(v.Extensions, w.Extensions)
}

// pgExtWrite stores v's image and lists, each as a write of its own columns
// (R3), under the instance's lock: never a whole row from an earlier read.
// The caller does not hold pgLock.
func (o *Orchestrator) pgExtWrite(ctx context.Context, tileID string, v pgExtVals) error {
	m, err := o.operated.GetByTile(ctx, tileID)
	if err != nil {
		return err
	}
	mu := o.pgLock(m.ID)
	mu.Lock()
	defer mu.Unlock()
	return o.pgExtWriteLocked(ctx, tileID, m, v)
}

func (o *Orchestrator) pgExtWriteLocked(ctx context.Context, tileID string, m OperatedInstance, v pgExtVals) error {
	if err := o.tiles.SetImageRef(ctx, tileID, v.Image); err != nil {
		return err
	}
	_, err := o.operated.SetExtensions(ctx, m, v.Preload, v.Extensions)
	return err
}

// ---- views ----

// slicesOn are the slice tiles whose database is, or would be, on instance t.
func (o *Orchestrator) slicesOn(ctx context.Context, t Tile, m OperatedInstance) ([]Tile, error) {
	ss, err := o.tiles.ListByKind(ctx, tile.Slice)
	if err != nil {
		return nil, err
	}
	var out []Tile
	for _, s := range ss {
		p, cut, err := o.operated.ProvisionOf(ctx, s.ID)
		if err != nil {
			return nil, err
		}
		if cut {
			if p.InstanceID == m.ID {
				out = append(out, s)
			}
			continue
		}
		it, err := o.deploy.Target(ctx, s)
		_, conflict := errs.IsConflict(err)
		_, unset := errs.IsUnset(err)
		_, invalid := errs.IsInvalid(err)
		switch {
		case conflict || unset || invalid || errors.Is(err, errs.ErrNotFound):
		case err != nil:
			return nil, err
		case it.ID == t.ID:
			out = append(out, s)
		}
	}
	return out, nil
}

// PGExtensions is instance tileID's extensions view.
func (o *Orchestrator) PGExtensions(ctx context.Context, tileID string) (PGExtView, error) {
	t, m, err := o.instanceOf(ctx, tileID)
	if err != nil {
		return PGExtView{}, err
	}
	if m.Engine != "postgres" {
		return PGExtView{}, errs.Invalidf("engine", "%s has no extensions; only postgres does", m.Engine)
	}
	v := PGExtView{
		Image:      t.ImageRef,
		Official:   mflow.PGOfficial(t.ImageRef),
		Running:    o.tileRunning(ctx, t),
		Preload:    slices.Clone([]string(m.Preload)),
		Extensions: slices.Clone([]string(m.Extensions)),
	}
	if st, ok := o.pgExtOf(ctx, m.ID); ok && st.Phase != pgExtDue {
		v.Busy = st.Phase
	}
	ss, err := o.slicesOn(ctx, t, m)
	if err != nil {
		return v, err
	}
	for _, s := range ss {
		v.Slices = append(v.Slices, s.Slug)
	}
	slices.Sort(v.Slices)
	list := "none"
	if len(v.Slices) > 0 {
		list = strings.Join(v.Slices, ", ")
	}
	v.Plan = fmt.Sprintf("%s restarts. A startup module runs in every database; a crash in it restarts every slice. Slices on it: %s.", t.Slug, list)
	if !v.Running {
		return v, nil
	}
	infos, err := o.engines.Extensions(ctx, t)
	if err != nil {
		return v, err
	}
	for _, e := range infos {
		it := PGExtItem{Name: e.Name, Installed: e.Installed, Available: e.Available, Listed: e.Listed, Startup: e.Startup}
		for _, s := range ss {
			if slices.Contains([]string(s.Extensions), e.Name) {
				it.UsedBy = append(it.UsedBy, s.Slug)
			}
		}
		v.Items = append(v.Items, it)
	}
	return v, nil
}

// SliceExtensions is slice sliceID's extensions view.
func (o *Orchestrator) SliceExtensions(ctx context.Context, sliceID string) (SliceExtView, error) {
	s, err := o.sliceTile(ctx, sliceID)
	if err != nil {
		return SliceExtView{}, err
	}
	v := SliceExtView{Extensions: slices.Clone([]string(s.Extensions))}
	var it Tile
	var m OperatedInstance
	p, ok, err := o.operated.ProvisionOf(ctx, s.ID)
	switch {
	case err != nil:
		return v, err
	case ok:
		v.Provisioned = true
		if m, err = o.operated.Get(ctx, p.InstanceID); err != nil {
			return v, err
		}
		if it, err = o.tiles.Get(ctx, m.TileID); err != nil {
			return v, err
		}
	default:
		if it, err = o.deploy.Target(ctx, s); err == nil {
			m, err = o.operated.GetByTile(ctx, it.ID)
		}
	}
	if err != nil || m.ID == "" {
		return v, nil // no instance to name: the deploy says why
	}
	v.Instance = it.Slug
	v.Allowed = slices.Clone([]string(m.Extensions))
	if !v.Provisioned || m.Engine != "postgres" {
		for _, n := range v.Extensions {
			v.Items = append(v.Items, SliceExtItem{Name: n, Listed: true})
		}
		return v, nil
	}
	infos, err := o.engines.SliceExtensions(ctx, s)
	if err != nil {
		return v, err
	}
	for _, e := range infos {
		v.Items = append(v.Items, SliceExtItem{Name: e.Name, Installed: e.Installed, Available: e.Available, AddedWith: e.AddedWith, Listed: e.Listed})
	}
	return v, nil
}

// SetSliceExtensions sets a slice's list; a provisioned slice queues its own
// deploy and returns that job (R18). Taking a name off the list leaves the
// extension in place.
func (o *Orchestrator) SetSliceExtensions(ctx context.Context, sliceID string, names []string) (Tile, *Job, error) {
	s, err := o.sliceTile(ctx, sliceID)
	if err != nil {
		return s, nil, err
	}
	st, err := o.stacks.Get(ctx, s.StackID)
	if err != nil {
		return s, nil, err
	}
	if st.ConfigRepo != "" {
		return s, nil, errs.Conflictf("stack %s is config-managed; set extensions in its stack file", st.Slug)
	}
	if err := deploy.CheckExtNames(names); err != nil {
		return s, nil, err
	}
	p, provisioned, err := o.operated.ProvisionOf(ctx, s.ID)
	if err != nil {
		return s, nil, err
	}
	if provisioned {
		m, err := o.operated.Get(ctx, p.InstanceID)
		if err != nil {
			return s, nil, err
		}
		if m.Engine != "postgres" {
			return s, nil, errs.Invalidf("extensions", "slice %s: extensions apply to postgres slices only", s.Slug)
		}
		if miss := deploy.ExtMissing(m.Extensions, names); len(miss) > 0 {
			it, err := o.tiles.Get(ctx, m.TileID)
			if err != nil {
				return s, nil, err
			}
			return s, nil, errs.Conflictf("slice %s: %s is not enabled on %s. Add it to the instance's extensions first.", s.Slug, miss[0], it.Slug)
		}
	}
	cur := s
	cur.Extensions = names
	out, _, err := o.tiles.Update(ctx, s, cur)
	if err != nil || !provisioned {
		return out, nil, err
	}
	j, err := o.Deploy(ctx, s.ID)
	if err != nil {
		return out, nil, err
	}
	return out, &j, nil
}

// ---- the request ----

// SetPGExtensions changes an instance's image, startup modules or
// extensions; the job is nil when nothing needs one.
func (o *Orchestrator) SetPGExtensions(ctx context.Context, by *Principal, tileID string, in PGExtIn) (PGExtView, *Job, error) {
	t, m, err := o.instanceOf(ctx, tileID)
	if err != nil {
		return PGExtView{}, nil, err
	}
	st, err := o.stacks.Get(ctx, t.StackID)
	if err != nil {
		return PGExtView{}, nil, err
	}
	if err := mayWrite(by, st); err != nil {
		return PGExtView{}, nil, err
	}
	_, j, err := o.setPGExt(ctx, t, m, in)
	if err != nil {
		return PGExtView{}, nil, err
	}
	v, err := o.PGExtensions(ctx, tileID)
	return v, j, err
}

// setPGExt is the one entry SetPGExtensions and UpdateTile share. An official
// image to an official image and nothing else is today's minor update and
// goes through UpdateTile, unchanged (R6). Anything else takes the instance's
// lock, reads the row again under it and runs the checks and the store
// together (V1), so two requests cannot both pass.
func (o *Orchestrator) setPGExt(ctx context.Context, t Tile, m OperatedInstance, in PGExtIn) (Tile, *Job, error) {
	if m.Engine != "postgres" {
		return t, nil, errs.Invalidf("engine", "%s has no extensions; only postgres does", m.Engine)
	}
	if in.Image != nil && in.Preload == nil && in.Extensions == nil &&
		*in.Image != t.ImageRef && mflow.PGOfficial(t.ImageRef) && mflow.PGOfficial(*in.Image) {
		img := *in.Image
		return o.UpdateTile(ctx, t.ID, func(x *Tile) error { x.ImageRef = img; return nil })
	}
	mu := o.pgLock(m.ID)
	mu.Lock()
	defer mu.Unlock()
	var err error
	if t, m, err = o.instanceOf(ctx, t.ID); err != nil {
		return t, nil, err
	}
	return o.setPGExtLocked(ctx, t, m, in)
}

// pgExtChange is a checked change: what it moves from and to, and whether the
// instance runs.
type pgExtChange struct {
	t        Tile
	m        OperatedInstance
	old, new pgExtVals
	running  bool
}

// extsOnly: the change moves the extensions list alone, so no restart.
func (c pgExtChange) extsOnly() bool {
	return c.old.Image == c.new.Image && slices.Equal(c.old.Preload, c.new.Preload)
}

// pgExtPrepare is steps 1, 2 and 4 to 9 of setPGExt: every refusal, nothing
// written. The caller holds pgLock(instance) and passes rows it read under
// it. changed is false when the request moves nothing.
func (o *Orchestrator) pgExtPrepare(ctx context.Context, t Tile, m OperatedInstance, in PGExtIn) (c pgExtChange, changed bool, err error) {
	if m.Engine != "postgres" {
		return c, false, errs.Invalidf("engine", "%s has no extensions; only postgres does", m.Engine)
	}
	if in.Extensions != nil {
		if err = deploy.CheckExtNames(*in.Extensions); err != nil {
			return
		}
	}
	if in.Preload != nil {
		if err = deploy.CheckPreload(*in.Preload); err != nil {
			return
		}
	}
	c = pgExtChange{t: t, m: m, old: pgExtValsOf(t, m)}
	c.new = pgExtValsOf(t, m)
	if in.Image != nil {
		c.new.Image = *in.Image
	}
	if in.Preload != nil {
		c.new.Preload = slices.Clone(*in.Preload)
	}
	if in.Extensions != nil {
		c.new.Extensions = slices.Clone(*in.Extensions)
	}
	if c.old.equal(c.new) {
		return c, false, nil
	}
	if err = o.pgExtBusy(ctx, t, m); err != nil { // R4
		return
	}
	minor := c.old.Image != c.new.Image && mflow.PGOfficial(c.old.Image) && mflow.PGOfficial(c.new.Image) &&
		slices.Equal(c.old.Preload, c.new.Preload) && slices.Equal(c.old.Extensions, c.new.Extensions)
	if !c.extsOnly() && !minor { // R5: a stock minor update stays as it is today
		if st, ok := o.pgupOf(ctx, m.ID); ok && !st.Done {
			return c, false, errs.Conflictf("%s can still be rolled back to postgres %d. Finish or drop the rollback first.", t.Slug, st.From)
		}
		if rb := o.pgupRollback(ctx, m.ID); rb != nil {
			return c, false, errs.Conflictf("%s can still be rolled back to postgres %d. Finish or drop the rollback first.", t.Slug, rb.From)
		}
	}
	if gone := removedNames(c.old.Extensions, c.new.Extensions); len(gone) > 0 { // message 5
		ss, err := o.slicesOn(ctx, t, m)
		if err != nil {
			return c, false, err
		}
		for _, n := range gone {
			var by []string
			for _, s := range ss {
				if slices.Contains([]string(s.Extensions), n) {
					by = append(by, s.Slug)
				}
			}
			if len(by) == 1 {
				return c, false, errs.Conflictf("%s is listed by slice %s. Take it off the slice first.", n, by[0])
			}
			if len(by) > 1 {
				return c, false, errs.Conflictf("%s is listed by slice %s and %d more. Take it off the slice first.", n, by[0], len(by)-1)
			}
		}
	}
	if c.old.Image != c.new.Image {
		chk := t
		chk.ImageRef = c.new.Image
		if err = o.engines.CheckImage(ctx, chk); err != nil {
			return
		}
	}
	if !c.extsOnly() && !minor { // message 1
		ms, err := o.members.List(ctx, m.ID)
		if err != nil {
			return c, false, err
		}
		if opmember.HasReplica(ms) &&
			(!mflow.PGOfficial(c.old.Image) || !mflow.PGOfficial(c.new.Image) || !slices.Equal(c.old.Preload, c.new.Preload)) {
			return c, false, errs.Conflictf("%s has a standby. Remove the standby, change the image or the startup modules, then add the standby again.", t.Slug)
		}
	}
	reach, err := o.reachedByRedeploy(ctx, t)
	if err != nil {
		return c, false, err
	}
	c.running = reach
	return c, true, nil
}

func removedNames(old, nw []string) []string {
	var out []string
	for _, n := range old {
		if !slices.Contains(nw, n) {
			out = append(out, n)
		}
	}
	return out
}

// pgExtNotOffered is message 4b for the first name of add the running
// server's image does not offer.
func (o *Orchestrator) pgExtNotOffered(ctx context.Context, c pgExtChange) error {
	var add []string
	for _, n := range c.new.Extensions {
		if !slices.Contains(c.old.Extensions, n) {
			add = append(add, n)
		}
	}
	if len(add) == 0 {
		return nil
	}
	infos, err := o.engines.Extensions(ctx, c.t)
	if err != nil {
		return err
	}
	for _, n := range add {
		if !slices.ContainsFunc(infos, func(e mflow.ExtInfo) bool { return e.Name == n && e.Available != "" }) {
			return errs.Conflictf("the image %s has no extension %s; nothing was changed.", c.t.ImageRef, n)
		}
	}
	return nil
}

// pgExtStoreNow stores a change that needs no job: the list alone, or any
// change of a stopped instance (R8: the first deploy boots it and, for an
// image or startup module change, reconciles, which the cursor in phase due
// says). noCursor: write no cursor (a new instance of a file has no
// provisions to reconcile). The caller holds pgLock.
func (o *Orchestrator) pgExtStoreNow(ctx context.Context, c pgExtChange, noCursor bool) error {
	if err := o.pgExtWriteLocked(ctx, c.t.ID, c.m, c.new); err != nil {
		return err
	}
	if c.extsOnly() || noCursor {
		return nil
	}
	return o.setPGExtState(ctx, c.m.ID, pgExtState{Phase: pgExtDue, Due: true, Old: c.old, New: c.new})
}

// setPGExtLocked is steps 1 to 11 of setPGExt; the caller holds pgLock and
// passes rows read under it.
func (o *Orchestrator) setPGExtLocked(ctx context.Context, t Tile, m OperatedInstance, in PGExtIn) (Tile, *Job, error) {
	if err := o.gateTile(ctx, t.ID, false); err != nil {
		return t, nil, err
	}
	c, changed, err := o.pgExtPrepare(ctx, t, m, in)
	if err != nil || !changed {
		return t, nil, err
	}
	if c.extsOnly() || !c.running {
		if c.running {
			if err := o.pgExtNotOffered(ctx, c); err != nil {
				return t, nil, err
			}
		}
		if err := o.pgExtStoreNow(ctx, c, false); err != nil {
			return t, nil, err
		}
		out, err := o.tiles.Get(ctx, t.ID)
		return out, nil, err
	}
	lock := []string{t.ID}
	if sv, err := o.dataVolume(ctx, t); err == nil { // none yet: the tile alone
		if lock, err = o.pgupLocks(ctx, pgChange{sv: sv}); err != nil {
			return t, nil, err
		}
	}
	if err := o.setPGExtState(ctx, m.ID, pgExtState{Phase: pgExtQueued, Old: c.old, New: c.new}); err != nil {
		return t, nil, err
	}
	j, err := o.enqueue(ctx, kindPGExt, pgExtJob{TileID: t.ID, Old: c.old, New: c.new}, lock...)
	if err != nil {
		o.delPGExtState(ctx, m.ID)
		return t, nil, err
	}
	return t, &j, nil
}

// pgExtFromFile is the stack file's way in (promote's Flow.PGExt): the
// image, preload and extensions a file sets for an instance, nil = not in the
// file. A stopped instance and a list-only change store (R8); a running one
// gets the same change as the panel's, run inline with the calling job's log:
// no job is enqueued (it would wait behind the job running this) and no gate
// is asked of the caller.
func (o *Orchestrator) pgExtFromFile(ctx context.Context, t store.Tile, m store.OperatedInstance, image *string, preload, exts *[]string, log io.Writer) error {
	mu := o.pgLock(m.ID)
	mu.Lock()
	t, m, err := o.instanceOf(ctx, t.ID)
	if err != nil {
		mu.Unlock()
		return err
	}
	c, changed, err := o.pgExtPrepare(ctx, t, m, PGExtIn{Image: image, Preload: preload, Extensions: exts})
	if err != nil || !changed {
		mu.Unlock()
		return err
	}
	if c.extsOnly() || !c.running {
		defer mu.Unlock()
		if c.running {
			if err := o.pgExtNotOffered(ctx, c); err != nil {
				return err
			}
		}
		ps, err := o.operated.ByInstance(ctx, m.ID)
		if err != nil {
			return err
		}
		return o.pgExtStoreNow(ctx, c, len(ps) == 0)
	}
	st := pgExtState{Phase: pgExtQueued, Old: c.old, New: c.new}
	err = o.setPGExtState(ctx, m.ID, st)
	mu.Unlock()
	if err != nil {
		return err
	}
	return o.applyPGExt(ctx, t, m, st, log)
}

// ---- the job ----

// runPGExt is the kindPGExt job handler.
func (o *Orchestrator) runPGExt(ctx context.Context, r *jobs.Run, p pgExtJob) error {
	ctx, err := o.withTileNode(ctx, p.TileID)
	if err != nil {
		return err
	}
	o.ensureMesh(ctx)
	t, m, err := o.instanceOf(ctx, p.TileID)
	if err != nil {
		return err
	}
	st, ok := o.pgExtOf(ctx, m.ID)
	if !ok {
		return errors.New("the change is no longer pending")
	}
	return o.applyPGExt(ctx, t, m, st, r.Log)
}

// applyPGExt is the one body the job and the file hook run: revalidate,
// check the new image boots, store, restart, and put the old values back when
// the server does not start. It does not hold pgLock across any of it.
func (o *Orchestrator) applyPGExt(ctx context.Context, t Tile, m OperatedInstance, st pgExtState, log io.Writer) error {
	save := func(phase string) error {
		st.Phase = phase
		return o.setPGExtState(ctx, m.ID, st)
	}
	fail := func(err error) error { // nothing was stored
		o.delPGExtState(ctx, m.ID)
		return err
	}
	if st.Phase == pgExtPutBack {
		return o.pgExtPutBack(ctx, t, m, st, log, nil)
	}
	if st.Phase == pgExtQueued {
		// R4: what the request read must still be what is stored
		ft, fm, err := o.instanceOf(ctx, t.ID)
		if err != nil {
			return fail(err)
		}
		if !pgExtValsOf(ft, fm).equal(st.Old) {
			return fail(errors.New("changed since it was queued; nothing was changed"))
		}
		t, m = ft, fm
		if err := save(pgExtCheck); err != nil {
			return err
		}
	}
	if st.Phase == pgExtCheck {
		if err := o.pgExtBootCheck(ctx, t, m, st.New, log); err != nil {
			if ctx.Err() != nil {
				return err
			}
			return fail(err)
		}
		st.Due = true
		if err := save(pgExtStore); err != nil {
			return err
		}
	}
	if st.Phase == pgExtStore {
		if err := o.pgExtWrite(ctx, t.ID, st.New); err != nil {
			return err
		}
		if err := save(pgExtStart); err != nil {
			return err
		}
	}
	logf(log, "restarting %s on the new image and startup modules\n", t.Slug)
	err := o.deploy.Redeploy(ctx, t.ID, log, nil)
	if err == nil {
		o.delPGExtState(ctx, m.ID)
		return o.afterDeploy(ctx, []string{t.ID}, false)
	}
	if ctx.Err() != nil {
		return err // a panel stop: the cursor resumes
	}
	switch deploy.PhaseOf(err) {
	case deploy.PhaseStart: // R1
		return o.pgExtPutBack(ctx, t, m, st, log, err)
	case deploy.PhasePooler, deploy.PhaseReconcile:
		// the server runs: the new values stay, the next deploy reconciles
		st.Due = true
		_ = save(pgExtDue)
		return err
	default:
		// V3: it came before the first container changed, so the old one
		// still runs: only the columns go back
		if werr := o.pgExtWrite(context.WithoutCancel(ctx), t.ID, st.Old); werr != nil {
			return errors.Join(fmt.Errorf("nothing was changed: %w", err), werr)
		}
		o.delPGExtState(ctx, m.ID)
		return fmt.Errorf("nothing was changed: %w", err)
	}
}

// pgExtPutBack writes the old values back and starts the old image again
// (messages 7a, 7b). It runs under WithoutCancel: a stop in the middle of it
// would leave the instance down.
func (o *Orchestrator) pgExtPutBack(ctx context.Context, t Tile, m OperatedInstance, st pgExtState, log io.Writer, cause error) error {
	ctx = context.WithoutCancel(ctx)
	st.Phase = pgExtPutBack
	if err := o.setPGExtState(ctx, m.ID, st); err != nil {
		return err
	}
	why := ""
	if cause != nil {
		why = ": " + cause.Error()
	}
	if err := o.pgExtWrite(ctx, t.ID, st.Old); err != nil {
		return err
	}
	logf(log, "%s did not start on %s; putting the previous image and startup modules back\n", t.Slug, st.New.Image)
	if err := o.deploy.Redeploy(ctx, t.ID, log, nil); err != nil {
		o.downAfterFailedDeploy(ctx, t.ID, err)
		o.delPGExtState(ctx, m.ID)
		return fmt.Errorf("%s did not start on %s. The previous image and startup modules are back, but it did not start either: %w", t.Slug, st.New.Image, err)
	}
	o.delPGExtState(ctx, m.ID)
	if err := o.afterDeploy(ctx, []string{t.ID}, false); err != nil {
		return err
	}
	return fmt.Errorf("%s did not start on %s. The previous image and startup modules are back and it runs again%s", t.Slug, st.New.Image, why)
}

func logf(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }

// pgExtBootCheck pulls the new image and boots a scratch cluster in it with
// the new startup modules before anything is stored (R19: only when the image
// is custom or the list is not empty). Failures: 30 (pull), 6 (does not
// start), 4b (a listed name the image lacks).
func (o *Orchestrator) pgExtBootCheck(ctx context.Context, t Tile, m OperatedInstance, nw pgExtVals, log io.Writer) error {
	if mflow.PGOfficial(nw.Image) && len(nw.Preload) == 0 {
		return nil
	}
	orgID, err := o.tileOrg(ctx, t)
	if err != nil {
		return err
	}
	og, err := o.store.Orgs.Get(ctx, orgID)
	if err != nil {
		return err
	}
	pullErr := func(err error) error {
		return fmt.Errorf("the image %s could not be pulled; nothing was changed: %w", nw.Image, err)
	}
	c, ok, err := o.pullCred(ctx, orgID, nw.Image)
	if err != nil {
		return pullErr(err)
	}
	run, imported, err := o.importForDeploy(ctx, orgID, nw.Image, c, log)
	if err != nil {
		return pullErr(err)
	}
	auth := ""
	if ok && !imported {
		auth = credential.Auth(c)
	}
	if _, err := o.images.Ensure(docker.WithPullOrg(ctx, og.Slug), run, auth, log); err != nil {
		return pullErr(err)
	}
	node, err := o.envNode(ctx, t.EnvironmentID)
	if err != nil {
		return err
	}
	bc := mflow.BootCheck{
		Name:    "stackr-extcheck-" + m.ID[:min(8, len(m.ID))],
		Image:   run,
		Major:   m.Major,
		Preload: nw.Preload,
		Conf:    pgDottedConf(m.Settings),
	}
	out, code, err := o.oneShotFor(ctx, node, bc.Spec(), nil, 5*time.Minute, 2000)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("postgres did not start in %s with startup modules %s; nothing was changed:\n%s", nw.Image, strings.Join(nw.Preload, ", "), out)
	}
	avail, err := mflow.ParseBootCheck(out)
	if err != nil {
		return err
	}
	for _, n := range nw.Extensions {
		if _, ok := avail[n]; !ok {
			return fmt.Errorf("the image %s has no extension %s; nothing was changed.", nw.Image, n)
		}
	}
	return nil
}

// pgDottedConf is the instance's dotted settings (an extension's own, which
// the boot check's server may refuse without its module) as conf lines.
func pgDottedConf(settings map[string]string) string {
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(settings)) {
		v := settings[k]
		if !strings.Contains(k, ".") || strings.ContainsAny(v, "\n\r") {
			continue
		}
		fmt.Fprintf(&b, "%s = '%s'\n", k, strings.ReplaceAll(v, "'", "''"))
	}
	return b.String()
}

// ---- the deploy hook and the boot resume ----

// pgExtDue is deploy's hook after an operated tile starts: reconcile when the
// instance's cursor says one is owed, then clear it. One settings read per
// instance deploy otherwise.
func (o *Orchestrator) pgExtDue(ctx context.Context, t store.Tile, log io.Writer) error {
	m, err := o.operated.GetByTile(ctx, t.ID)
	if err != nil {
		return nil // not an instance row: nothing owed
	}
	st, ok := o.pgExtOf(ctx, m.ID)
	if !ok || !st.Due {
		return nil
	}
	if err := o.engines.Reconcile(ctx, t, log); err != nil {
		return err
	}
	mu := o.pgLock(m.ID)
	mu.Lock()
	defer mu.Unlock()
	// read again: a request that came in meanwhile owns the cursor now
	cur, ok := o.pgExtOf(ctx, m.ID)
	switch {
	case !ok:
	case cur.Phase == pgExtDue:
		o.delPGExtState(ctx, m.ID)
	case cur.Phase == st.Phase && cur.Due:
		cur.Due = false
		_ = o.setPGExtState(ctx, m.ID, cur)
	}
	return nil
}

// pgExtMaxResumes caps the automatic resumes of one change.
const pgExtMaxResumes = 3

// resumePGExt finishes a change a panel restart cut short, at boot: phase
// putback, store or start is queued again and continues from its phase (a
// put-back first); queued or check stored nothing, so the cursor goes; due
// stays for the next deploy.
func (o *Orchestrator) resumePGExt(ctx context.Context) {
	rows, err := o.store.Settings.ListPrefix(ctx, pgextKey)
	if err != nil {
		return
	}
	live, err := o.store.Jobs.ListByState(ctx, "queued", "waiting", "running")
	if err != nil {
		return
	}
	for k, v := range rows {
		var st pgExtState
		if json.Unmarshal([]byte(v), &st) != nil {
			continue
		}
		id := strings.TrimPrefix(k, pgextKey)
		if st.Phase == pgExtDue {
			continue
		}
		m, err := o.operated.Get(ctx, id)
		if errors.Is(err, errs.ErrNotFound) {
			o.delPGExtState(ctx, id)
			continue
		}
		if err != nil {
			continue
		}
		if slices.ContainsFunc(live, func(j store.Job) bool { return j.Kind == string(kindPGExt) && slices.Contains(j.LockSet, m.TileID) }) {
			continue // its job may still run it
		}
		if st.Phase == pgExtQueued || st.Phase == pgExtCheck {
			o.delPGExtState(ctx, id) // nothing was stored
			continue
		}
		if st.Resumes >= pgExtMaxResumes {
			slog.Warn("pg extensions: not resuming again; change the extensions again", "instance", id, "phase", st.Phase)
			continue
		}
		if err := o.resumePGExtOne(ctx, m, st); err != nil {
			slog.Warn("pg extensions: resume failed", "instance", id, "err", err)
		}
	}
}

func (o *Orchestrator) resumePGExtOne(ctx context.Context, m OperatedInstance, st pgExtState) error {
	t, m, err := o.instanceOf(ctx, m.TileID)
	if err != nil {
		return err
	}
	nctx, err := o.withTileNode(ctx, t.ID)
	if err != nil {
		return err
	}
	lock := []string{t.ID}
	if sv, err := o.dataVolume(nctx, t); err == nil {
		if lock, err = o.pgupLocks(ctx, pgChange{sv: sv}); err != nil {
			return err
		}
	}
	st.Resumes++
	if err := o.setPGExtState(ctx, m.ID, st); err != nil {
		return err
	}
	slog.Info("pg extensions: resuming after a restart", "instance", m.ID, "phase", st.Phase)
	_, err = o.enqueue(ctx, kindPGExt, pgExtJob{TileID: t.ID, Old: st.Old, New: st.New}, lock...)
	return err
}
