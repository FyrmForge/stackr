package tile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Labels this leaf stamps on what it runs. LabelSystem marks the panel and
// proxy containers (stackrd and the installer set it); the guard below reads it.
const (
	LabelTile   = "stackr.tile"
	LabelRole   = "stackr.role" // replica | pause | run
	LabelRun    = "stackr.run"  // a run container's run id
	LabelSystem = "stackr.system"

	// PauseImage holds the VIP: a do-nothing container whose only job is an
	// IP on the env network with the tile's slug as its alias.
	// ponytail: pinned tag; the flow pulls it like any image.
	PauseImage = "registry.k8s.io/pause:3.10"
)

// Docker is the slice of the wrapper this leaf needs.
type Docker interface {
	Run(ctx context.Context, spec docker.ContainerSpec) (string, error)
	Stop(ctx context.Context, id string) error
	Restart(ctx context.Context, id string) error
	StopRemove(ctx context.Context, id string) error
	List(ctx context.Context, labels map[string]string) ([]docker.Container, error)
	Inspect(ctx context.Context, id string) (docker.Detail, error)
	Logs(ctx context.Context, id string, tail int) (string, error)
	Exec(ctx context.Context, id string, cmd []string) (string, error)
	ExecStream(ctx context.Context, id string, cmd []string, stdin io.Reader) (io.Reader, func() error, error)
	Start(ctx context.Context, id string) error
	Pause(ctx context.Context, id string) error
	Unpause(ctx context.Context, id string) error
	StreamLogs(ctx context.Context, id string, tail int) (<-chan string, func(), error)
	Wait(ctx context.Context, id string) (int, error)
}

// VIP is the slice of internal/vip this leaf needs.
type VIP interface {
	Set(ctx context.Context, vip string, replicas []string) error
	Remove(ctx context.Context, vip string) error
}

// Gate is the health gate's timing: poll every Poll; a container with no
// HEALTHCHECK passes once running for Grace; give up after Deadline plus the
// tile's start period.
type Gate struct{ Poll, Grace, Deadline time.Duration }

var DefaultGate = Gate{Poll: time.Second, Grace: 10 * time.Second, Deadline: 60 * time.Second}

// Start runs one replica from a resolved spec and gates it. A replica that
// fails the gate is removed and the error returned; whatever ran before
// keeps running. The VIP is not touched: the flow calls Route when the
// rollout says so.
func (l *Leaf) Start(ctx context.Context, t store.Tile, spec docker.ContainerSpec) (string, error) {
	spec.Labels = maps.Clone(spec.Labels)
	if spec.Labels == nil {
		spec.Labels = map[string]string{}
	}
	spec.Labels[LabelTile], spec.Labels[LabelRole] = t.ID, "replica"
	id, err := l.docker.Run(ctx, spec)
	if err != nil {
		return "", err
	}
	if err := l.gate(ctx, id, time.Duration(t.HealthcheckStartPeriodS)*time.Second); err != nil {
		_ = l.docker.StopRemove(context.WithoutCancel(ctx), id)
		return "", fmt.Errorf("%s: %w", t.Slug, err)
	}
	return id, nil
}

// RunOnce runs one run-to-completion container: no health gate, no restart,
// its log lines copied into log. It waits for the exit and removes the
// container on every path (docker run --rm). ctx ending (timeout, cancel)
// stops it and returns ctx.Err().
func (l *Leaf) RunOnce(
	ctx context.Context,
	t store.Tile,
	spec docker.ContainerSpec,
	runID string,
	log io.Writer,
) (int, error) {
	spec.Labels = maps.Clone(spec.Labels)
	if spec.Labels == nil {
		spec.Labels = map[string]string{}
	}
	spec.Labels[LabelTile], spec.Labels[LabelRole], spec.Labels[LabelRun] = t.ID, "run", runID
	spec.Restart = "no"
	id, err := l.docker.Run(ctx, spec)
	if err != nil {
		return -1, err
	}
	defer func() { _ = l.docker.StopRemove(context.WithoutCancel(ctx), id) }()
	lines, stop, err := l.docker.StreamLogs(context.WithoutCancel(ctx), id, -1)
	if err != nil {
		return -1, err
	}
	defer stop()
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		for s := range lines {
			_, _ = io.WriteString(log, s+"\n")
		}
	}()
	code, err := l.docker.Wait(ctx, id)
	if ctx.Err() != nil {
		return -1, ctx.Err()
	}
	select { // the stream ends with the container; don't hang on a stuck one
	case <-copied:
	case <-time.After(5 * time.Second):
	}
	return code, err
}

