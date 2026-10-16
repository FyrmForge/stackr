package service

import (
	"context"
	"fmt"
	"slices"

	"github.com/FyrmForge/stackr/internal/service/internal/flow/move"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// The move progress view reads these; moves start in placementmove.go.

// MoveStep is one line of the progress view. State is done, running or
// waiting.
type MoveStep struct {
	Label  string
	State  string
	Detail string
}

// MoveStatus is the newest move of an env, as the progress view shows it.
type MoveStatus struct {
	Move     EnvMove
	Path     string // org/stack/env
	From, To string // node names
	Active   bool
	Steps    []MoveStep
}

// EnvMoveStatus is the env's newest move, nil when it never moved.
func (o *Orchestrator) EnvMoveStatus(ctx context.Context, envID string) (*MoveStatus, error) {
	ms, err := o.store.Moves.ListByEnv(ctx, envID)
	if err != nil || len(ms) == 0 {
		return nil, err
	}
	m := slices.MaxFunc(ms, func(a, b store.EnvMove) int { return a.StartedAt.Compare(b.StartedAt) })
	name := o.nodeNamer(ctx)
	s := &MoveStatus{Move: m, From: name(m.FromNode), To: name(m.ToNode)}
	if e, err := o.store.Environments.Get(ctx, envID); err == nil {
		s.Path, _ = o.pmEnvPath(ctx, e)
	}
	s.Active = m.Phase != move.Done && m.Phase != move.RolledBack
	type stepDef struct{ phase, label string }
	defs := []stepDef{{move.Stopping, "Stop"}, {move.Copying, "Copy"}, {move.Starting, "Start on " + s.To}, {move.Cleaning, "Remove old containers"}}
	if m.Live {
		defs = []stepDef{{move.Syncing, "Copy while running"}, {move.Stopping, "Stop"}, {move.Final, "Final copy"}, {move.Starting, "Start on " + s.To}, {move.Cleaning, "Remove old containers"}}
	}
	cur := slices.IndexFunc(defs, func(d stepDef) bool { return d.phase == m.Phase })
	for i, d := range defs {
		st := MoveStep{Label: d.label, State: "waiting"}
		switch {
		case m.Phase == move.Done || cur >= 0 && i < cur:
			st.State = "done"
		case i == cur:
			st.State = "running"
			if m.Bytes > 0 && (d.phase == move.Syncing || d.phase == move.Copying || d.phase == move.Final) {
				st.Detail = HumanBytes(m.Bytes) + " copied"
			}
		}
		s.Steps = append(s.Steps, st)
	}
	return s, nil
}

// HumanBytes is n in the largest decimal unit that keeps it above 1.
func HumanBytes(n int64) string {
	const k = 1000
	if n < k {
		return fmt.Sprintf("%d B", n)
	}
	f, u := float64(n), "kMGTP"
	for i := 0; ; i++ {
		f /= k
		if f < k || i == len(u)-1 {
			return fmt.Sprintf("%.1f %cB", f, u[i])
		}
	}
}
