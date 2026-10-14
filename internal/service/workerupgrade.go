package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/worker"
)

// Worker upgrades: after the panel upgrades itself, the admin's click rolls
// every worker onto the panel's build, one at a time. The swap runs on the
// node, in a host helper container (hostHelperSpec) that replaces the
// worker's binaries from the panel image and restarts the service; it puts
// the old ones back by itself if the controller never removes it.

// Settings keys (rows of the settings table, not catalogue knobs).
const (
	keyPanelVersion     = "panel_version"      // the build that last booted
	keyPanelPrevVersion = "panel_prev_version" // the one before it: workers on it are still allowed
	keyRolloutTarget    = "worker_rollout_target"
	keyRolloutStopped   = "worker_rollout_stopped" // a failure message; empty = not stopped
	keyUpgradePrefix    = "worker_upgrade."        // + node id: a nodeUpgrade as JSON
)

// Per-node rollout states.
const (
	stateWaiting   = "waiting"
	stateUpgrading = "upgrading"
	stateDone      = "done"
	stateFailed    = "failed"
	stateSkipped   = "skipped"
)

const (
	// kindWorkerRollout is the rollout job; its handler is runWorkerRollout
	// and its lock is rolloutLock.
	kindWorkerRollout jobs.Kind = "worker-rollout"

	rolloutLock = "worker-rollout"

	// hostHelperName is the one helper container a node may have.
	hostHelperName = "stackr-host-helper"
	// helperDeadline is the helper's own rollback deadline, counted from its
	// restart of the worker; the controller gives up sooner (rolloutWait) so
	// the two never race.
	helperDeadline = 150 * time.Second
	rolloutWait    = 120 * time.Second
	// rolloutExitWait is how long a failed node's helper may take to finish
	// its rollback: the rest of its deadline plus the restart.
	rolloutExitWait = 90 * time.Second
	rolloutPoll     = 5 * time.Second
)

// hostHelperSpec is the host helper: the panel image on a node with the
// host's pid and network namespaces and its / at /host, running one of
// `stackrd worker-swap` or `stackrd host-run`. The image's entrypoint is
// stackrd, so cmd starts with the subcommand.
func hostHelperSpec(image string, cmd ...string) docker.ContainerSpec {
	return docker.ContainerSpec{
		Name: hostHelperName, Image: image, Cmd: cmd,
		Labels:     map[string]string{installspec.LabelRole: "host-helper"},
		Privileged: true, PidHost: true, HostNetwork: true,
		Volumes: []string{"/:/host"}, Restart: "no",
	}
}

// sameVersion compares builds ignoring the leading v of a release tag.
func sameVersion(a, b string) bool { return strings.TrimPrefix(a, "v") == strings.TrimPrefix(b, "v") }

// nodeUpgrade is one node's rollout row, kept as JSON under
// worker_upgrade.<node id>.
type nodeUpgrade struct {
	Version string    `json:"version"`
	State   string    `json:"state"`
	Error   string    `json:"error"`
	At      time.Time `json:"at"`
}

func (o *Orchestrator) upgradeOf(ctx context.Context, id string) nodeUpgrade {
	var u nodeUpgrade
	if v, ok, _ := o.store.Settings.Get(ctx, keyUpgradePrefix+id); ok {
		_ = json.Unmarshal([]byte(v), &u)
	}
	return u
}

func (o *Orchestrator) setUpgrade(ctx context.Context, id string, u nodeUpgrade) {
	if u.At.IsZero() {
		u.At = time.Now().UTC()
	}
	b, _ := json.Marshal(u)
	if err := o.store.Settings.Set(ctx, keyUpgradePrefix+id, string(b)); err != nil {
		slog.Warn("worker upgrade: state not saved", "node", id, "err", err)
	}
}

// recordPanelVersion runs at boot: when the build differs from the one
// recorded, the old one becomes panel_prev_version, the only older build a
// worker may still be on.
func (o *Orchestrator) recordPanelVersion(ctx context.Context) error {
	if o.cfg.Version == "" {
		return nil
	}
	cur, _, err := o.store.Settings.Get(ctx, keyPanelVersion)
	if err != nil || cur == o.cfg.Version {
		return err
	}
	if cur != "" {
		if err := o.store.Settings.Set(ctx, keyPanelPrevVersion, cur); err != nil {
			return err
		}
	}
	return o.store.Settings.Set(ctx, keyPanelVersion, o.cfg.Version)
}

