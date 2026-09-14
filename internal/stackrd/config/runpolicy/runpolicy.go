// Package runpolicy is the registry of runnable tile types: how each type's
// built artifact runs, and which config surface it accepts. It is the
// runnable-side twin of the managed-engine registry (internal/databases),
// adding a future type (a scale-to-zero function, say) means adding an entry
// here, not another scatter of `if kind == "cron"` branches.
//
// Every runnable type shares the same base: a source (git build / image) the
// deploy engine turns into an artifact, env/refs, and limits.
// The policy is only what differs: lifecycle and allowed keys.
package runpolicy

// Policy describes one runnable tile type.
type Policy struct {
	// KeepAlive: the deploy engine starts (and health-gates) a long-lived
	// container after the build. False = the build stops at the artifact;
	// running it is someone else's trigger (the cron scheduler today).
	KeepAlive bool
	// RequiresSchedule gates the schedule: key (and its validation).
	RequiresSchedule bool
	// AllowsCommand gates command:, a cron's one-shot override (run as
	// `sh -c`) or a service's CMD override (argv, appended to the entrypoint).
	AllowsCommand bool
	// AllowsIngress gates port: and domains:, only a keep-alive artifact
	// has an endpoint to route to.
	AllowsIngress bool
}

// Policies is the registry, keyed by tile kind / config type.
var Policies = map[string]Policy{
	"service": {KeepAlive: true, AllowsIngress: true, AllowsCommand: true},
	"cron":    {RequiresSchedule: true, AllowsCommand: true},
	// A function is a cron minus the schedule: run-to-completion, triggered
	// manually or by its own deploy finishing (Tile.RunOnDeploy).
	"function": {AllowsCommand: true},
}

// For returns the policy for a runnable kind; ok is false for kinds that are
// not runnable (managed, volume, slice).
func For(kind string) (Policy, bool) {
	p, ok := Policies[kind]
	return p, ok
}
