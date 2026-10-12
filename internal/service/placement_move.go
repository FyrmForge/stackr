package service

import (
	"context"
	"fmt"
	"slices"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/move"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// The move dialog and the move progress view read these; the move itself is
// MoveEnv (move.go).

// MovePlan is what the move dialog shows for one env and one target.
type MovePlan struct {
	EnvName  string
	From, To string // node names
	FromID   string
	Targets  []PlacementNode // nodes the env may move to, not its own
	Volumes  int
	Bytes    int64 // -1 = not all sizes are known
}

// PlanMove reads the env's data on its node and the nodes it may go to. to
// may be "" (the first target is used).
func (o *Orchestrator) PlanMove(ctx context.Context, envID, to string) (MovePlan, error) {
	var p MovePlan
	e, err := o.store.Environments.Get(ctx, envID)
	if err != nil {
		return p, err
	}
	st, err := o.store.Stacks.Get(ctx, e.StackID)
	if err != nil {
		return p, err
	}
	from, err := o.nodeOfEnvID(ctx, envID)
	if err != nil {
		return p, err
	}
	ns, err := o.OfferedNodes(ctx, st.OrgID)
	if err != nil {
		return p, err
	}
	p = MovePlan{EnvName: e.Name, From: from.Name, FromID: from.ID}
	for _, n := range ns {
		if n.ID != from.ID {
			p.Targets = append(p.Targets, n)
		}
	}
	if len(p.Targets) == 0 {
		return p, errs.Conflictf("no other node is open to this organization")
	}
	p.To = p.Targets[0].Name
	for _, n := range p.Targets {
		if n.ID == to {
			p.To = n.Name
		}
	}
	vols, err := moveEnv{o}.Volumes(ctx, envID)
	if err != nil {
		return p, err
	}
	p.Volumes = len(vols)
	for _, v := range vols {
		info, err := o.docker.InspectVolume(docker.WithNode(ctx, from.ID), v.Name)
		if err != nil || info.SizeBytes < 0 {
			p.Bytes = -1
			break
		}
		p.Bytes += info.SizeBytes
	}
	return p, nil
}

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