// workerBehind: a worker whose reported build is not the panel's. Unknown
// facts are not behind (nothing is known); an empty version, from a worker
// older than the version report, is.
func (o *Orchestrator) workerBehind(n store.Node, f *worker.Facts) bool {
	return n.Role == node.Worker && f != nil && !sameVersion(f.Version, o.cfg.Version)
}

// ---- triggers ----

// setRolloutTarget records the build an admin is rolling workers to.
func (o *Orchestrator) setRolloutTarget(ctx context.Context, version string) error {
	return o.store.Settings.Set(ctx, keyRolloutTarget, version)
}

// rolloutActive: a rollout job is queued, running or parked. A second one
// must not be enqueued over it: a newer job with the same lock cancels the
// running one, which would strand a node mid-swap.
func (o *Orchestrator) rolloutActive(ctx context.Context) bool {
	js, err := o.store.Jobs.ListByState(ctx, "queued", "running", "waiting")
	return err == nil && slices.ContainsFunc(js, func(j store.Job) bool { return j.Kind == string(kindWorkerRollout) })
}

func (o *Orchestrator) enqueueRollout(ctx context.Context) (Job, error) {
	return o.enqueue(ctx, kindWorkerRollout, struct{}{}, rolloutLock)
}

// rolloutWanted: the admin's target is this build and the rollout has not
// stopped on a failure (Retry clears that).
func (o *Orchestrator) rolloutWanted(ctx context.Context) bool {
	target, _, _ := o.store.Settings.Get(ctx, keyRolloutTarget)
	stopped, _, _ := o.store.Settings.Get(ctx, keyRolloutStopped)
	return target != "" && sameVersion(target, o.cfg.Version) && stopped == ""
}

// rolloutAfterBoot queues the rollout the admin asked for before the panel
// restarted into the target build.
func (o *Orchestrator) rolloutAfterBoot(ctx context.Context) {
	if !o.rolloutWanted(ctx) || o.rolloutActive(ctx) {
		return
	}
	if _, err := o.enqueueRollout(ctx); err != nil {
		slog.Warn("worker upgrade: rollout not queued", "err", err)
	}
}

// workerBack is the reconnect hook: a worker that came online with facts f
// is upgraded when a rollout is wanted and it is behind.
func (o *Orchestrator) workerBack(ctx context.Context, n store.Node, f *worker.Facts) {
	if o.workerBehind(n, f) {
		o.rolloutAfterBoot(ctx)
	}
}

// UpdateWorkers (server admin) is the Update tab's button, and Retry: it
// clears a stop, makes this build the target and queues the rollout.
func (o *Orchestrator) UpdateWorkers(ctx context.Context, _ *Principal) (Job, error) {
	if o.rolloutActive(ctx) {
		return Job{}, errs.Conflictf("A worker rollout is already running.")
	}
	if err := o.store.Settings.Delete(ctx, keyRolloutStopped); err != nil {
		return Job{}, err
	}
	if err := o.setRolloutTarget(ctx, o.cfg.Version); err != nil {
		return Job{}, err
	}
	return o.enqueueRollout(ctx)
}

// ---- the Update tab's view ----

// NodeUpgrade is one worker in the rollout.
type NodeUpgrade struct {
	NodeID  string    `json:"node_id"`
	Name    string    `json:"name"`
	Current string    `json:"current"` // the build it reports, "" when unknown
	Behind  bool      `json:"behind"`
	Version string    `json:"version"` // the build the last rollout aimed at
	State   string    `json:"state"`   // waiting | upgrading | done | failed | skipped, "" = never rolled
	Error   string    `json:"error"`
	At      time.Time `json:"at"`
}

// Rollout is where the worker rollout stands.
type Rollout struct {
	Target  string        `json:"target"`
	Stopped string        `json:"stopped"` // "<node> failed to upgrade, rolled back"; "" = not stopped
	Nodes   []NodeUpgrade `json:"nodes"`
}

// WorkerRollout (server admin) lists every worker with its rollout state.
func (o *Orchestrator) WorkerRollout(ctx context.Context) (Rollout, error) {
	var out Rollout
	out.Target, _, _ = o.store.Settings.Get(ctx, keyRolloutTarget)
	out.Stopped, _, _ = o.store.Settings.Get(ctx, keyRolloutStopped)
	ns, err := o.workerNodes(ctx)
	if err != nil {
		return out, err
	}
	for _, n := range rolloutOrder(ns, o.rolloutHops(ctx)) {
		u := o.upgradeOf(ctx, n.ID)
		row := NodeUpgrade{NodeID: n.ID, Name: n.Name, Version: u.Version, State: u.State, Error: u.Error, At: u.At}
		if f := o.reportedFacts(ctx, n); f != nil {
			row.Current, row.Behind = f.Version, o.workerBehind(n, f)
		}
		if row.State == stateDone && row.Behind {
			row.State = "" // done for an older build: the panel moved on, it is behind again
		}
		out.Nodes = append(out.Nodes, row)
	}
	return out, nil
}

