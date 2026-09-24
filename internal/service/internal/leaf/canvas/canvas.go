// Package canvas owns what people put on a canvas by hand: card positions
// (shared per canvas, like v0) and annotations (notes and boxes). Which
// cards exist is flow/graph's question, never this leaf's.
package canvas

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Scope is one canvas: home (a viewer's id), org, stack or env.
type Scope struct{ Kind, ID string }

const (
	Home  = "home"
	Org   = "org"
	Stack = "stack"
	Env   = "env"

	Note = "note"
	Box  = "box"

	// Reach bounds a coordinate; nothing sane is drawn further out.
	Reach   = 100000
	MaxText = 500
	MinBoxW = 40
	MinBoxH = 30
)

// Point is a card's top-left corner in world pixels.
type Point struct{ X, Y int }

type Leaf struct {
	positions   store.PositionStore
	annotations store.AnnotationStore
}

func New(positions store.PositionStore, annotations store.AnnotationStore) *Leaf {
	return &Leaf{positions: positions, annotations: annotations}
}

func (s Scope) valid() error {
	switch s.Kind {
	case Home, Org, Stack, Env:
		if s.ID != "" {
			return nil
		}
	}
	return errs.Invalidf("scope", "unknown canvas")
}

func inReach(x, y int) error {
	if x < -Reach || x > Reach || y < -Reach || y > Reach {
		return errs.Invalidf("x", "position is off the canvas")
	}
	return nil
}

// Positions is every saved card on the canvas, by node id.
func (l *Leaf) Positions(ctx context.Context, s Scope) (map[string]Point, error) {
	rows, err := l.positions.ListByScope(ctx, s.Kind, s.ID)
	out := make(map[string]Point, len(rows))
	for _, r := range rows {
		out[r.NodeID] = Point{r.X, r.Y}
	}
	return out, err
}

// Save places one card. The caller has checked the card is on the canvas.
func (l *Leaf) Save(ctx context.Context, s Scope, nodeID string, p Point) error {
	if err := s.valid(); err != nil {
		return err
	}
	if err := inReach(p.X, p.Y); err != nil {
		return err
	}
	return l.positions.Put(ctx, store.Position{ScopeKind: s.Kind, ScopeID: s.ID, NodeID: nodeID, X: p.X, Y: p.Y})
}

// Reset forgets every card position on the canvas; annotations stay.
func (l *Leaf) Reset(ctx context.Context, s Scope) error {
	return l.positions.DeleteScope(ctx, s.Kind, s.ID)
}

func (l *Leaf) Annotations(ctx context.Context, s Scope) ([]store.Annotation, error) {
	return l.annotations.ListByScope(ctx, s.Kind, s.ID)
}

// get is one annotation of this canvas; another canvas's is not there.
func (l *Leaf) get(ctx context.Context, s Scope, id string) (store.Annotation, error) {
	a, err := l.annotations.Get(ctx, id)
	if err == nil && (a.ScopeKind != s.Kind || a.ScopeID != s.ID) {
		err = errs.ErrNotFound
	}
	return a, err
}

// Put creates (a.ID "") or rewrites an annotation. A note emptied of text
// is deleted, as in v0: deleted reports it.
func (l *Leaf) Put(ctx context.Context, s Scope, a store.Annotation) (out store.Annotation, deleted bool, err error) {
	if err := s.valid(); err != nil {
		return a, false, err
	}
	a.Text = strings.TrimSpace(a.Text)
	switch {
	case a.Kind != Note && a.Kind != Box:
		return a, false, errs.Invalidf("kind", "must be note or box")
	case len(a.Text) > MaxText:
		return a, false, errs.Invalidf("text", "at most 500 characters")
	case a.Kind == Box && (a.W < MinBoxW || a.H < MinBoxH):
		return a, false, errs.Invalidf("w", "a box is at least 40 by 30")
	case a.W < 0 || a.H < 0 || a.W > Reach || a.H > Reach:
		return a, false, errs.Invalidf("w", "size is off the canvas")
	}
	if err := inReach(a.X, a.Y); err != nil {
		return a, false, err
	}
	if a.ID == "" {
		if a.Kind == Note && a.Text == "" {
			return a, false, errs.Invalidf("text", "a note needs text")
		}
		a.ID, a.ScopeKind, a.ScopeID, a.CreatedAt = uuid.NewString(), s.Kind, s.ID, time.Now().UTC()
		return a, false, l.annotations.Create(ctx, a)
	}
	old, err := l.get(ctx, s, a.ID)
	if err != nil {
		return a, false, err
	}
	if a.Kind == Note && a.Text == "" {
		return old, true, l.annotations.Delete(ctx, old.ID)
	}
	a.ScopeKind, a.ScopeID, a.CreatedAt = old.ScopeKind, old.ScopeID, old.CreatedAt
	return a, false, l.annotations.Update(ctx, a)
}

// Move places an annotation; the canvas's drag goes through here.
func (l *Leaf) Move(ctx context.Context, s Scope, id string, p Point) error {
	if err := inReach(p.X, p.Y); err != nil {
		return err
	}
	a, err := l.get(ctx, s, id)
	if err != nil {
		return err
	}
	a.X, a.Y = p.X, p.Y
	return l.annotations.Update(ctx, a)
}

func (l *Leaf) Delete(ctx context.Context, s Scope, id string) error {
	a, err := l.get(ctx, s, id)
	if errors.Is(err, errs.ErrNotFound) {
		return nil // already gone: a double click on ×
	}
	if err != nil {
		return err
	}
	return l.annotations.Delete(ctx, a.ID)
}
