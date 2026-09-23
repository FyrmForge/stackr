// Package deploystate holds the deployment status vocabulary shared by the
// daemon, the CLI and the config applier.
//
// It lives outside internal/stackrd because the CLI deliberately imports no
// stackrd package (it talks to the daemon over HTTP) and infra/ may not reach
// up into the service layer, yet all three have to agree on which statuses
// mean "still going to happen". Four hand-written copies of that set had
// already drifted apart before this package existed.
package deploystate

// The status strings written to deployments.status.
const (
	// WaitingCI is a push held back by wait_for_ci: the CI gate releases or
	// fails it once the commit's checks settle. It survives a restart —
	// cigate.Run re-adopts exactly this status on boot, and the boot sweep
	// deliberately leaves it alone.
	WaitingCI = "waiting_ci"
	Queued    = "queued"
	Running   = "running"
	Done      = "done"
	// Error is an interrupted or failed run. Cancelled is a user or a
	// supersede. Two spellings on purpose: they are different events.
	Error     = "error"
	Cancelled = "cancelled"
)

// IsLive reports whether a deployment is still going to happen — it is
// queued, rolling out, or parked waiting on CI.
//
// A parked row counts as live. A poll or an SSE stream that ended on
// waiting_ci left the browser showing a dead badge for a deploy that was
// about to start on its own.
func IsLive(status string) bool {
	switch status {
	case Queued, Running, WaitingCI:
		return true
	}
	return false
}

// IsTerminal reports whether a deployment has stopped for good. It is the
// exact complement of IsLive over the statuses that are ever written.
func IsTerminal(status string) bool { return !IsLive(status) }

// IsCancellable reports whether a deployment can be cancelled by writing the
// row. A running deploy is cancelled through its context instead, which the
// engine handles before it reaches this.
func IsCancellable(status string) bool { return status == Queued || status == WaitingCI }