// gate is docker.Healthy with the tile's timings; the start period is
// added to the deadline.
func (l *Leaf) gate(ctx context.Context, id string, startPeriod time.Duration) error {
	return docker.Healthy(ctx, l.docker.Inspect, id, l.Gate.Poll, l.Gate.Grace, l.Gate.Deadline+startPeriod)
}

// Replicas are the tile's replica containers, any state.
func (l *Leaf) Replicas(ctx context.Context, t store.Tile) ([]docker.Container, error) {
	return l.docker.List(ctx, map[string]string{LabelTile: t.ID, LabelRole: "replica"})
}

// Addresses maps every IP of every running tile container (replicas, runs
// and the pause container that holds the VIP) to its tile id.
func (l *Leaf) Addresses(ctx context.Context) (map[string]string, error) {
	cs, err := l.docker.List(ctx, map[string]string{docker.LabelManaged: "true"})
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, c := range cs {
		if id := c.Labels[LabelTile]; id != "" && c.State == "running" {
			for _, ip := range c.IPs {
				out[ip] = id
			}
		}
	}
	return out, nil
}

// Pause ensures the tile's pause container on network and returns its IP,
// which is the tile's VIP.
func (l *Leaf) Pause(ctx context.Context, t store.Tile, network string) (string, error) {
	cs, err := l.docker.List(ctx, map[string]string{LabelTile: t.ID, LabelRole: "pause"})
	if err != nil {
		return "", err
	}
	var id string
	if len(cs) > 0 {
		id = cs[0].ID
	} else {
		id, err = l.docker.Run(ctx, docker.ContainerSpec{
			Name:     "stackr-pause-" + t.ID,
			Image:    PauseImage,
			Restart:  "always",
			Labels:   map[string]string{LabelTile: t.ID, LabelRole: "pause"},
			Networks: []docker.NetAttach{{Name: network, Aliases: []string{t.Slug}}},
		})
		if err != nil {
			return "", err
		}
	}
	d, err := l.docker.Inspect(ctx, id)
	if err != nil {
		return "", err
	}
	if d.Networks[network] == "" {
		return "", fmt.Errorf("%s: the pause container has no address on %s", t.Slug, network)
	}
	return d.Networks[network], nil
}

// Route points the VIP at the running replicas' addresses on network.
// ponytail: stackrd rebuilds every VIP on boot (vip.Rebuild) from these same
// reads; that loop is the orchestrator's, not here.
func (l *Leaf) Route(ctx context.Context, t store.Tile, network string) error {
	ip, err := l.Pause(ctx, t, network)
	if err != nil {
		return err
	}
	cs, err := l.Replicas(ctx, t)
	if err != nil {
		return err
	}
	var ips []string
	for _, c := range cs {
		d, err := l.docker.Inspect(ctx, c.ID)
		if err != nil {
			return err
		}
		if d.Running && d.Networks[network] != "" {
			ips = append(ips, d.Networks[network])
		}
	}
	if len(ips) == 0 {
		return l.vip.Remove(ctx, ip)
	}
	return l.vip.Set(ctx, ip, ips)
}

