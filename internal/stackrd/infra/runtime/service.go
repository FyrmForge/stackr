package runtime

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
)

// NetAttach is one network a service's tasks join, with the DNS names they
// answer to on it. Attachments are part of the spec on purpose: a running
// service cannot join a network without rolling every task, which is the
// whole reason the overlays come from a pool (infra/netpool).
type NetAttach struct {
	Name    string
	Aliases []string
}

// ServiceSpec describes one swarm service. It is deliberately the same shape
// as ContainerSpec where the two overlap, so the deploy engine reads the same
// either way, ContainerSpec stays for the things that are still plain
// containers (one-shots, the volume tool, the forward relays, traefik).
//
// Three ContainerSpec fields have no swarm equivalent and are dropped with a
// warning by the caller rather than silently: Devices, ShmSizeMB and
// Privileged. Swarm's ContainerSpec has no --device, no --shm-size and no
// --privileged; its Privileges field is about AppArmor/seccomp, not root.
type ServiceSpec struct {
	Name       string
	Image      string
	Entrypoint []string // replaces the image's ENTRYPOINT (nil keeps it)
	Cmd        []string
	Env        []string
	Labels     map[string]string
	Mounts     []string // "name-or-hostpath:/container/path[:ro]"
	Ports      map[string]string
	// HostPorts publishes on the node's own interface instead of through the
	// routing mesh. The mesh SNATs, so the visitor IP is lost, traefik and
	// anything else that has to see a real client address needs this.
	HostPorts bool
	Networks  []NetAttach
	Replicas  int
	User      string

	CPULimit   float64
	MemLimitMB int

	// ShmSizeMB sizes /dev/shm. Swarm has no --shm-size, so it rides as a
	// tmpfs mount on /dev/shm, the same thing docker does under the flag.
	// Postgres parallel query dies on the 64MB default.
	ShmSizeMB int

	// Pinned tiles hold a volume: exactly one replica, the old task stopped
	// before the new one starts (two writers on one volume corrupt it), and
	// placement fixed so the task cannot land on a node with an empty disk.
	Pinned bool

	// HomeNode is the swarm node ID a pinned tile's volume lives on, turned
	// into a `node.id ==` constraint. A local volume is a directory on one
	// host's disk: lose this and swarm will happily schedule the database
	// onto an empty node with an empty volume, which is silent data loss and
	// not an error (docs/plans/30-docker-swarm.md, addendum). EnsureService
	// refuses a pinned spec without one.
	HomeNode string

	// NodeGroup constrains the task to nodes labelled stackr.group=<x>.
	// Empty means anywhere.
	NodeGroup string

	// ManagerOnly pins the task to the swarm manager by role rather than by
	// node ID. stackr's own infrastructure, traefik, the registry, the panel,
	// is placed this way: it holds the manager's disk, and it is what a
	// single-manager install means. Tiles never use it.
	ManagerOnly bool

	// Global places one task on every node instead of a replica count. The
	// node agent is the only global service (infra/agent).
	Global bool

	// Secrets are swarm secrets mounted into the task at
	// /run/secrets/<Target>, by secret name.
	Secrets []SecretMount

	// StopGraceS overrides swarm's 10s SIGTERM-to-SIGKILL window (0 = leave
	// the default).
	StopGraceS int

	// RegistryAuth is the base64 auth blob for pulling the image on whichever
	// node the task lands (the API's --with-registry-auth). Empty is fine on
	// a single node with a public or already-pulled image.
	RegistryAuth string

	// Unconfined turns off seccomp and apparmor for the task. Rootless
	// buildkit cannot start under the default profile and it is the only
	// thing that sets this. Not --privileged: swarm has none, and buildkit
	// does not need it.
	Unconfined bool

	HealthCmd          string
	HealthIntervalS    int
	HealthTimeoutS     int
	HealthRetries      int
	HealthStartPeriodS int

	// RestartPolicy is one of RestartPolicies: "" (restart on any exit, the
	// default), "on-failure", or "no" (a task that exits stays down).
	RestartPolicy string
}

// RestartPolicies are the accepted tile restart values, canonical form.
// "always" and "unless-stopped" from the container days are aliases of "".
var RestartPolicies = []string{"", "on-failure", "no"}

// NormalizeRestart maps any accepted spelling to its canonical value and
// rejects the rest. Every surface that takes the value (form, API, config)
// runs it through here so the stored column is always canonical.
func NormalizeRestart(s string) (string, error) {
	switch s {
	case "", "always", "unless-stopped":
		return "", nil
	case "on-failure", "no":
		return s, nil
	}
	return "", fmt.Errorf("restart %q must be always, on-failure or no", s)
}

func restartCondition(s string) swarm.RestartPolicyCondition {
	switch s {
	case "on-failure":
		return swarm.RestartPolicyConditionOnFailure
	case "no":
		return swarm.RestartPolicyConditionNone
	}
	return swarm.RestartPolicyConditionAny
}

