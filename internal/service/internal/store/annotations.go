package store

import (
	"context"
	"time"
)

// Annotation is a note or a box drawn on one canvas.
type Annotation struct {
	ID        string    `db:"id" json:"id"`
	ScopeKind string    `db:"scope_kind" json:"scope_kind"`
	ScopeID   string    `db:"scope_id" json:"scope_id"`
	Kind      string    `db:"kind" json:"kind"`
	X         int       `db:"x" json:"x"`
	Y         int       `db:"y" json:"y"`
	W         int       `db:"w" json:"w"`
	H         int       `db:"h" json:"h"`
	Text      string    `db:"text" json:"text"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

type AnnotationStore interface {
	Create(ctx context.Context, a Annotation) error
	Get(ctx context.Context, id string) (Annotation, error)
	ListByScope(ctx context.Context, scopeKind, scopeID string) ([]Annotation, error)
	Update(ctx context.Context, a Annotation) error
	Delete(ctx context.Context, id string) error
}

var annotationsT = newTable[Annotation]("annotations", nil)

type annotations struct{ crud[Annotation] }

func (s annotations) ListByScope(ctx context.Context, scopeKind, scopeID string) ([]Annotation, error) {
	return s.many(ctx, "scope_kind = ? AND scope_id = ?", scopeKind, scopeID)
}
