package promote

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/deploy"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/hostgrant"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Sync is the dry run of making one env's setup match another's: tiles
// added, removed and edited, their params and volumes. Versions (image,
// branch), paused and domains are never touched. Tiles lists every tagged
// slug, dropped ones included, so a review can offer Undo.
type Sync struct {
	Plan
	From  string     `json:"from"`
	Tiles []SyncTile `json:"tiles"`
	// Sig is the hash of everything the full sync would write; the deploy
	// posts the review's and a mismatch refuses.
	Sig string `json:"sig"`
	// Creates are the rows the sync would add, for the canvas ghosts.
	Creates []store.Tile `json:"-"`
	// Owed are the ids of the tiles whose deploy a parked rollout has left,
	// the one that parked first; SyncRollout picks them up.
	Owed []string `json:"-"`
}

type SyncTile struct {
	Slug    string `json:"slug"`
	Kind    string `json:"kind"`
	Tag     string `json:"tag"` // new | edited | removed
	Dropped bool   `json:"dropped,omitempty"`
}

const (
	tagNew     = "new"
	tagEdited  = "edited"
	tagRemoved = "removed"
)

// SyncPlan is the dry run of making envID's setup match fromID's, minus drop.
func (f *Flow) SyncPlan(ctx context.Context, envID, fromID string, drop []string) (*Sync, error) {
	e, src, st, err := f.syncEnvs(ctx, envID, fromID)
	if err != nil {
		return nil, err
	}
	s, _, err := f.syncWork(ctx, e, src, st, nil)
	if err != nil || len(drop) == 0 {
		return s, err
	}
	sig := s.Sig
	if s, _, err = f.syncWork(ctx, e, src, st, drop); err != nil {
		return nil, err
	}
	s.Sig = sig
	return s, nil
}

// SyncApply re-plans, refuses on drift or blockers, then applies. keep are
// the slugs to sync (drop = tagged minus keep); sig is the review's.
func (f *Flow) SyncApply(
	ctx context.Context,
	envID, fromID string,
	keep []string,
	sig string,
	log io.Writer,
	swap func() error,
) (*Sync, error) {
	e, src, st, err := f.syncEnvs(ctx, envID, fromID)
	if err != nil {
		return nil, err
	}
	full, _, err := f.syncWork(ctx, e, src, st, nil)
	if err != nil {
		return nil, err
	}
	if full.Sig != sig {
		return nil, errs.Conflictf("the environments changed since the review; review again")
	}
	var drop []string
	for _, t := range full.Tiles {
		if !slices.Contains(keep, t.Slug) {
			drop = append(drop, t.Slug)
		}
	}
	s, w, err := f.syncWork(ctx, e, src, st, drop)
	if err != nil {
		return nil, err
	}
	if s.Blocked() {
		if err := needsApproval(&s.Plan, st); err != nil {
			return s, err
		}
		return s, errs.Conflictf("%s", strings.Join(s.Blockers, "; "))
	}
	if len(s.Changes) == 0 {
		return s, errs.Conflictf("nothing to sync")
	}
	for _, c := range s.Changes {
		logf(log, "plan: %s\n", c.Line())
	}
	if swap != nil {
		if err := swap(); err != nil {
			return s, err
		}
	}
	err = f.applySync(ctx, w, log)
	s.Deployed, s.Owed, s.Removed = w.deployed, w.owed, w.removed
	return s, err
}

// SyncRollout deploys the tiles a parked sync still owes (Sync.Owed). Its
// rows are already written, so it plans nothing and checks no sig: a re-plan
// would read the sync's own writes as drift.
func (f *Flow) SyncRollout(ctx context.Context, owed []string, log io.Writer) (*Sync, error) {
	w := &work{}
	err := f.syncDeploy(ctx, w, owed, log)
	return &Sync{Deployed: w.deployed, Owed: w.owed}, err
}