// restartDelay is swarm's own default, written out so our spec matches the
// one it stores.
var restartDelay = 5 * time.Second

// serviceSpec turns our spec into swarm's.
func serviceSpec(s ServiceSpec) (swarm.ServiceSpec, error) {
	if s.Labels == nil {
		s.Labels = map[string]string{}
	}
	s.Labels[LabelManaged] = "true"

	mounts, err := parseMounts(s.Mounts)
	if err != nil {
		return swarm.ServiceSpec{}, err
	}
	ports, err := servicePorts(s.Ports, s.HostPorts)
	if err != nil {
		return swarm.ServiceSpec{}, err
	}

	if s.ShmSizeMB > 0 {
		mounts = append(mounts, mount.Mount{
			Type:         mount.TypeTmpfs,
			Target:       "/dev/shm",
			TmpfsOptions: &mount.TmpfsOptions{SizeBytes: int64(s.ShmSizeMB) << 20},
		})
	}

	cs := &swarm.ContainerSpec{
		Image:   s.Image,
		Command: s.Entrypoint,
		Args:    s.Cmd,
		Env:     s.Env,
		User:    s.User,
		// Container labels, not service labels: ListByLabel filters containers,
		// and a label set on the service never reaches the task's container.
		Labels: s.Labels,
		Mounts: mounts,
	}
	// Always set, even at the default: swarm fills its own defaults into the
	// stored spec and then decides whether to roll by diffing what it stored
	// against what it is handed. A field we leave nil reads as a change on
	// every single update, which rolls the service for nothing, on traefik
	// that is a gap on 80 and 443 every time the panel restarts.
	grace := 10 * time.Second
	if s.StopGraceS > 0 {
		grace = time.Duration(s.StopGraceS) * time.Second
	}
	cs.StopGracePeriod = &grace
	if s.Unconfined {
		cs.Privileges = &swarm.Privileges{
			Seccomp:  &swarm.SeccompOpts{Mode: swarm.SeccompModeUnconfined},
			AppArmor: &swarm.AppArmorOpts{Mode: swarm.AppArmorModeDisabled},
		}
	}
	if s.HealthCmd != "" {
		interval := 5
		if s.HealthIntervalS > 0 {
			interval = s.HealthIntervalS
		}
		cs.Healthcheck = &container.HealthConfig{
			Test:        []string{"CMD-SHELL", s.HealthCmd},
			Interval:    time.Duration(interval) * time.Second,
			Timeout:     time.Duration(s.HealthTimeoutS) * time.Second,
			Retries:     s.HealthRetries,
			StartPeriod: time.Duration(s.HealthStartPeriodS) * time.Second,
		}
	}

	var nets []swarm.NetworkAttachmentConfig
	for _, n := range s.Networks {
		if n.Name == "" {
			continue
		}
		nets = append(nets, swarm.NetworkAttachmentConfig{Target: n.Name, Aliases: n.Aliases})
	}

	replicas := uint64(max(s.Replicas, 1))
	order := swarm.UpdateOrderStartFirst
	if s.Pinned {
		replicas, order = 1, swarm.UpdateOrderStopFirst
	}

	for _, sec := range s.Secrets {
		cs.Secrets = append(cs.Secrets, &swarm.SecretReference{
			SecretID:   sec.ID,
			SecretName: sec.Name,
			File: &swarm.SecretReferenceFileTarget{
				Name: sec.Target, UID: "0", GID: "0", Mode: 0o400,
			},
		})
	}

	mode := swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &replicas}}
	if s.Global {
		mode = swarm.ServiceMode{Global: &swarm.GlobalService{}}
	}

	spec := swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: s.Name, Labels: s.Labels},
		Mode:        mode,
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: cs,
			Networks:      nets,
			Resources: &swarm.ResourceRequirements{Limits: &swarm.Limit{
				NanoCPUs:    int64(s.CPULimit * 1e9),
				MemoryBytes: int64(s.MemLimitMB) << 20,
			}},
			// Delay spelled out for the same reason as StopGracePeriod above.
			RestartPolicy: &swarm.RestartPolicy{
				Condition: restartCondition(s.RestartPolicy),
				Delay:     &restartDelay,
			},
		},
		UpdateConfig: &swarm.UpdateConfig{
			Parallelism: 1,
			Order:       order,
			// Swarm runs the rollback itself; the engine only has to notice it
			// happened and say so in the deploy log.
			FailureAction: swarm.UpdateFailureActionRollback,
			// Matches the old hand-rolled gate's "still running after 3s",
			// rounded up: a task that dies inside this window fails the update.
			Monitor: 5 * time.Second,
		},
		RollbackConfig: &swarm.UpdateConfig{
			Parallelism: 1,
			Order:       order,
			// A failed rollback must stop and stay visible, not loop.
			FailureAction: swarm.UpdateFailureActionPause,
		},
		EndpointSpec: &swarm.EndpointSpec{Mode: swarm.ResolutionModeVIP, Ports: ports},
	}
	var constraints []string
	if s.ManagerOnly {
		constraints = append(constraints, "node.role == manager")
	}
	if s.HomeNode != "" {
		// The tile's volume is on that disk and nowhere else.
		constraints = append(constraints, "node.id == "+s.HomeNode)
	} else if s.Pinned && !s.ManagerOnly {
		// No home node and pinned is the dangerous combination: swarm would
		// place it anywhere and start it on an empty volume. Refused here so
		// it cannot be created at all.
		return swarm.ServiceSpec{}, fmt.Errorf("%s holds a volume but has no home node", s.Name)
	}
	if s.NodeGroup != "" {
		constraints = append(constraints, "node.labels."+NodeGroupLabel+" == "+s.NodeGroup)
	}
	if len(constraints) > 0 {
		spec.TaskTemplate.Placement = &swarm.Placement{Constraints: constraints}
	}
	return spec, nil
}

