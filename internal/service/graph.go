package service

import (
	"context"

	"github.com/FyrmForge/stackr/internal/service/internal/flow/graph"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/canvas"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// The canvas (ui-plan §2): one view per level of the tree.
type (
	CanvasScope = canvas.Scope
	Point       = canvas.Point
	GraphView   = graph.View
	GraphNode   = graph.Node
	GraphEdge   = graph.Edge
	GraphSub    = graph.Sub
	GraphRung   = graph.Rung
	GraphShow   = graph.Show
	Annotation  = store.Annotation
)

// Canvas kinds, the CanvasScope.Kind values. Home's ID is the viewer's.
const (
	CanvasHome  = canvas.Home
	CanvasOrg   = canvas.Org
	CanvasStack = canvas.Stack
	CanvasEnv   = canvas.Env
)

// ShowAll draws every card and edge kind.
var ShowAll = graph.All

func (o *Orchestrator) graphIn(ctx context.Context, s CanvasScope, show GraphShow, status bool) (graph.In, error) {
	in := graph.In{Show: show, Status: status}
	if s.Kind == canvas.Env && show.Traffic {
		var err error
		if in.Traffic, err = o.sample.Edges(ctx, s.ID); err != nil {
			return in, err
		}
	}
	return in, nil
}

// Canvas is the level at s: cards with worst-of status, edges, ghost refs,
// positions (saved or arranged) and annotations.
func (o *Orchestrator) Canvas(ctx context.Context, s CanvasScope, show GraphShow) (GraphView, error) {
	in, err := o.graphIn(ctx, s, show, true)
	if err != nil {
		return GraphView{}, err
	}
	return o.graph.Build(ctx, s, in)
}

// SetPosition saves where a card was dropped ("note:<id>" moves a note).
// A card that is not on the canvas is errs.ErrNotFound.
func (o *Orchestrator) SetPosition(ctx context.Context, s CanvasScope, nodeID string, p Point) error {
	in, err := o.graphIn(ctx, s, ShowAll, false)
	if err != nil {
		return err
	}
	return o.graph.Place(ctx, s, in, nodeID, p)
}

// ResetPositions forgets the canvas's positions: every card is arranged
// again. Annotations stay.
func (o *Orchestrator) ResetPositions(ctx context.Context, s CanvasScope) error {
	return o.canvas.Reset(ctx, s)
}

func (o *Orchestrator) Annotations(ctx context.Context, s CanvasScope) ([]Annotation, error) {
	return o.canvas.Annotations(ctx, s)
}

// SetAnnotation creates (ID "") or rewrites a note or box. A note saved
// with no text is deleted; deleted says so.
func (o *Orchestrator) SetAnnotation(
	ctx context.Context,
	s CanvasScope,
	a Annotation,
) (out Annotation, deleted bool, err error) {
	return o.canvas.Put(ctx, s, a)
}

func (o *Orchestrator) DeleteAnnotation(ctx context.Context, s CanvasScope, id string) error {
	return o.canvas.Delete(ctx, s, id)
}
