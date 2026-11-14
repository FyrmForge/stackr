package service

import (
	"context"
	"errors"
	"io"

	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Postgres extensions on operated instances: an instance lists the extensions
// its slices may use and the startup modules it loads; a slice lists the ones
// its database holds. The types here are real; the bodies are stubs the
// service lane fills in. A stub on a path every deploy takes (pgExtDue,
// resumePGExt) does nothing, so deploys behave as before; one only the new
// entry points reach returns errPGExtNotBuilt.

const (
	kindPGExt jobs.Kind = "pg-extensions"

	// pgextKey is an instance's extensions change in flight: pgext.<instance
	// id>. A cursor in the "due" phase only says the values are stored and a
	// reconcile is owed at the next start; it does not block new requests.
	pgextKey = "pgext."
)

var errPGExtNotBuilt = errors.New("postgres extensions: not built yet")

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

// PGExtensions is instance tileID's extensions view. Stub.
func (o *Orchestrator) PGExtensions(ctx context.Context, tileID string) (PGExtView, error) {
	return PGExtView{}, errPGExtNotBuilt
}

// SetPGExtensions changes an instance's image, startup modules or
// extensions; the job is nil when nothing needs one. Stub.
func (o *Orchestrator) SetPGExtensions(ctx context.Context, by *Principal, tileID string, in PGExtIn) (PGExtView, *Job, error) {
	return PGExtView{}, nil, errPGExtNotBuilt
}

// SliceExtensions is slice sliceID's extensions view. Stub.
func (o *Orchestrator) SliceExtensions(ctx context.Context, sliceID string) (SliceExtView, error) {
	return SliceExtView{}, errPGExtNotBuilt
}

// SetSliceExtensions sets a slice's list; a provisioned slice queues its own
// deploy and returns that job. Stub.
func (o *Orchestrator) SetSliceExtensions(ctx context.Context, sliceID string, names []string) (Tile, *Job, error) {
	return Tile{}, nil, errPGExtNotBuilt
}

// setPGExt is the one entry SetPGExtensions and UpdateTile share. Stub.
func (o *Orchestrator) setPGExt(ctx context.Context, t Tile, m OperatedInstance, in PGExtIn) (Tile, *Job, error) {
	return Tile{}, nil, errPGExtNotBuilt
}

// applyPGExt is the one body the job and the stack file hook run. Stub.
func (o *Orchestrator) applyPGExt(ctx context.Context, t Tile, m OperatedInstance, st pgExtState, log io.Writer) error {
	return errPGExtNotBuilt
}

// runPGExt is the kindPGExt job handler. Stub.
func (o *Orchestrator) runPGExt(ctx context.Context, r *jobs.Run, p pgExtJob) error {
	return errPGExtNotBuilt
}

// pgExtFromFile is the stack file's way in (promote's Flow.PGExt): the
// image, preload and extensions a file sets for an instance, nil = not in the
// file. Stub.
func (o *Orchestrator) pgExtFromFile(ctx context.Context, t store.Tile, m store.OperatedInstance, image *string, preload, exts *[]string, log io.Writer) error {
	return errPGExtNotBuilt
}

// pgExtDue is deploy's hook after an operated tile starts: reconcile when the
// instance's cursor says one is owed. Stub: nothing is ever owed.
func (o *Orchestrator) pgExtDue(ctx context.Context, t store.Tile, log io.Writer) error {
	return nil
}

// resumePGExt finishes a change a panel restart cut short, at boot. Stub.
func (o *Orchestrator) resumePGExt(ctx context.Context) {}