// SecretMount is one swarm secret in a service spec. Target is the filename
// under /run/secrets.
type SecretMount struct {
	ID     string
	Name   string
	Target string
}

// parseMounts turns "source:/target[:ro]" lines into swarm mounts. An absolute
// source is a bind, anything else a named volume, the same rule docker's own
// CLI uses, and the same one ContainerSpec.Volumes relied on implicitly.
func parseMounts(lines []string) ([]mount.Mount, error) {
	var out []mount.Mount
	for _, l := range lines {
		parts := strings.Split(l, ":")
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("volume %q: want source:/target[:ro]", l)
		}
		m := mount.Mount{Source: parts[0], Target: parts[1], Type: mount.TypeVolume}
		if strings.HasPrefix(parts[0], "/") || strings.HasPrefix(parts[0], ".") {
			m.Type = mount.TypeBind
		}
		if len(parts) > 2 && strings.Contains(parts[2], "ro") {
			m.ReadOnly = true
		}
		out = append(out, m)
	}
	return out, nil
}

// servicePorts publishes through the routing mesh, so any node's IP reaches
// the task wherever it runs.
func servicePorts(ports map[string]string, hostMode bool) ([]swarm.PortConfig, error) {
	mode := swarm.PortConfigPublishModeIngress
	if hostMode {
		mode = swarm.PortConfigPublishModeHost
	}
	var out []swarm.PortConfig
	for host, cport := range ports {
		proto := "tcp"
		if p, rest, ok := strings.Cut(cport, "/"); ok {
			cport, proto = p, rest
		}
		hp, err := strconv.Atoi(strings.TrimSpace(host))
		if err != nil {
			return nil, fmt.Errorf("published port %q: %w", host, err)
		}
		cp, err := strconv.Atoi(strings.TrimSpace(cport))
		if err != nil {
			return nil, fmt.Errorf("container port %q: %w", cport, err)
		}
		out = append(out, swarm.PortConfig{
			Protocol:      swarm.PortConfigProtocol(proto),
			TargetPort:    uint32(cp),
			PublishedPort: uint32(hp),
			PublishMode:   mode,
		})
	}
	return out, nil
}

// MountSecret adds one swarm secret to an existing service, if it is not
// already mounted, and reports whether it changed anything.
//
// The panel needs this for its own service: the shared runtime key is created
// the first time a node is added, long after `docker service create stackr`
// ran, and without it every call the panel makes to a remote agent fails with
// "no node agent on this install yet". Adding the secret restarts the panel
// once and it comes back holding the key.
func (r *Runtime) MountSecret(ctx context.Context, service, secretID, secretName, target string) (bool, error) {
	changed := false
	err := r.updateService(ctx, service, swarm.ServiceUpdateOptions{}, func(spec *swarm.ServiceSpec) bool {
		cs := spec.TaskTemplate.ContainerSpec
		if cs == nil {
			return false
		}
		for _, ref := range cs.Secrets {
			if ref.SecretName == secretName {
				return false
			}
		}
		cs.Secrets = append(cs.Secrets, &swarm.SecretReference{
			SecretID:   secretID,
			SecretName: secretName,
			File: &swarm.SecretReferenceFileTarget{
				Name: target, UID: "0", GID: "0", Mode: 0o400,
			},
		})
		changed = true
		return true
	})
	if errors.Is(err, errNoService) {
		return false, nil
	}
	return changed, err
}