func (o *Orchestrator) workerNodes(ctx context.Context) ([]store.Node, error) {
	ns, err := o.store.Nodes.List(ctx)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(ns, func(n store.Node) bool { return n.Role != node.Worker }), nil
}

// ---- the job ----

// rolloutOrder: nodes others are reached through (hops) go last, so the
// upgrade of h3 runs while h4, its way out, is still up; then by name.
func rolloutOrder(ns []store.Node, hops map[string]bool) []store.Node {
	out := slices.Clone(ns)
	slices.SortFunc(out, func(a, b store.Node) int {
		if hops[a.ID] != hops[b.ID] {
			if hops[a.ID] {
				return 1
			}
			return -1
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out
}

// rolloutHops are the nodes some pair of nodes is routed through.
func (o *Orchestrator) rolloutHops(ctx context.Context) map[string]bool {
	if o.mesh == nil {
		return nil
	}
	ns, err := o.store.Nodes.List(ctx)
	if err != nil {
		return nil
	}
	hops := map[string]bool{}
	for _, a := range ns {
		for _, b := range ns {
			if a.ID != b.ID {
				if h := o.mesh.Via(a.ID, b.ID); h != "" {
					hops[h] = true
				}
			}
		}
	}
	return hops
}

// rollout is the job body with its effects as fields, so a test swaps them.
type rollout struct {
	o      *Orchestrator
	docker Docker // the router; every call names its node
	// facts asks a node over the tunnel (an answer means the tunnel is up).
	facts func(ctx context.Context, id string) (*worker.Facts, error)
	// image is the stackr image ref every node pulls from the cluster registry.
	image func(ctx context.Context) (string, error)
	hops  func(ctx context.Context) map[string]bool
	// allowOld lets the pull and the helper reach a worker that is too old
	// for ordinary calls.
	allowOld func(ctx context.Context) context.Context
	arch     string // the controller's: the image is built for it
	// interval, wait and exitWait: the version poll, how long a node has to
	// report the new build, and how long its helper has to finish rolling back.
	interval, wait, exitWait time.Duration
	log                      io.Writer
}

func (o *Orchestrator) newRollout(log io.Writer) rollout {
	return rollout{
		o: o, docker: o.docker, log: log,
		facts: func(ctx context.Context, id string) (*worker.Facts, error) {
			cl, err := o.nodeExtras(id)
			if err != nil {
				return nil, err
			}
			ctx, cancel := context.WithTimeout(ctx, factsWait)
			defer cancel()
			f, err := cl.Facts(ctx)
			return &f, err
		},
		image: func(ctx context.Context) (string, error) {
			if err := o.waitStackrPublished(ctx); err != nil {
				return "", err
			}
			img, err := o.controllerStackrImage(ctx)
			if err != nil {
				return "", err
			}
			return stackrSystemRef(img), nil
		},
		hops:     o.rolloutHops,
		allowOld: allowOld,
		// ponytail: the stackr image is built for the controller's CPU type only.
		arch:     runtime.GOARCH,
		interval: rolloutPoll, wait: rolloutWait, exitWait: rolloutExitWait,
	}
}

// runWorkerRollout is the job body of kindWorkerRollout (registered in jobs.go).
func (o *Orchestrator) runWorkerRollout(ctx context.Context, r *jobs.Run) error {
	if o.cfg.FakeHost != "" {
		_, _ = fmt.Fprintln(r.Log, "fake workers are not upgraded")
		return nil
	}
	return o.newRollout(r.Log).run(ctx)
}

func (ro rollout) say(format string, a ...any) { _, _ = fmt.Fprintf(ro.log, format+"\n", a...) }

// run upgrades the workers one at a time. A failure stops it: the failed
// node is rolled back, the later ones stay waiting, and worker_rollout_stopped
// says why until Retry clears it.
func (ro rollout) run(ctx context.Context) error {
	ns, err := ro.o.workerNodes(ctx)
	if err != nil {
		return err
	}
	ns = rolloutOrder(ns, ro.hops(ctx))
	target := ro.o.cfg.Version
	for _, n := range ns {
		ro.o.setUpgrade(ctx, n.ID, nodeUpgrade{Version: target, State: stateWaiting})
	}
	for _, n := range ns {
		if err := ro.one(ctx, n, target); err != nil {
			return err
		}
	}
	return nil
}

func (ro rollout) one(ctx context.Context, n store.Node, target string) error {
	set := func(state, msg string) {
		ro.o.setUpgrade(ctx, n.ID, nodeUpgrade{Version: target, State: state, Error: msg})
		if msg != "" {
			ro.say("%s: %s: %s", n.Name, state, msg)
		} else {
			ro.say("%s: %s", n.Name, state)
		}
	}
	// stop ends the rollout on this node: rolledBack says the helper ran.
	stop := func(msg string, rolledBack bool) error {
		set(stateFailed, msg)
		why := n.Name + " failed to upgrade"
		if rolledBack {
			why += ", rolled back"
		}
		if err := ro.o.store.Settings.Set(ctx, keyRolloutStopped, why); err != nil {
			slog.Warn("worker upgrade: stop not saved", "err", err)
		}
		return errors.New(why)
	}

	f, err := ro.facts(ctx, n.ID)
	switch {
	case n.Status != node.StatusOnline || err != nil:
		set(stateSkipped, "offline, upgrades when it reconnects")
		return nil
	case f.Arch != "" && f.Arch != ro.arch:
		set(stateSkipped, fmt.Sprintf("%s is %s; the stackr image is %s only", n.Name, f.Arch, ro.arch))
		return nil
	case sameVersion(f.Version, target):
		set(stateDone, "")
		return nil
	}

	set(stateUpgrading, "")
	img, err := ro.image(ctx)
	if err != nil {
		return stop("the stackr image is not ready: "+err.Error(), false)
	}
	nctx := docker.WithNode(ro.allowOld(ctx), n.ID)
	if err := ro.docker.Pull(nctx, img, "", ro.log); err != nil {
		return stop("pulling "+img+": "+err.Error(), false)
	}
	if err := ro.removeHelpers(nctx); err != nil {
		return stop("removing an old helper: "+err.Error(), false)
	}
	id, err := ro.docker.Run(nctx, hostHelperSpec(img, "worker-swap", "--deadline", fmt.Sprintf("%ds", int(helperDeadline.Seconds()))))
	if err != nil {
		return stop("starting the helper: "+err.Error(), false)
	}

	confirmed, err := ro.waitVersion(ctx, n.ID, target)
	if err != nil {
		return err // cancelled: the helper rolls back by itself at its deadline
	}
	if confirmed {
		// The removal is the commit. If it fails the helper would roll the
		// node back at its deadline, so it is retried and, failing that, reported.
		if err := ro.remove(nctx, id); err != nil {
			return stop("the helper could not be removed, so the node rolls back: "+err.Error(), true)
		}
		set(stateDone, "")
		return nil
	}
	// Not back in time: the helper restores the old binaries and exits.
	wctx, cancel := context.WithTimeout(nctx, ro.exitWait)
	defer cancel()
	if _, err := ro.docker.Wait(wctx, id); err != nil {
		ro.say("%s: waiting for the helper: %v", n.Name, err)
	}
	_ = ro.docker.StopRemove(context.WithoutCancel(nctx), id)
	return stop("did not report "+target+" in time", true)
}

// waitVersion polls the node's facts until it reports target. false means
// the wait ran out; the error is the job's own cancellation.
func (ro rollout) waitVersion(ctx context.Context, id, target string) (bool, error) {
	deadline := time.NewTimer(ro.wait)
	defer deadline.Stop()
	tick := time.NewTicker(ro.interval)
	defer tick.Stop()
	for {
		if f, err := ro.facts(ctx, id); err == nil && sameVersion(f.Version, target) {
			return true, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-deadline.C:
			return false, nil
		case <-tick.C:
		}
	}
}

// removeHelpers removes any helper left on the node by an earlier run.
func (ro rollout) removeHelpers(nctx context.Context) error {
	cs, err := ro.docker.List(nctx, map[string]string{installspec.LabelRole: "host-helper"})
	if err != nil {
		return err
	}
	for _, c := range cs {
		if err := ro.docker.StopRemove(nctx, c.ID); err != nil {
			return err
		}
	}
	return nil
}

// remove is StopRemove with a few tries.
func (ro rollout) remove(nctx context.Context, id string) (err error) {
	for range 3 {
		if err = ro.docker.StopRemove(nctx, id); err == nil {
			return nil
		}
		time.Sleep(ro.interval)
	}
	return err
}
