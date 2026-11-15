package service

import "context"

// SetPanelSeams is WithPanelSeams after New, for a test that changes what
// the outside answers between syncs (none running).
func (o *Orchestrator) SetPanelSeams(outside func(context.Context, string) (ipv4, vantage string), probe func(context.Context, string, string) (string, bool), resolve func(context.Context, string) ([]string, error)) {
	var op options
	WithPanelSeams(outside, probe, resolve)(&op)
	o.panelSeam = op.panelSeam
}

// RecheckPanelName is the tick's recheck, and WaitRecheck waits for it.
func (o *Orchestrator) RecheckPanelName(ctx context.Context) {
	o.recheckPanelName(ctx)
	o.trustAsync.Wait()
}

// ForcePanelDue makes the one-shot recheck due now; PanelDue says whether
// one is still waiting.
func (o *Orchestrator) ForcePanelDue() { o.panelDueAt.Store(1) }
func (o *Orchestrator) PanelDue() bool { return o.panelDueAt.Load() != 0 }

// CheckPanelName runs the panel name check now.
func (o *Orchestrator) CheckPanelName(ctx context.Context) { o.checkPanelName(ctx) }