// updateService re-reads the service, lets mutate edit the spec, and writes it
// back, retrying when swarm rejects the version.
//
// Every ServiceUpdate carries the version index the caller read, and swarm
// refuses one that is not current, that is how it refuses a write against a
// spec someone else changed. It also bumps the version by itself while it
// schedules a service, so an update issued seconds after a create loses the
// race through nobody's fault ("update out of sequence"). Re-reading and
// re-applying is the whole fix; mutate must therefore be safe to run twice.
//
// mutate returns false when there is nothing to write. A missing service is
// not an error: a tile that never deployed has none.
func (r *Runtime) updateService(ctx context.Context, name string, opts swarm.ServiceUpdateOptions, mutate func(*swarm.ServiceSpec) bool) error {
	var lastErr error
	for attempt := range 4 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * 250 * time.Millisecond):
			}
		}
		cur, _, err := r.cli.ServiceInspectWithRaw(ctx, name, swarm.ServiceInspectOptions{})
		if err != nil {
			if cerrdefs.IsNotFound(err) {
				return errNoService
			}
			return err
		}
		spec := cur.Spec
		if !mutate(&spec) {
			return nil
		}
		if _, err := r.cli.ServiceUpdate(ctx, cur.ID, cur.Version, spec, opts); err != nil {
			if strings.Contains(err.Error(), "out of sequence") {
				lastErr = err
				continue
			}
			return err
		}
		return nil
	}
	return lastErr
}

// errNoService is updateService's "nothing there", swallowed by the callers
// for which a missing service is a normal state.
var errNoService = errors.New("service does not exist")

// EnsureService creates the service or updates it in place, reporting whether
// it had to create it. Callers use that to skip work that a fresh task has
// already done, a forced roll right after a create is both pointless and a
// race against swarm's own version bump.
func (r *Runtime) EnsureService(ctx context.Context, s ServiceSpec) (created bool, err error) {
	spec, err := serviceSpec(s)
	if err != nil {
		return false, err
	}
	if err := r.resolveNetworkIDs(ctx, &spec); err != nil {
		return false, err
	}
	opts := swarm.ServiceUpdateOptions{
		EncodedRegistryAuth: s.RegistryAuth,
		// Fall back to the auth already in the spec when this call carries
		// none, so a redeploy of a private image does not lose its credentials.
		RegistryAuthFrom: swarm.RegistryAuthFromSpec,
	}
	err = r.updateService(ctx, s.Name, opts, func(cur *swarm.ServiceSpec) bool {
		*cur = spec
		return true
	})
	if !errors.Is(err, errNoService) {
		return false, err
	}
	if _, err := r.cli.ServiceCreate(ctx, spec, swarm.ServiceCreateOptions{
		EncodedRegistryAuth: s.RegistryAuth,
	}); err != nil {
		return false, err
	}
	return true, nil
}

// resolveNetworkIDs rewrites network attachments from names to ids.
//
// Swarm accepts either and stores the id. It also decides whether to roll the
// tasks by diffing the task template it stored against the one it is handed,
// so a spec that says "stkr-net-01" where the stored one says the id reads as
// a change, and every EnsureService rolls the service even though nothing
// about it moved. For traefik that is a gap on 80 and 443 on every panel
// restart. A name with no network is left alone: swarm's own error for it is
// better than one invented here.
func (r *Runtime) resolveNetworkIDs(ctx context.Context, spec *swarm.ServiceSpec) error {
	if len(spec.TaskTemplate.Networks) == 0 {
		return nil
	}
	id, err := r.networkIDs(ctx)
	if err != nil {
		return err
	}
	for i, n := range spec.TaskTemplate.Networks {
		if v, ok := id[n.Target]; ok {
			spec.TaskTemplate.Networks[i].Target = v
		}
	}
	return nil
}

// networkIDs maps every network's name to its id.
func (r *Runtime) networkIDs(ctx context.Context) (map[string]string, error) {
	nets, err := r.cli.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		return nil, err
	}
	id := make(map[string]string, len(nets))
	for _, n := range nets {
		id[n.Name] = n.ID
	}
	return id, nil
}

// RemoveService deletes a service. Missing is not an error.
func (r *Runtime) RemoveService(ctx context.Context, name string) error {
	err := r.cli.ServiceRemove(ctx, name)
	if err != nil && cerrdefs.IsNotFound(err) {
		return nil
	}
	return err
}

// ScaleService sets a service's replica count. Zero is how a tile is stopped:
// the spec (and its volumes, ports and networks) stays, so scaling back to one
// brings the same thing back, the service equivalent of `docker stop`.
// Missing service is not an error; a tile that never deployed has none.
// liveTasks counts the tasks that could still have a process holding the
// volume. Anything not yet shut down counts: a task that is only assigned or
// preparing has no container yet, but it is about to get one, and starting the
// rsync in that window is the same race as starting it while the old container
// is still writing.
func liveTasks(ts []swarm.Task) int {
	n := 0
	for _, t := range ts {
		switch t.Status.State {
		case swarm.TaskStateRunning, swarm.TaskStateStarting,
			swarm.TaskStatePreparing, swarm.TaskStateAssigned, swarm.TaskStateAccepted:
			n++
		}
	}
	return n
}

