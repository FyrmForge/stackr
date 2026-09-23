package service

import (
	"context"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// GraphService owns a canvas's saved layout: where the cards sit, the notes
// stuck to them, and the boxes drawn around them.
//
// Three tables with one shape. Each is keyed by a repo.GraphOwner — a scope
// (server, org, stack, environment or user) plus the id of the thing at that
// scope — and the handlers that read them were assembling that key themselves,
// five times over, in the two canvases and the three panels that draw a
// miniature of one.
//
// There is no tenancy here and there should not be: the owner key names a
// resource whose org the route's gate has already resolved. What this does
// own is that the three tables are read and written as one layout, so a
// canvas that drops its positions drops its notes and boxes with them.
type GraphService struct {
	store repo.Store
}

func NewGraphService(store repo.Store) *GraphService { return &GraphService{store: store} }

// Layout is one canvas's saved state.
type Layout struct {
	Positions   []repo.NodePosition
	Annotations []repo.Annotation
	Groups      []repo.GraphGroup
}

// Load reads a canvas's whole layout. A missing layout is an empty one, not
// an error: an unvisited canvas has never saved anything and renders from the
// auto-layout.
func (s *GraphService) Load(ctx context.Context, owner string) (Layout, error) {
	var l Layout
	var err error
	if l.Positions, err = s.store.ListNodePositions(ctx, owner); err != nil {
		return Layout{}, err
	}
	if l.Annotations, err = s.store.ListAnnotations(ctx, owner); err != nil {
		return Layout{}, err
	}
	if l.Groups, err = s.store.ListGraphGroups(ctx, owner); err != nil {
		return Layout{}, err
	}
	return l, nil
}

// Positions are a canvas's card positions on their own, for the pages that
// draw cards and nothing else.
func (s *GraphService) Positions(ctx context.Context, owner string) ([]repo.NodePosition, error) {
	return s.store.ListNodePositions(ctx, owner)
}

// Annotations are a canvas's notes.
func (s *GraphService) Annotations(ctx context.Context, owner string) ([]repo.Annotation, error) {
	return s.store.ListAnnotations(ctx, owner)
}

// Groups are a canvas's boxes.
func (s *GraphService) Groups(ctx context.Context, owner string) ([]repo.GraphGroup, error) {
	return s.store.ListGraphGroups(ctx, owner)
}

// SavePositions replaces the saved positions for a canvas.
func (s *GraphService) SavePositions(ctx context.Context, owner string, ps []repo.NodePosition) error {
	return s.store.SaveNodePositions(ctx, owner, ps)
}

// ResetPositions drops a canvas's saved positions so it falls back to the
// auto-layout. Notes and boxes survive: they carry their own coordinates and
// are not a layout the auto-arranger can produce.
func (s *GraphService) ResetPositions(ctx context.Context, owner string) error {
	return s.store.DeleteNodePositions(ctx, owner)
}

// SaveAnnotation adds or updates one note.
func (s *GraphService) SaveAnnotation(ctx context.Context, a *repo.Annotation) error {
	return s.store.UpsertAnnotation(ctx, a)
}

// DeleteAnnotation removes one note from a canvas.
func (s *GraphService) DeleteAnnotation(ctx context.Context, owner, id string) error {
	return s.store.DeleteAnnotation(ctx, owner, id)
}

// SaveGroup adds or updates one box.
func (s *GraphService) SaveGroup(ctx context.Context, g *repo.GraphGroup) error {
	return s.store.UpsertGraphGroup(ctx, g)
}

// DeleteGroup removes one box from a canvas.
func (s *GraphService) DeleteGroup(ctx context.Context, owner, id string) error {
	return s.store.DeleteGraphGroup(ctx, owner, id)
}