func (f *Flow) syncEnvs(ctx context.Context, envID, fromID string) (e, src store.Environment, st store.Stack, err error) {
	d := f.D
	if e, err = d.Envs.Get(ctx, envID); err != nil {
		return
	}
	if src, err = d.Envs.Get(ctx, fromID); err != nil {
		return
	}
	st, err = d.Stacks.Get(ctx, e.StackID)
	return
}

// syncItem is one tagged slug: old is the target's row and want the row it
// would hold after the sync (a removed tile has none).
type syncItem struct {
	tag         string
	old, want   store.Tile
	oldM, wantM store.ManagedInstance
}

// syncWork is the one planner. w.re is a thin ResolvedEnv of the tiles the
// env holds after the sync (drop applied), read by the plan helpers it
// shares with promote; rows are never built through toRow.
func (f *Flow) syncWork(
	ctx context.Context,
	e, src store.Environment,
	st store.Stack,
	drop []string,
) (*Sync, *work, error) {
	d := f.D
	s := &Sync{
		Plan:  Plan{Stack: st.Slug, Env: e.Slug, Changes: []Change{}},
		From:  src.Slug,
		Tiles: []SyncTile{},
	}
	p := &s.Plan
	w := &work{
		e:         e,
		st:        st,
		re:        &ResolvedEnv{Tiles: map[string]TileConf{}},
		domains:   map[string]domainWork{},
		instances: map[string]instanceWork{},
		declare:   map[string]VolumeConf{},
		redeploy:  map[string]bool{},
		unpin:     map[string]bool{},
	}
	// orgSlug reads w.orgs: slice targets and allow lists name the org.
	orgs, err := d.Orgs.ListAll(ctx)
	if err != nil {
		return nil, nil, err
	}
	w.orgs = orgs
	if e.ID == src.ID {
		p.block("an environment cannot sync from itself")
	}
	if st.ConfigRepo != "" {
		p.block("the stack is managed by its config file; change the file instead")
	}
	if src.StackID != e.StackID {
		p.block("%s belongs to another stack", src.Slug)
	}
	if p.Blocked() {
		s.Sig = w.sig()
		return s, w, nil
	}
	if e.BaseEnvID != nil {
		if base, err := d.Envs.Get(ctx, *e.BaseEnvID); err == nil {
			w.base = base.Slug
		}
	}

	live, err := d.Tiles.List(ctx, e.ID)
	if err != nil {
		return nil, nil, err
	}
	theirs, err := d.Tiles.List(ctx, src.ID)
	if err != nil {
		return nil, nil, err
	}
	have, from := map[string]store.Tile{}, map[string]store.Tile{}
	names := map[string]bool{}
	insts := map[string]store.ManagedInstance{} // by tile id
	for _, t := range live {
		have[t.Slug], names[t.Slug] = t, true
	}
	for _, t := range theirs {
		from[t.Slug], names[t.Slug] = t, true
	}
	for _, t := range slices.Concat(live, theirs) {
		if t.Kind != tile.Managed {
			continue
		}
		m, err := d.Managed.GetByTile(ctx, t.ID)
		if err != nil && !errors.Is(err, errs.ErrNotFound) {
			return nil, nil, err
		}
		insts[t.ID] = m
	}
	order := slices.Sorted(maps.Keys(names))

	// Tags.
	items := map[string]syncItem{}
	for _, n := range order {
		o, inEnv := have[n]
		t, inSrc := from[n]
		switch {
		case !inSrc:
			items[n] = syncItem{tag: tagRemoved, old: o, oldM: insts[o.ID]}
		case !inEnv:
			want := t
			want.ID, want.Paused = "", false
			want.StackID, want.EnvironmentID = e.StackID, e.ID
			want.CreatedAt, want.UpdatedAt = time.Time{}, time.Time{}
			if tile.Builds(want) && e.FromKind == environment.FromBranch {
				want.GitBranch = e.FromBranch
			}
			items[n] = syncItem{tag: tagNew, want: want, wantM: insts[t.ID]}
		default:
			want := t
			want.ID, want.StackID, want.EnvironmentID = o.ID, o.StackID, o.EnvironmentID
			want.Name, want.ImageRef, want.GitBranch, want.Paused = o.Name, o.ImageRef, o.GitBranch, o.Paused
			want.CreatedAt, want.UpdatedAt = o.CreatedAt, o.UpdatedAt
			moved := o.Kind == tile.Managed && t.Kind == tile.Managed && instMoved(insts[o.ID], insts[t.ID])
			if tile.Diff(o, want).Any() || moved {
				items[n] = syncItem{tag: tagEdited, old: o, want: want, oldM: insts[o.ID], wantM: insts[t.ID]}
			}
		}
	}

	// The tiles the env holds after the sync. A dropped slug is the target's
	// own state: a dropped New has no entry, a dropped Edited or Removed keeps
	// its row. One rule feeds the deletes, refs, slice targets and dangling.
	gone := map[string]bool{}
	for _, n := range drop {
		gone[n] = true
	}
	rows := map[string]store.Tile{}
	wants := map[string]store.Tile{} // kept New and Edited
	for _, n := range order {
		it, tagged := items[n]
		if tagged {
			s.Tiles = append(s.Tiles, SyncTile{Slug: n, Kind: it.kind(), Tag: it.tag, Dropped: gone[n]})
		}
		switch {
		case !tagged:
			rows[n] = have[n]
			w.re.Tiles[n] = thinConf(have[n], insts[have[n].ID])
		case gone[n]:
			if it.tag != tagNew {
				rows[n] = it.old
				w.re.Tiles[n] = thinConf(it.old, it.oldM)
			}
		case it.tag != tagRemoved:
			rows[n] = it.want
			wants[n] = it.want
			w.re.Tiles[n] = thinConf(it.want, it.wantM)
		}
	}

	for _, n := range slices.Sorted(maps.Keys(wants)) {
		it, want, tc := items[n], wants[n], w.re.Tiles[n]
		if it.tag == tagNew {
			if err := tile.Validate(&want); err != nil {
				p.block("tile %s: %v", n, err)
				continue
			}
			wants[n], rows[n] = want, want
			w.creates = append(w.creates, want)
			w.redeploy[n] = true
			p.add(Change{Kind: "create", Tile: n, New: want.Kind})
			if tile.Builds(want) {
				p.Warnings = append(p.Warnings, noBuild(n, e))
			}
			if err := f.syncKind(ctx, p, w, want, store.Tile{}, false, tc); err != nil {
				return nil, nil, err
			}
			warnHost(p, e, n, want, store.Tile{})
			continue
		}
		old := it.old
		if old.Kind != want.Kind {
			if err := f.planUpdate(ctx, p, w, old, want, tc); err != nil {
				return nil, nil, err
			}
			continue
		}
		if tile.Builds(old) != tile.Builds(want) {
			p.block(
				"tile %s changes its source (%s); remove it in one sync and add it back in the next",
				n,
				sourceChange(want),
			)
			continue
		}
		if err := tile.Validate(&want); err != nil {
			p.block("tile %s: %v", n, err)
			continue
		}
		wants[n], rows[n] = want, want
		if err := f.planUpdate(ctx, p, w, old, want, tc); err != nil {
			return nil, nil, err
		}
		if err := f.syncKind(ctx, p, w, want, old, true, tc); err != nil {
			return nil, nil, err
		}
		if want.Kind == tile.Managed && w.redeploy[n] {
			p.Warnings = append(p.Warnings, n+" restarts to take the new settings")
		}
		warnHost(p, e, n, want, old)
	}
	// A slice that is new or moved reaches its consumers through their refs.
	for _, sl := range w.sliced {
		for _, n := range slices.Sorted(maps.Keys(w.re.Tiles)) {
			if refsTile(w.re.Tiles[n], sl) {
				w.redeploy[n] = true
			}
		}
	}
	if err := f.planDeletes(ctx, p, w, live); err != nil {
		return nil, nil, err
	}
	if err := f.syncParams(ctx, p, w, src, wants); err != nil {
		return nil, nil, err
	}
	if err := f.syncVolumes(ctx, p, w, src, rows, items, gone); err != nil {
		return nil, nil, err
	}
	dangling(p, rows)
	envNotes(p, items)
	if err := f.syncHostAccess(ctx, p, st, wants); err != nil {
		return nil, nil, err
	}
	s.Creates = w.creates
	s.Sig = w.sig()
	return s, w, nil
}