// StopService scales a service to zero and waits for its container to actually
// exit. ScaleService returns as soon as the new spec is posted, and a database
// with dbStopGraceS (60s) of grace goes on writing for up to a minute after
// that. Both callers, a volume move and a backup, then rsync or rm -rf a
// volume a live process is still writing to, which is how a "green" backup
// ends up torn.
//
// WaitConverged is not the tool: it reports converged only against a wanted
// count above zero. This watches the tasks instead, and a task that has left
// the list is gone.
//
// A timeout is a failure, not a shrug: carrying on would do the unsafe thing
// the wait exists to prevent.
func (r *Runtime) StopService(ctx context.Context, name string) error {
	if name == "" {
		return nil
	}
	if err := r.ScaleService(ctx, name, 0); err != nil {
		return err
	}
	// Grace is 60s for databases, plus room for the kill and the container to
	// be reaped.
	deadline := time.Now().Add(3 * time.Minute)
	for {
		ts, err := r.cli.TaskList(ctx, swarm.TaskListOptions{
			Filters: filters.NewArgs(filters.Arg("service", name)),
		})
		if err != nil {
			return err
		}
		live := liveTasks(ts)
		if live == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s still has %d running task(s) after scaling to zero", name, live)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (r *Runtime) ScaleService(ctx context.Context, name string, n int) error {
	err := r.updateService(ctx, name,
		swarm.ServiceUpdateOptions{RegistryAuthFrom: swarm.RegistryAuthFromSpec},
		func(spec *swarm.ServiceSpec) bool {
			replicas := uint64(n)
			spec.Mode = swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &replicas}}
			return true
		})
	if errors.Is(err, errNoService) {
		return nil
	}
	return err
}

// RestartService brings a service back to one replica and re-rolls its tasks.
// One update, not a scale followed by a force: each ServiceUpdate carries the
// version it read, so two back-to-back calls race the first one's version bump.
// want is how many replicas a stopped service comes back with. Zero or less
// means one. It is a parameter because the spec of a stopped service says
// zero and has forgotten what it was: restarting a tile configured for three
// replicas used to silently bring back one, and nothing in the panel then said
// the count had changed.
func (r *Runtime) RestartService(ctx context.Context, name string, want int) error {
	if want < 1 {
		want = 1
	}
	return r.updateService(ctx, name,
		swarm.ServiceUpdateOptions{RegistryAuthFrom: swarm.RegistryAuthFromSpec},
		func(spec *swarm.ServiceSpec) bool {
			if spec.Mode.Replicated != nil && spec.Mode.Replicated.Replicas != nil && *spec.Mode.Replicated.Replicas == 0 {
				n := uint64(want)
				spec.Mode.Replicated.Replicas = &n
			}
			spec.TaskTemplate.ForceUpdate++
			return true
		})
}

// RollbackService puts the previous spec back. Swarm does this on its own
// when an update fails its monitor window; this is for the case swarm cannot
// see, a task stuck pulling or starting, which it will wait on forever.
func (r *Runtime) RollbackService(ctx context.Context, name string) error {
	// With Rollback set the spec is ignored, but the API still wants one.
	err := r.updateService(ctx, name,
		swarm.ServiceUpdateOptions{Rollback: "previous", RegistryAuthFrom: swarm.RegistryAuthFromSpec},
		func(*swarm.ServiceSpec) bool { return true })
	if errors.Is(err, errNoService) {
		return nil
	}
	return err
}

// ServiceTask is one running (or wanted) replica of a service. Slot is the
// stable handle, replica 2 is slot 2 for the life of the service, while
// TaskID and ContainerID change on every restart.
type ServiceTask struct {
	Slot        int
	TaskID      string
	ContainerID string
	NodeID      string
	State       string // swarm's task state: running, starting, failed, ...
	Desired     string
	Err         string
	ExitCode    int
}

// ServiceTasks lists a service's tasks whose desired state is running: the
// replicas that are meant to exist right now, which is what a slot picker and
// the canvas both want. A service that is gone has no tasks, not an error,
// that is the state between delete and the next list.
func (r *Runtime) ServiceTasks(ctx context.Context, name string) ([]ServiceTask, error) {
	return r.serviceTasks(ctx, name, true)
}

// serviceTasks lists a service's tasks. onlyRunning keeps the desired-state
// filter; a job's task ends up desired-state shutdown the moment it completes,
// so the job path has to see those too.
func (r *Runtime) serviceTasks(ctx context.Context, name string, onlyRunning bool) ([]ServiceTask, error) {
	f := filters.NewArgs(filters.Arg("service", name))
	if onlyRunning {
		f.Add("desired-state", "running")
	}
	ts, err := r.cli.TaskList(ctx, swarm.TaskListOptions{Filters: f})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]ServiceTask, 0, len(ts))
	for _, t := range ts {
		st := ServiceTask{
			Slot:    t.Slot,
			TaskID:  t.ID,
			NodeID:  t.NodeID,
			State:   string(t.Status.State),
			Desired: string(t.DesiredState),
			Err:     t.Status.Err,
		}
		if t.Status.ContainerStatus != nil {
			st.ContainerID = t.Status.ContainerStatus.ContainerID
			st.ExitCode = t.Status.ContainerStatus.ExitCode
		}
		out = append(out, st)
	}
	return out, nil
}

