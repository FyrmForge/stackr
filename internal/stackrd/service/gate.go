package service

import (
	"context"

	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// GateKind is which question is being asked of a config-managed stack. The
// two are not one function with a flag bolted on: they answer different
// things, and collapsing them would let a structural write stage on a stack
// whose file owns its structure (00-calibration.md, "The gate").
type GateKind int

const (
	// GateFieldEdit is an edit to a field the config file owns — a setting,
	// a limit, a domain's HTTPS toggle. On a stack that declares
	// `ui_edits: stage` this is held in the env's pending set and the next
	// config plan shows it as drift, terraform style.
	GateFieldEdit GateKind = iota
	// GateStructural is a create or delete: a tile, a domain, an env. The
	// file owns the shape, so this always refuses on a config-managed stack,
	// whatever ui_edits says.
	GateStructural
)

// Surface is where the write came from, because whether a write queues for
// review is a property of the surface as much as of the stack. The canvas
// queues every edit into the env's pending set and applies them together;
// the API and the CLI deliberately do not (docs/features/api.md), so a
// script can act without a human pressing Apply afterwards.
type Surface int

const (
	// SurfaceCanvas is the web panel: edits land in the pending set and the
	// tile shows as changed (or struck through) until the set is applied.
	SurfaceCanvas Surface = iota
	// SurfaceDirect is the API, the CLI and a config apply: the write lands
	// immediately.
	SurfaceDirect
)

// GateService answers "may this stack be written to, and does the write go
// straight through or into the pending set".
type GateService struct{ store repo.Store }

func NewGateService(store repo.Store) *GateService { return &GateService{store: store} }

// Gate reports whether the write stages rather than applying immediately, or
// refuses it.
//
// It fails closed on every uncertainty: a stack that cannot be read is a
// refusal, not a pass, because "cannot tell" must never become "allowed" on a
// locked stack. A missing stack is ErrNotFound rather than a conflict — the
// caller may not be entitled to know it is missing at all.
func (g *GateService) Gate(ctx context.Context, stackID string, kind GateKind, via Surface) (stage bool, err error) {
	s, err := g.store.GetStack(ctx, stackID)
	if err != nil || s == nil {
		return false, svcerr.ErrNotFound
	}
	if !s.ConfigManaged() {
		// Nobody but the UI owns this stack, so the only question left is the
		// surface's own habit.
		return via == SurfaceCanvas, nil
	}
	// A structural write refuses whatever ui_edits says: the file owns the
	// shape of the stack, and a staged create or delete would be a change to
	// the shape waiting to be applied behind the file's back.
	if kind == GateFieldEdit && s.UIEdits() == repo.UIEditsStage {
		// Forced, and forced for every surface. The API used to refuse here
		// while the panel staged, so a stack set to `ui_edits: stage` took a
		// settings edit from a browser and 409'd the identical CLI edit
		// (00-calibration.md, "The gate").
		return true, nil
	}
	return false, ManagedConflict(s)
}

// ManagedConflict is the 409 for a write the config file owns. Exported
// because handlers that already hold the stack (the API's create paths) use
// it without a second lookup.
func ManagedConflict(s *repo.Stack) error {
	ref := "the config file"
	if s != nil && s.ConfigRepo != "" {
		ref = s.ConfigRepo
	}
	return svcerr.Conflictf("stack is managed by %s; edit the config file to change its structure", ref)
}