// syncHostAccess blocks, as a promote does, when the tiles the sync adds or
// edits ask for host access the stack's grant lacks. Apply parks on it.
func (f *Flow) syncHostAccess(ctx context.Context, p *Plan, st store.Stack, wants map[string]store.Tile) error {
	if f.D.HostGrants == nil {
		return nil
	}
	have, err := f.D.HostGrants.Of(ctx, st.ID)
	if err != nil {
		return err
	}
	var rows []store.Tile
	for _, n := range slices.Sorted(maps.Keys(wants)) {
		rows = append(rows, wants[n])
	}
	if m := deploy.HostSet(rows...).Missing(have); len(m) > 0 {
		p.block("%s", hostgrant.Text(m))
	}
	return nil
}

// syncKind is the managed and slice rows of one New or Edited tile.
func (f *Flow) syncKind(
	ctx context.Context,
	p *Plan,
	w *work,
	want, old store.Tile,
	exists bool,
	tc TileConf,
) error {
	switch want.Kind {
	case tile.Managed:
		return f.planInstance(ctx, p, w, want.Slug, tc, old, exists)
	case tile.Slice:
		return f.planSlice(ctx, p, w, want, old, exists)
	}
	return nil
}

func (it syncItem) kind() string {
	if it.tag == tagRemoved {
		return it.old.Kind
	}
	return it.want.Kind
}