// ServiceState is what stackrd needs to narrate a rolling update: how many
// replicas are up out of how many are wanted, and what swarm says about the
// update itself.
type ServiceState struct {
	Wanted  int
	Running int
	Update  string // swarm's UpdateStatus.State ("" = no update recorded)
	Message string
	// UpdateStarted is when swarm began the update it is reporting, zero when
	// it has never updated this service. It is the only way to tell "the
	// update I just asked for has finished" from "a previous update finished
	// and this one has not been picked up yet", which read identically and
	// made WaitConverged return before the roll had even started.
	UpdateStarted time.Time
}

// RolledBack reports that swarm gave up on the new spec and put the old one
// back on its own. The deploy is a failure even though the service is healthy.
func (s ServiceState) RolledBack() bool {
	return s.Update == string(swarm.UpdateStateRollbackStarted) ||
		s.Update == string(swarm.UpdateStateRollbackCompleted) ||
		s.Update == string(swarm.UpdateStateRollbackPaused)
}

// Converged reports every wanted replica running and no update in flight.
func (s ServiceState) Converged() bool {
	if s.Wanted == 0 || s.Running < s.Wanted {
		return false
	}
	return s.Update == "" || s.Update == string(swarm.UpdateStateCompleted)
}

// ServiceStatus reads the service's own view plus its tasks.
func (r *Runtime) ServiceStatus(ctx context.Context, name string) (ServiceState, error) {
	svc, _, err := r.cli.ServiceInspectWithRaw(ctx, name, swarm.ServiceInspectOptions{})
	if err != nil {
		return ServiceState{}, err
	}
	var st ServiceState
	if svc.Spec.Mode.Replicated != nil && svc.Spec.Mode.Replicated.Replicas != nil {
		st.Wanted = int(*svc.Spec.Mode.Replicated.Replicas)
	}
	if svc.UpdateStatus != nil {
		st.Update, st.Message = string(svc.UpdateStatus.State), svc.UpdateStatus.Message
		if svc.UpdateStatus.StartedAt != nil {
			st.UpdateStarted = *svc.UpdateStatus.StartedAt
		}
	}
	tasks, err := r.ServiceTasks(ctx, name)
	if err != nil {
		return st, err
	}
	for _, t := range tasks {
		// Desired too, not state alone. The moment an update starts, the old
		// task keeps State "running" while its DesiredState flips to shutdown,
		// so counting by state alone reports the wanted replicas already up
		// and a deploy "converges" on the very task it is replacing, before
		// the new one has so much as pulled its image.
		if t.State == "running" && t.Desired == "running" {
			st.Running++
		} else if t.Err != "" && st.Message == "" {
			// Swarm only fills UpdateStatus.Message on an update; a task that
			// cannot start at all (bad image, no such node) says why here.
			st.Message = t.Err
		}
	}
	return st, nil
}

// ServiceLogs returns the last tail lines from every replica of a service,
// one-shot. Swarm collects them through the manager, so a task on another node
// needs nothing extra; each line is prefixed with the task name, which carries
// the slot.
func (r *Runtime) ServiceLogs(ctx context.Context, name string, tail int) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "service", "logs",
		"--tail", fmt.Sprint(tail), name).CombinedOutput()
	return string(out), err
}

// StreamServiceLogsMarked follows every replica's logs, marked the same way
// StreamLogsMarked marks a container's.
func (r *Runtime) StreamServiceLogsMarked(ctx context.Context, name string, tail int) (<-chan string, func(), error) {
	return r.streamMarked(ctx, []string{"service", "logs"}, tail, name)
}

// --- one-shots ---

// jobPoll is how often RunJob asks swarm whether the task finished. A job that
// runs for milliseconds still costs one tick; anything shorter costs the
// manager more than it saves the user.
const jobPoll = time.Second