// Teardown removes every container of the tile and its VIP rules.
func (l *Leaf) Teardown(ctx context.Context, t store.Tile, network string) error {
	cs, err := l.docker.List(ctx, map[string]string{LabelTile: t.ID})
	if err != nil {
		return err
	}
	for _, c := range cs {
		if c.Labels[LabelRole] == "pause" {
			if d, err := l.docker.Inspect(ctx, c.ID); err == nil && d.Networks[network] != "" {
				if err := l.vip.Remove(ctx, d.Networks[network]); err != nil {
					return err
				}
			}
		}
		if err := l.docker.StopRemove(ctx, c.ID); err != nil {
			return err
		}
	}
	return nil
}

// State is container truth only (DECIDE 8); the orchestrator layers the
// last job on top. Word: none | running | stopped | degraded | unhealthy.
type State struct {
	Word     string
	Replicas []docker.Container
}

func (l *Leaf) State(ctx context.Context, t store.Tile) (State, error) {
	cs, err := l.Replicas(ctx, t)
	if err != nil {
		return State{}, err
	}
	s := State{Word: "none", Replicas: cs}
	running := 0
	for _, c := range cs {
		if c.Health == "unhealthy" {
			s.Word = "unhealthy"
			return s, nil
		}
		if c.State == "running" {
			running++
		}
	}
	switch {
	case len(cs) == 0:
	case running == len(cs):
		s.Word = "running"
	case running == 0:
		s.Word = "stopped"
	default:
		s.Word = "degraded"
	}
	return s, nil
}

// The container verbs. id is a container of this tile; tileID "" is the
// admin's containers screen, where any non-system container goes.
func (l *Leaf) Stop(ctx context.Context, tileID, id string) error {
	if err := l.guard(ctx, tileID, id, "stopped"); err != nil {
		return err
	}
	return l.docker.Stop(ctx, id)
}

func (l *Leaf) StartContainer(ctx context.Context, tileID, id string) error {
	if err := l.guard(ctx, tileID, id, "started"); err != nil {
		return err
	}
	return l.docker.Start(ctx, id)
}

func (l *Leaf) Restart(ctx context.Context, tileID, id string) error {
	if err := l.guard(ctx, tileID, id, "stopped"); err != nil {
		return err
	}
	return l.docker.Restart(ctx, id)
}

// Exec runs an engine command (or a terminal line) in the container.
func (l *Leaf) Exec(ctx context.Context, tileID, id string, cmd []string) (string, error) {
	if err := l.guard(ctx, tileID, id, "opened a terminal into"); err != nil {
		return "", err
	}
	return l.docker.Exec(ctx, id, cmd)
}

// Terminal streams a command (a shell) with stdin; the guard as Exec.
func (l *Leaf) Terminal(
	ctx context.Context,
	tileID, id string,
	cmd []string,
	stdin io.Reader,
) (io.Reader, func() error, error) {
	if err := l.guard(ctx, tileID, id, "opened a terminal into"); err != nil {
		return nil, nil, err
	}
	return l.docker.ExecStream(ctx, id, cmd, stdin)
}

// Follow streams the container's log lines until stop is called.
func (l *Leaf) Follow(ctx context.Context, tileID, id string, tail int) (<-chan string, func(), error) {
	if _, err := l.find(ctx, tileID, id); err != nil {
		return nil, nil, err
	}
	return l.docker.StreamLogs(ctx, id, tail)
}

func (l *Leaf) Logs(ctx context.Context, tileID, id string, tail int) (string, error) {
	if _, err := l.find(ctx, tileID, id); err != nil {
		return "", err
	}
	return l.docker.Logs(ctx, id, tail)
}

// Remove bends the guard for an exited system container: a previous
// generation left by an upgrade must be clearable.
func (l *Leaf) Remove(ctx context.Context, tileID, id string) error {
	c, err := l.find(ctx, tileID, id)
	if errors.Is(err, errSystem) {
		if d, ierr := l.docker.Inspect(ctx, id); ierr != nil || d.Running {
			return errs.Conflictf("%s cannot be removed while they are running", systemNames)
		}
		return l.docker.StopRemove(ctx, id)
	}
	if err != nil {
		return err
	}
	return l.docker.StopRemove(ctx, c.ID)
}