func sourceChange(want store.Tile) string {
	if tile.Builds(want) {
		return "image to git build"
	}
	return "git build to image"
}

func noBuild(slug string, e store.Environment) string {
	if e.FromKind == environment.FromBranch {
		return fmt.Sprintf("%s has no build in %s yet; it starts on the next push to %s", slug, e.Slug, e.FromBranch)
	}
	return fmt.Sprintf("%s has no build in %s yet; it starts with the next release promoted here", slug, e.Slug)
}

// warnHost flags a tile that gains host access the target row did not have.
func warnHost(p *Plan, e store.Environment, slug string, want, old store.Tile) {
	if (want.Privileged || want.Devices != "") && !old.Privileged && old.Devices == "" {
		p.Warnings = append(p.Warnings, slug+" runs privileged or with host devices in "+e.Slug+" after this sync")
	}
}

// instMoved: the engine, allow list or env pairs of two instances differ.
func instMoved(a, b store.ManagedInstance) bool {
	return a.Engine != b.Engine || signedDiff(a.Allow, b.Allow) != "" || !maps.Equal(a.EnvPairs, b.EnvPairs)
}

// thinConf is the part of a TileConf the shared plan helpers read, from a
// tile row and its instance.
func thinConf(t store.Tile, m store.ManagedInstance) TileConf {
	tc := TileConf{
		Type:     t.Kind,
		Engine:   m.Engine,
		Allow:    m.Allow,
		EnvPairs: m.EnvPairs,
		Command:  t.Command,
	}
	_ = json.Unmarshal([]byte(t.EnvJSON), &tc.Env)
	return tc
}

func rowMap(blob string) map[string]string {
	var m map[string]string
	_ = json.Unmarshal([]byte(blob), &m)
	return m
}