// RunJob runs a spec once as a replicated job: one task, run to completion,
// logs collected through the manager. Swarm never removes a finished job
// service, so this does, on every path, a stale one collides with the next
// run of the same name and its task keeps showing up in task lists.
//
// Returns the task's output and a non-nil error when it exited non-zero, so
// the caller reads it exactly like a `docker run` result.
func (r *Runtime) RunJob(ctx context.Context, s ServiceSpec) (string, error) {
	spec, err := jobSpec(s)
	if err != nil {
		return "", err
	}
	// A name left over from a previous run (a panel killed mid-run) would make
	// the create fail; the run that owns the name is over either way.
	_ = r.RemoveService(ctx, s.Name)
	if _, err := r.cli.ServiceCreate(ctx, spec, swarm.ServiceCreateOptions{
		EncodedRegistryAuth: s.RegistryAuth,
	}); err != nil {
		return "", err
	}
	// Removal runs even when ctx is already dead (timeout, stopped by hand),
	// with the caller's ctx it would be a no-op and leak the service.
	defer func() {
		rmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = r.RemoveService(rmCtx, s.Name)
	}()

	tick := time.NewTicker(jobPoll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return r.jobLogs(ctx, s.Name), ctx.Err()
		case <-tick.C:
		}
		tasks, err := r.serviceTasks(ctx, s.Name, false)
		if err != nil {
			return "", err
		}
		for _, t := range tasks {
			switch t.State {
			case "complete":
				return r.jobLogs(ctx, s.Name), nil
			case "failed", "rejected", "orphaned":
				out := r.jobLogs(ctx, s.Name)
				if t.Err != "" {
					return out, fmt.Errorf("%s", t.Err)
				}
				return out, fmt.Errorf("exit status %d", t.ExitCode)
			}
		}
	}
}

// jobSpec is a service spec in replicated-job mode: one task, run once, never
// restarted. Swarm rejects an update or endpoint config on a job.
func jobSpec(s ServiceSpec) (swarm.ServiceSpec, error) {
	spec, err := serviceSpec(s)
	if err != nil {
		return swarm.ServiceSpec{}, err
	}
	one := uint64(1)
	spec.Mode = swarm.ServiceMode{ReplicatedJob: &swarm.ReplicatedJob{
		MaxConcurrent:    &one,
		TotalCompletions: &one,
	}}
	spec.TaskTemplate.RestartPolicy = &swarm.RestartPolicy{Condition: swarm.RestartPolicyConditionNone}
	spec.UpdateConfig, spec.RollbackConfig = nil, nil
	spec.EndpointSpec = nil
	return spec, nil
}

// jobLogs reads what the run printed. The logs are read before the service is
// removed and are best effort: a task that never started has none, and that
// must not replace the real error.
func (r *Runtime) jobLogs(ctx context.Context, name string) string {
	logCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(logCtx, "docker", "service", "logs",
		"--raw", "--no-task-ids", name).CombinedOutput()
	if err != nil {
		return ""
	}
	return string(out)
}

// DetachServiceNetwork removes a network from a service's spec, reporting
// whether it changed anything. The mirror of AttachServiceNetworks, and the
// only way a swarm service leaves a network: NetworkDisconnect on the task's
// container is undone within seconds, because the attachment lives in the
// service spec and the orchestrator puts it back.
//
// This is what makes a pooled network safe to recycle. Without it the pool
// hands the name to the next environment with the previous tenant's service
// still on it, and two environments that should not see each other can reach
// each other by DNS.
func (r *Runtime) DetachServiceNetwork(ctx context.Context, name, netName string) (bool, error) {
	if name == "" || netName == "" {
		return false, nil
	}
	// By id and by name: swarm stores the id in the spec, but a network that
	// has already been removed is not in the id map at all and only the name
	// is left to match on.
	targets := map[string]bool{netName: true}
	if byName, err := r.networkIDs(ctx); err == nil {
		if id, ok := byName[netName]; ok {
			targets[id] = true
		}
	}
	removed := false
	err := r.updateService(ctx, name,
		swarm.ServiceUpdateOptions{RegistryAuthFrom: swarm.RegistryAuthFromSpec},
		func(spec *swarm.ServiceSpec) bool {
			// Recomputed on every attempt: a retry re-reads the spec.
			kept := make([]swarm.NetworkAttachmentConfig, 0, len(spec.TaskTemplate.Networks))
			removed = false
			for _, cur := range spec.TaskTemplate.Networks {
				if targets[cur.Target] {
					removed = true
					continue
				}
				kept = append(kept, cur)
			}
			spec.TaskTemplate.Networks = kept
			return removed
		})
	if errors.Is(err, errNoService) {
		return false, nil
	}
	return removed, err
}

// mergeAliases adds the missing entries of want to have, reporting whether
// anything changed. Order is kept so an unchanged set produces a byte-identical
// spec and swarm finds nothing to roll.
func mergeAliases(have, want []string) ([]string, bool) {
	if len(want) == 0 {
		return have, false
	}
	seen := make(map[string]bool, len(have))
	for _, a := range have {
		seen[a] = true
	}
	grew := false
	for _, a := range want {
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		have = append(have, a)
		grew = true
	}
	return have, grew
}

// AttachServiceNetwork adds a network to a service's spec if it is not already
// there, reporting whether it changed anything.
func (r *Runtime) AttachServiceNetwork(ctx context.Context, name string, n NetAttach) (bool, error) {
	return r.AttachServiceNetworks(ctx, name, []NetAttach{n})
}