// systemNames is the guard's wording. v1 has no node agent, so it names two.
const systemNames = "the panel and proxy containers"

var errSystem = errors.New("system container")

// guard is the one system-container rule over start, stop and the terminal.
// A fact about the container, not the caller: an admin is refused too.
func (l *Leaf) guard(ctx context.Context, tileID, id, verb string) error {
	_, err := l.find(ctx, tileID, id)
	if errors.Is(err, errSystem) {
		return errs.Conflictf("%s cannot be %s from here", systemNames, verb)
	}
	return err
}

// find resolves id among stackr's containers. Fails closed: a list error
// reads as system. A system container is errSystem; one that is not this
// tile's is ErrNotFound.
func (l *Leaf) find(ctx context.Context, tileID, id string) (docker.Container, error) {
	cs, err := l.docker.List(ctx, nil)
	if err != nil {
		return docker.Container{}, errSystem
	}
	for _, c := range cs {
		if c.ID != id && c.Name != id {
			continue
		}
		switch {
		case c.Labels[LabelSystem] != "":
			return c, errSystem
		case tileID != "" && c.Labels[LabelTile] != tileID:
			return c, errs.ErrNotFound
		}
		return c, nil
	}
	return docker.Container{}, errs.ErrNotFound
}

// running is every running replica of ts.
func (l *Leaf) running(ctx context.Context, ts []store.Tile) ([]string, error) {
	var ids []string
	for _, t := range ts {
		cs, err := l.Replicas(ctx, t)
		if err != nil {
			return nil, err
		}
		for _, c := range cs {
			if c.State == "running" {
				ids = append(ids, c.ID)
			}
		}
	}
	return ids, nil
}

// Freeze pauses every running replica of ts (a backup's pause mode); thaw
// unpauses them and runs even after the caller's context is gone.
func (l *Leaf) Freeze(ctx context.Context, ts []store.Tile) (thaw func(), err error) {
	ids, err := l.running(ctx, ts)
	if err != nil {
		return nil, err
	}
	var done []string
	thaw = func() {
		for _, id := range done {
			_ = l.docker.Unpause(context.WithoutCancel(ctx), id)
		}
	}
	for _, id := range ids {
		if err := l.docker.Pause(ctx, id); err != nil {
			thaw()
			return nil, fmt.Errorf("pausing %s: %w", id, err)
		}
		done = append(done, id)
	}
	return thaw, nil
}

// Quiesce stops every running replica of ts and waits for each to exit (a
// stop that returns early gives a torn tar); resume starts them again.
func (l *Leaf) Quiesce(ctx context.Context, ts []store.Tile) (resume func(), err error) {
	ids, err := l.running(ctx, ts)
	if err != nil {
		return nil, err
	}
	var done []string
	resume = func() {
		for _, id := range done {
			_ = l.docker.Start(context.WithoutCancel(ctx), id)
		}
	}
	for _, id := range ids {
		if err := l.docker.Stop(ctx, id); err != nil {
			resume()
			return nil, fmt.Errorf("stopping %s: %w", id, err)
		}
		done = append(done, id)
	}
	return resume, nil
}

// Stream execs cmd in the tile's first running replica with stdin, handing
// back stdout and a wait the caller must call on every path.
func (l *Leaf) Stream(
	ctx context.Context,
	t store.Tile,
	cmd []string,
	stdin io.Reader,
) (io.Reader, func() error, error) {
	ids, err := l.running(ctx, []store.Tile{t})
	if err != nil {
		return nil, nil, err
	}
	if len(ids) == 0 {
		return nil, nil, errs.Conflictf("%s is not running", t.Slug)
	}
	return l.docker.ExecStream(ctx, ids[0], cmd, stdin)
}