// syncParams: a plain param a kept row reads that the env lacks is copied
// with its value; a secret is a warning unless the stack shares it. A name
// already set in the env keeps its value.
func (f *Flow) syncParams(
	ctx context.Context,
	p *Plan,
	w *work,
	src store.Environment,
	wants map[string]store.Tile,
) error {
	type ref struct {
		coll, name string
		tiles      []string
	}
	reads := map[string]*ref{}
	for _, n := range slices.Sorted(maps.Keys(wants)) {
		t := wants[n]
		vals := append(slices.Collect(maps.Values(rowMap(t.EnvJSON))), t.Command)
		for _, v := range vals {
			for _, body := range params.Refs(v) {
				r, err := params.Parse(body)
				if err != nil || r.Kind != params.KindParam {
					continue
				}
				key := r.Slug + "." + r.Name
				if reads[key] == nil {
					reads[key] = &ref{coll: r.Slug, name: r.Name}
				}
				if !slices.Contains(reads[key].tiles, n) {
					reads[key].tiles = append(reads[key].tiles, n)
				}
			}
		}
	}
	if len(reads) == 0 {
		return nil
	}
	vals := func(kind, id string) (map[string]params.Value, error) {
		return f.D.Params.Values(ctx, params.Scope{Kind: kind, ID: id}, true)
	}
	from, err := vals("env", src.ID)
	if err != nil {
		return err
	}
	here, err := vals("env", w.e.ID)
	if err != nil {
		return err
	}
	shared, err := vals("stack", w.st.ID)
	if err != nil {
		return err
	}
	for _, key := range slices.Sorted(maps.Keys(reads)) {
		r := reads[key]
		sv, ok := from[key]
		_, inEnv := here[key]
		if !ok || inEnv {
			continue
		}
		if sv.Secret {
			if _, inStack := shared[key]; inStack {
				continue
			}
			p.Warnings = append(p.Warnings, fmt.Sprintf(
				"params.%s is a secret and is not set in %s; %s read it", key, w.e.Slug, strings.Join(r.tiles, ", ")))
			continue
		}
		p.add(Change{Kind: "param", Field: key})
		w.params = append(w.params, params.Entry{Collection: r.coll, Name: r.name, Kind: params.Param, Value: sv.V})
	}
	return nil
}

// mounts are the volume slugs a row mounts ("slug:/path[:ro]").
func mounts(t store.Tile) []string {
	var out []string
	for _, l := range tile.Lines(t.Volumes) {
		if m, err := tile.ParseMount(l); err == nil && m.Kind == tile.MountVolume {
			out = append(out, m.Volume)
		}
	}
	return out
}

// syncVolumes: a volume a kept row mounts is declared in the env when it is
// missing, resized or orphaned there. A volume only removed tiles mounted is
// orphaned. planVolumes is not reused: it orphans everything the file lacks.
func (f *Flow) syncVolumes(
	ctx context.Context,
	p *Plan,
	w *work,
	src store.Environment,
	rows map[string]store.Tile,
	items map[string]syncItem,
	gone map[string]bool,
) error {
	read := func(e store.Environment) (map[string]store.Volume, error) {
		vs, err := f.D.Volumes.List(ctx, volume.Scope{Kind: "env", ID: e.ID})
		out := map[string]store.Volume{}
		for _, v := range vs {
			if v.InstanceID == nil {
				out[v.Slug] = v
			}
		}
		return out, err
	}
	from, err := read(src)
	if err != nil {
		return err
	}
	here, err := read(w.e)
	if err != nil {
		return err
	}
	used, want := map[string]bool{}, map[string]bool{}
	for n, t := range rows {
		for _, sl := range mounts(t) {
			used[sl] = true
			if it, ok := items[n]; ok && !gone[n] && it.tag != tagRemoved {
				want[sl] = true
			}
		}
	}
	for _, sl := range slices.Sorted(maps.Keys(want)) {
		sv, ok := from[sl]
		if !ok {
			continue
		}
		tv, there := here[sl]
		switch {
		case !there:
			p.add(Change{Kind: "volume", New: sl})
		case tv.OrphanedAt != nil:
			p.add(Change{Kind: "volume", New: sl, Note: "re-adopts the orphaned volume and its data"})
		case tv.MaxSizeMB != sv.MaxSizeMB:
			p.add(Change{
				Kind:  "volume",
				Field: "max_size_mb",
				Old:   fmt.Sprint(tv.MaxSizeMB),
				New:   fmt.Sprint(sv.MaxSizeMB),
				Tile:  sl,
			})
		default:
			continue
		}
		w.declare[sl] = VolumeConf{MaxSizeMB: sv.MaxSizeMB}
	}
	freed := map[string]bool{}
	for n, it := range items {
		if it.tag == tagRemoved && !gone[n] {
			for _, sl := range mounts(it.old) {
				freed[sl] = true
			}
		}
	}
	for _, sl := range slices.Sorted(maps.Keys(freed)) {
		if v, ok := here[sl]; ok && !used[sl] && v.OrphanedAt == nil {
			p.add(Change{Kind: "orphan", Old: sl, Note: "the volume and its data stay; retention deletes it later"})
			w.orphan = append(w.orphan, v)
		}
	}
	return nil
}