// AttachServiceNetworks adds every missing network in one spec update,
// reporting whether it changed anything. A running service cannot join a
// network the way a container can: the attachment is part of the task spec,
// so the update rolls every task, once for the whole batch, which is why
// pool growth adds all the new overlays together rather than one at a time.
// Callers holding a container id must re-read it after a true, and a service
// already on every network must cost nothing, which is what makes this safe
// to call on every reconcile.
func (r *Runtime) AttachServiceNetworks(ctx context.Context, name string, ns []NetAttach) (bool, error) {
	if name == "" {
		return false, nil
	}
	// By id, not by name: swarm stores the id, so a name-keyed comparison
	// matches nothing and re-appends every network on every call. Left
	// unfixed that grows the spec without bound and rolls the service every
	// time, on traefik, a gap on 80 and 443 on every panel restart.
	want := make([]NetAttach, 0, len(ns))
	byName, err := r.networkIDs(ctx)
	if err != nil {
		return false, err
	}
	for _, n := range ns {
		if id, ok := byName[n.Name]; ok {
			n.Name = id
		}
		want = append(want, n)
	}
	ns = want
	added := false
	err = r.updateService(ctx, name,
		swarm.ServiceUpdateOptions{RegistryAuthFrom: swarm.RegistryAuthFromSpec},
		func(spec *swarm.ServiceSpec) bool {
			// Recomputed on every attempt, not carried over: a retry re-reads
			// the spec, and the networks it is missing may not be the ones the
			// first read was missing.
			at := map[string]int{}
			for i, cur := range spec.TaskTemplate.Networks {
				at[cur.Target] = i
			}
			added = false
			for _, n := range ns {
				if n.Name == "" {
					continue
				}
				i, present := at[n.Name]
				if !present {
					at[n.Name] = len(spec.TaskTemplate.Networks)
					added = true
					spec.TaskTemplate.Networks = append(spec.TaskTemplate.Networks,
						swarm.NetworkAttachmentConfig{Target: n.Name, Aliases: n.Aliases})
					continue
				}
				// Already on the network, but perhaps without the aliases this
				// call is asking for, and then it is not attached the way the
				// caller needs. A shared database attached by the deploy engine
				// with no aliases, then attached again by the provisioner with
				// its tile alias, used to keep the engine's alias-less entry:
				// the name never resolved, so the panel's own DNS answered with
				// whatever else was on that overlay and every s3 slice failed
				// with "connection refused".
				if merged, grew := mergeAliases(spec.TaskTemplate.Networks[i].Aliases, n.Aliases); grew {
					spec.TaskTemplate.Networks[i].Aliases = merged
					added = true
					// Swarm accepts an alias-only change and reports the
					// update completed without rolling the task, so the
					// running container keeps its old attachment and the new
					// name resolves to nothing. ForceUpdate makes it a real
					// roll, which is the only way the alias starts working.
					spec.TaskTemplate.ForceUpdate++
				}
			}
			return added
		})
	if errors.Is(err, errNoService) {
		return false, nil
	}
	return added && err == nil, err
}

// WaitConverged blocks until the service has its replicas up and no update in
// flight, or the timeout passes. Callers that read a task's container or its
// addresses right after a roll need this: the old task is still listed while
// the new one starts.
func (r *Runtime) WaitConverged(ctx context.Context, name string, timeout time.Duration) error {
	return r.WaitRolled(ctx, name, time.Time{}, timeout)
}

// WaitRolled waits for a service to settle on an update that swarm began at or
// after since. Pass the time just before posting the update; a zero time means
// "settled at all", which is WaitConverged.
//
// The baseline is the whole point. Right after a spec update swarm may still
// be reporting the previous update as completed with the old task running and
// wanted, which Converged reads as settled. A caller that then execs into "the"
// container gets the one that is about to be killed: seen on the rig as
// "the database system is shutting down" in the middle of cutting a slice.
func (r *Runtime) WaitRolled(ctx context.Context, name string, since time.Time, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		st, err := r.ServiceStatus(ctx, name)
		if err == nil && st.Converged() && !st.UpdateStarted.Before(since) {
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return err
			}
			return fmt.Errorf("%s did not settle within %s: %s", name, timeout, st.Message)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// ListServiceNames returns every swarm service on this cluster, by name,
// stackr's own and anyone else's. Read-only: the one caller is agent.Running,
// which asks whether the agent service exists at all. Anything that acts on
// the list must filter on LabelManaged first.
func (r *Runtime) ListServiceNames(ctx context.Context) ([]string, error) {
	svcs, err := r.cli.ServiceList(ctx, swarm.ServiceListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(svcs))
	for _, s := range svcs {
		out = append(out, s.Spec.Name)
	}
	return out, nil
}