// dangling blocks a result that names a tile it does not hold, through
// depends_on, a tile ref or slice_access.
func dangling(p *Plan, rows map[string]store.Tile) {
	for _, n := range slices.Sorted(maps.Keys(rows)) {
		for _, to := range slices.Sorted(slices.Values(uses(rows[n]))) {
			if _, ok := rows[to]; !ok && to != n {
				p.block("tile %s names %s, which %s will not have", n, to, p.Env)
			}
		}
	}
}

// uses are the slugs a row names: depends_on, tile refs, slice_access.
func uses(t store.Tile) []string {
	var out []string
	add := func(s string) {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	for _, l := range tile.Lines(t.DependsOn) {
		if sl, _, err := tile.ParseDep(l); err == nil {
			add(sl)
		}
	}
	for _, v := range append(slices.Collect(maps.Values(rowMap(t.EnvJSON))), t.Command) {
		for _, body := range params.Refs(v) {
			if r, err := params.Parse(body); err == nil && r.Kind == params.KindTile {
				add(r.Slug)
			}
		}
	}
	for _, a := range t.SliceAccess {
		add(a.From)
	}
	return out
}

// envNotes: an env or build_args update row carries which keys came and went,
// never a value.
func envNotes(p *Plan, items map[string]syncItem) {
	for i, c := range p.Changes {
		it, ok := items[c.Tile]
		if c.Kind != "update" || !ok || it.tag != tagEdited {
			continue
		}
		switch c.Field {
		case "env":
			p.Changes[i].Note = signedDiff(
				slices.Sorted(maps.Keys(rowMap(it.old.EnvJSON))), slices.Sorted(maps.Keys(rowMap(it.want.EnvJSON))))
		case "build_args":
			p.Changes[i].Note = signedDiff(
				slices.Sorted(maps.Keys(rowMap(it.old.BuildArgs))), slices.Sorted(maps.Keys(rowMap(it.want.BuildArgs))))
		}
	}
}

// sig hashes what the work would write, so any change to it moves the hash
// and the display does not matter.
func (w *work) sig() string {
	type inst struct {
		Allow []string
		Pairs map[string]string
	}
	in := map[string]inst{}
	for k, v := range w.instances {
		in[k] = inst{v.allow, v.pairs}
	}
	var upd []store.Tile
	for _, u := range w.updates {
		upd = append(upd, u[1])
	}
	var del, orph []string
	for _, t := range w.deletes {
		del = append(del, t.ID)
	}
	for _, v := range w.orphan {
		orph = append(orph, v.ID)
	}
	slices.Sort(del)
	slices.Sort(orph)
	b, _ := json.Marshal(struct {
		Creates, Updates []store.Tile
		Deletes, Orphan  []string
		Params           []params.Entry
		Declare          map[string]VolumeConf
		Instances        map[string]inst
	}{w.creates, upd, del, orph, w.params, w.declare, in})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

// applySync writes the work, then rolls out: a New tile deploys unless it
// builds from git (nothing built yet); an Edited tile, and a consumer of a
// new or moved slice, only when it has replicas. Every deploy is Redeploy,
// which owns pins and the first-run derive.
func (f *Flow) applySync(ctx context.Context, w *work, log io.Writer) error {
	d, e := f.D, w.e
	if len(w.params) > 0 {
		if err := d.Params.Merge(ctx, params.Scope{Kind: "env", ID: e.ID}, w.params); err != nil {
			return err
		}
	}
	scope := volume.Scope{Kind: "env", ID: e.ID}
	for _, n := range slices.Sorted(maps.Keys(w.declare)) {
		if _, _, err := d.Volumes.Declare(ctx, scope, n, w.declare[n].MaxSizeMB, nil); err != nil {
			return err
		}
	}
	for _, v := range w.orphan {
		if _, err := d.Volumes.Orphan(ctx, v); err != nil {
			return err
		}
	}
	created := map[string]bool{}
	for _, row := range w.creates {
		t, err := d.Tiles.Create(ctx, row)
		if err != nil {
			return fmt.Errorf("create %s: %w", row.Slug, err)
		}
		if t.Kind == tile.Managed {
			if _, err := d.Managed.Create(ctx, t.ID, w.re.Tiles[t.Slug].Engine, "stackr", ""); err != nil {
				return err
			}
		}
		created[t.Slug] = true
		logf(log, "created %s\n", t.Slug)
	}
	for _, u := range w.updates {
		if _, _, err := d.Tiles.Update(ctx, u[0], u[1]); err != nil {
			return fmt.Errorf("update %s: %w", u[0].Slug, err)
		}
	}
	if err := f.applyInstances(ctx, w, e); err != nil {
		return err
	}
	var err error
	if w.removed, err = f.Remove(ctx, e, w.deletes, log); err != nil {
		return err
	}
	if w.sync && d.Sync != nil {
		if err := d.Sync(ctx); err != nil {
			return err
		}
	}

	live, err := d.Tiles.List(ctx, e.ID)
	if err != nil {
		return err
	}
	bySlug := map[string]store.Tile{}
	var slugs []string
	for _, t := range live {
		bySlug[t.Slug] = t
		slugs = append(slugs, t.Slug)
	}
	order := topo(slugs, depsOf(live))
	if order == nil {
		return errors.New("depends_on has a cycle")
	}
	var ids []string
	for pass := range 3 {
		for _, s := range order {
			t := bySlug[s]
			if !w.redeploy[s] || rolloutPass(t) != pass {
				continue
			}
			if created[s] && tile.Builds(t) {
				logf(log, "%s: no build yet, not deployed\n", s)
				continue
			}
			if !created[s] {
				if reps, err := replicaIDs(ctx, d, t); err != nil {
					return err
				} else if len(reps) == 0 {
					continue
				}
			}
			ids = append(ids, t.ID)
		}
	}
	return f.syncDeploy(ctx, w, ids, log)
}

// syncDeploy redeploys the tiles in order. When one fails, w.owed holds it
// and the rest: a deploy parked on an unset param is resumed from there.
func (f *Flow) syncDeploy(ctx context.Context, w *work, ids []string, log io.Writer) error {
	for i, id := range ids {
		t, err := f.D.Tiles.Get(ctx, id)
		switch {
		case errors.Is(err, errs.ErrNotFound):
			logf(log, "a tile owed a deploy is gone; skipped\n")
			continue
		case err != nil:
			w.owed = ids[i:]
			return err
		}
		logf(log, "deploying %s\n", t.Slug)
		if err := f.D.Redeploy(ctx, id, log, nil); err != nil {
			w.owed = ids[i:]
			return fmt.Errorf("deploy %s: %w", t.Slug, err)
		}
		w.deployed = append(w.deployed, id)
	}
	return nil
}
