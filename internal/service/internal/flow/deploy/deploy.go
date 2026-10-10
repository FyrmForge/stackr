// Package deploy puts a tile's image into containers: resolve the tile's
// facts (params, volumes, networks, limits), build the spec, pull, then roll
// out. A tile without a mount rolls with overlap (new replicas start and
// pass the gate before the old ones go); a tile with a mount, and every
// managed tile, stops then starts, one replica. A gate failure in the
// overlap keeps the old replicas serving.
//
// It runs inside a job: the orchestrator wraps Redeploy and Run into
// flow/jobs handlers (DECIDE 12), passing the job's log and swap marker.
package deploy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/managed"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/credential"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/hostgrant"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/image"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/managed"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/org"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
	lrun "github.com/FyrmForge/stackr/internal/service/internal/leaf/run"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/settings"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/stack"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tier"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Flow holds the leaves it sequences; it has no Docker handle of its own.
type Flow struct {
	Tiles    *tile.Leaf
	Envs     *environment.Leaf
	Stacks   *stack.Leaf
	Orgs     *org.Leaf
	Volumes  *volume.Leaf
	Images   *image.Leaf
	Releases *release.Leaf
	Params   *params.Leaf
	Tiers    *tier.Leaf
	Managed  *managed.Leaf
	Domains  *domain.Leaf
	Creds    *credential.Leaf
	Settings *settings.Leaf
	Jobs     *job.Leaf
	// HostGrants is the stack's approved host access (host: mounts,
	// devices, privileged); nil = nothing is granted.
	HostGrants *hostgrant.Leaf

	// DataDir is where fileBinds writes a tile's config files; set from
	// Config.DataDir.
	DataDir string

	// VIPLock, when set, is held while route reads the grants and sets the
	// VIP, so a revoke's rebuild (which holds it) cannot be overwritten.
	VIPLock sync.Locker

	// Sync re-pushes the proxy config (leaf/domain Syncer.Sync); nil = none.
	Sync func(context.Context) error
	// Engines is flow/managed (the deploy -> managed edge): a managed
	// tile's container and readiness, a slice tile's provision and every
	// consumer's binding; nil = no managed tiles here.
	Engines *mflow.Flow

	// Runs reads a function or cron tile's runs for a `tile:completed`
	// dependency; RunFirst runs an on_deploy function (and fails when its
	// run does) before its dependent rolls. Nil = no wait on completion.
	Runs     *lrun.Leaf
	RunFirst func(ctx context.Context, tileID string, log io.Writer) error
	// DepSleep waits one poll of a dependency wait; nil = a ctx-aware timer.
	// Tests replace it so no wait takes real time.
	DepSleep func(ctx context.Context, d time.Duration) error
}

// Redeploy runs the tile on the image its env's current release pins (B34:
// never the branch head). Deploy and redeploy share Run, so one gate decides
// both (B29).
func (f *Flow) Redeploy(ctx context.Context, tileID string, log io.Writer, swap func() error) error {
	t, err := f.Tiles.Get(ctx, tileID)
	if err != nil {
		return err
	}
	e, err := f.Envs.Get(ctx, t.EnvironmentID)
	if err != nil {
		return err
	}
	ref, pinned, err := f.Current(ctx, t, e)
	if err != nil {
		return err
	}
	if pinned && tile.Pulls(t) {
		pins, err := f.Releases.Pins(ctx, *e.ReleaseID)
		if err != nil {
			return err
		}
		if stale(t, pins[t.Slug]) {
			ref, pinned = t.ImageRef, false
		}
	}
	digest, err := f.Run(ctx, t, ref, log, swap)
	if err != nil || pinned || !tile.Pulls(t) {
		return err
	}
	// An image tile's first run, or its first after a tag edit: pin what the
	// tag meant, as a release.
	r, err := f.Releases.Derive(
		ctx,
		t.StackID,
		deref(e.ReleaseID),
		"deploy",
		release.Pin{Slug: t.Slug, Repo: t.ImageRef, Digest: digest},
	)
	if err != nil {
		return err
	}
	_, err = f.Envs.SetRelease(ctx, e, r.ID)
	return err
}

// Current is the image the env's release pins for the tile. pinned=false:
// the release has no pin for it and an image tile falls back to its tag.
func (f *Flow) Current(ctx context.Context, t store.Tile, e store.Environment) (ref string, pinned bool, err error) {
	if e.ReleaseID != nil {
		pins, err := f.Releases.Pins(ctx, *e.ReleaseID)
		if err != nil {
			return "", false, err
		}
		if p, ok := pins[t.Slug]; ok {
			switch {
			case p.ImageID != nil:
				im, err := f.Images.Get(ctx, *p.ImageID)
				return im.Ref, true, err
			case p.Digest != "":
				repo := RepoOf(p.Repo)
				if repo == "" {
					repo = RepoOf(t.ImageRef)
				}
				return repo + "@" + p.Digest, true, nil
			}
		}
	}
	switch {
	case tile.Pulls(t):
		return t.ImageRef, false, nil
	case t.Kind == tile.Managed:
		return "", false, nil // the engine's image
	case t.Kind == tile.Slice:
		return "", false, nil // no image: its deploy is a provision
	}
	return "", false, errs.Conflictf("%s has nothing built yet: build a commit first", t.Slug)
}

// Run deploys ref as tile t and returns the image's registry digest ("" for
// an image built here). swap marks the point past which the job must not be
// cancelled (the Railway rule); it is called right before the first
// container changes.
func (f *Flow) Run(ctx context.Context, t store.Tile, ref string, log io.Writer, swap func() error) (string, error) {
	if swap == nil {
		swap = func() error { return nil }
	}
	if t.Kind == tile.Slice {
		if err := swap(); err != nil {
			return "", err
		}
		return "", f.provision(ctx, t, log)
	}
	r, e, digest, err := f.prepare(ctx, t, ref, log)
	if err != nil {
		return digest, err
	}
	if tile.RunToCompletion(t.Kind) {
		// A cron or function deploy stops at the pulled artifact: a run
		// starts its container.
		f.prune(ctx, t, e, log)
		return digest, nil
	}
	if err := f.awaitDeps(ctx, t, log); err != nil {
		return "", err
	}
	if _, err := f.Images.Ensure(ctx, tile.PauseImage, "", log); err != nil {
		return "", fmt.Errorf("pull %s: %w", tile.PauseImage, err)
	}
	if err := f.rollout(ctx, t, e, r, log, swap); err != nil {
		return "", err
	}
	if t.Kind == tile.Managed {
		logf(log, "waiting for %s to accept connections\n", t.Slug)
		return digest, f.Engines.Ready(ctx, t)
	}
	f.prune(ctx, t, e, log)
	f.pruneFolders(ctx, t, log)
	return digest, nil
}

// pruneFolders drops the tile's config file folders no replica mounts any
// more. A failure is logged: the deploy already rolled out.
func (f *Flow) pruneFolders(ctx context.Context, t store.Tile, log io.Writer) {
	ms, err := f.Tiles.Mounts(ctx, t)
	if err == nil {
		err = f.pruneFiles(t, ms)
	}
	if err != nil {
		logf(log, "warning: prune config files: %v\n", err)
	}
}

// Spec is the container a run of t starts on ref: the same facts, networks,
// params and volumes a service replica gets, from the same builder, pulled.
func (f *Flow) Spec(ctx context.Context, t store.Tile, ref string, log io.Writer) (docker.ContainerSpec, error) {
	r, _, _, err := f.prepare(ctx, t, ref, log)
	if err != nil {
		return docker.ContainerSpec{}, err
	}
	s := spec(t, r)
	s.Name = r.name
	return s, nil
}

// prepare resolves t's facts and pulls ref: everything a container needs
// short of starting one.
func (f *Flow) prepare(
	ctx context.Context,
	t store.Tile,
	ref string,
	log io.Writer,
) (resolved, store.Environment, string, error) {
	if t.Kind == tile.Slice {
		return resolved{}, store.Environment{}, "", errs.Invalidf("kind", "a slice has no containers")
	}
	e, err := f.Envs.Get(ctx, t.EnvironmentID)
	if err != nil {
		return resolved{}, e, "", err
	}
	st, err := f.Stacks.Get(ctx, t.StackID)
	if err != nil {
		return resolved{}, e, "", err
	}
	o, err := f.Orgs.Get(ctx, st.OrgID)
	if err != nil {
		return resolved{}, e, "", err
	}
	var def mflow.Container
	if t.Kind == tile.Managed {
		if f.Engines == nil {
			return resolved{}, e, "", fmt.Errorf("%s: no managed engines are wired", t.Slug)
		}
		if def, err = f.Engines.Container(ctx, t); err != nil {
			return resolved{}, e, "", err
		}
		if ref == "" {
			ref = def.Image
		}
	}
	if ref == "" {
		return resolved{}, e, "", fmt.Errorf("%s: no image to run", t.Slug)
	}
	if err := f.bind(ctx, t, e, st, o, log); err != nil {
		return resolved{}, e, "", err
	}

	r, err := f.resolve(ctx, t, e, st, o, def, log)
	if err != nil {
		return r, e, "", err
	}
	auth := ""
	if c, ok, err := f.Creds.For(ctx, o.ID, ref); err != nil {
		return r, e, "", err
	} else if ok {
		auth = credential.Auth(c)
	}
	logf(log, "pulling %s\n", ref)
	digest, err := f.Images.Ensure(ctx, ref, auth, log)
	if errors.Is(err, image.ErrCleanedUp) {
		return r, e, "", fmt.Errorf("the image for %s was cleaned up; rebuild it", t.Slug)
	}
	if err != nil {
		return r, e, "", fmt.Errorf("pull %s: %w", ref, err)
	}
	if digest != "" {
		// The replica label carries the pin so a tag-started tile compares
		// with a later digest pin (promote's runsOther).
		r.pinRef = RepoOf(ref) + "@" + digest
	}
	r.image = ref
	return r, e, digest, nil
}

// rollout swaps the tile's replicas for ones running r.
func (f *Flow) rollout(
	ctx context.Context,
	t store.Tile,
	e store.Environment,
	r resolved,
	log io.Writer,
	swap func() error,
) error {
	old, err := f.Tiles.Replicas(ctx, t)
	if err != nil {
		return err
	}
	overlap := t.Kind != tile.Managed && len(r.binds) == 0 && !r.hostNet
	if !overlap {
		return f.stopFirst(ctx, t, e, r, old, log, swap)
	}
	n := max(t.Replicas, 1)
	if err := swap(); err != nil {
		return err
	}
	var started []string
	for i := range n {
		s := spec(t, r)
		s.Name = fmt.Sprintf("%s-%d", r.name, i+1)
		logf(log, "starting %s\n", s.Name)
		id, err := f.Tiles.Start(ctx, t, s, log)
		if err != nil {
			// Whatever ran before keeps running (overlap), and no half set of
			// new replicas is left behind.
			for _, id := range started {
				_ = f.Tiles.Remove(context.WithoutCancel(ctx), t.ID, id)
			}
			return err
		}
		started = append(started, id)
	}
	if err := f.route(ctx, t, e); err != nil {
		return err
	}
	if len(old) > 0 {
		logf(log, "removing %d old replica(s)\n", len(old))
		if err := f.remove(ctx, t, old); err != nil {
			return err
		}
		return f.route(ctx, t, e)
	}
	return nil
}

func (f *Flow) remove(ctx context.Context, t store.Tile, cs []docker.Container) error {
	for _, c := range cs {
		if err := f.Tiles.Remove(ctx, t.ID, c.ID); err != nil && !errors.Is(err, errs.ErrNotFound) {
			return err
		}
	}
	return nil
}

// stopFirst is the rollout of a tile that holds data: the old replica stops
// before the new one starts, but the new one is created first (a spec the
// daemon refuses, like a limit above the host's, fails with nothing touched),
// and a new replica that does not start or pass the gate is removed and the
// old ones that ran are started again, so the tile is never left with
// nothing.
func (f *Flow) stopFirst(
	ctx context.Context,
	t store.Tile,
	e store.Environment,
	r resolved,
	old []docker.Container,
	log io.Writer,
	swap func() error,
) error {
	if err := swap(); err != nil {
		return err
	}
	s := spec(t, r)
	s.Name = r.name + "-1"
	logf(log, "creating %s\n", s.Name)
	id, err := f.Tiles.CreateReplica(ctx, t, s)
	if err != nil {
		return err
	}
	logf(log, "stopping %d replica(s): this tile holds data, so it stops before it starts\n", len(old))
	var stopped []docker.Container
	for _, c := range old {
		if c.State != "running" {
			continue
		}
		if err := f.Tiles.Stop(ctx, t.ID, c.ID); err != nil && !errors.Is(err, errs.ErrNotFound) {
			f.revive(ctx, t, e, stopped, id, log)
			return err
		}
		stopped = append(stopped, c)
	}
	logf(log, "starting %s\n", s.Name)
	if err := f.Tiles.Launch(ctx, t, id, log); err != nil {
		f.revive(ctx, t, e, stopped, "", log)
		return err
	}
	if err := f.route(ctx, t, e); err != nil {
		return err
	}
	logf(log, "removing %d old replica(s)\n", len(old))
	if err := f.remove(ctx, t, old); err != nil {
		return err
	}
	return f.route(ctx, t, e)
}

// revive starts the replicas a failed stop-first rollout stopped and drops
// the new one that never ran (drop "" = Launch already removed it). It runs
// on a context that survives a cancel; its own failures are logged, the
// rollout's error is what the job reports.
func (f *Flow) revive(
	ctx context.Context,
	t store.Tile,
	e store.Environment,
	stopped []docker.Container,
	drop string,
	log io.Writer,
) {
	ctx = context.WithoutCancel(ctx)
	if drop != "" {
		_ = f.Tiles.Remove(ctx, t.ID, drop)
	}
	for _, c := range stopped {
		if err := f.Tiles.StartContainer(ctx, t.ID, c.ID); err != nil {
			logf(log, "warning: start the old replica %s again: %v\n", c.Name, err)
		}
	}
	if err := f.route(ctx, t, e); err != nil {
		logf(log, "warning: route the old replica: %v\n", err)
	}
}

// volumeBind is a "volume:/abs[:ro]" line: the env's volume, created on first
// use, as a Docker bind.
func (f *Flow) volumeBind(ctx context.Context, t store.Tile, e store.Environment, m tile.Mount) (string, error) {
	v, err := f.Volumes.BySlug(ctx, volume.Scope{Kind: "env", ID: e.ID}, m.Volume)
	if errors.Is(err, errs.ErrNotFound) {
		return "", errs.Conflictf("%s mounts volume %q, which this environment does not declare", t.Slug, m.Volume)
	}
	if err != nil {
		return "", err
	}
	name, err := f.Volumes.Ensure(ctx, v)
	if err != nil {
		return "", err
	}
	return name + ":" + m.Path + roSuffix(m), nil
}

// roSuffix is ":ro" for a read-only mount, "" otherwise.
func roSuffix(m tile.Mount) string {
	if m.RO {
		return ":ro"
	}
	return ""
}

// route points the VIP and the proxy at the running replicas.
func (f *Flow) route(ctx context.Context, t store.Tile, e store.Environment) error {
	if t.HostNetwork { // no pause container, no VIP: only the proxy follows
		if err := f.Tiles.DropPause(ctx, t, e.Network); err != nil {
			return err
		}
		if f.Sync == nil {
			return nil
		}
		return f.Sync(ctx)
	}
	if err := f.routeVIP(ctx, t, e); err != nil {
		return err
	}
	if f.Sync == nil {
		return nil
	}
	return f.Sync(ctx)
}

// routeVIP reads the grants and sets the VIP as one step under VIPLock.
func (f *Flow) routeVIP(ctx context.Context, t store.Tile, e store.Environment) error {
	if f.VIPLock != nil {
		f.VIPLock.Lock()
		defer f.VIPLock.Unlock()
	}
	var lan []string
	if f.HostGrants != nil {
		g, err := f.HostGrants.Of(ctx, t.StackID)
		if err != nil {
			return err
		}
		lan = LanLines(g, t)
	}
	return f.Tiles.Route(ctx, t, e.Network, lan)
}

// LanLines are the "lan:..." perms (slug stripped) t asks for and g holds.
func LanLines(g hostgrant.Set, t store.Tile) []string {
	var out []string
	for _, l := range HostSet(t).Lines {
		if _, perm := hostgrant.Split(l); strings.HasPrefix(perm, hostgrant.LAN) && g.Has(l) {
			out = append(out, perm)
		}
	}
	return out
}

// resolve gathers every fact the spec needs, in the order that matters:
// values, then volume lines, then the command, then networks.
func (f *Flow) resolve(
	ctx context.Context,
	t store.Tile,
	e store.Environment,
	st store.Stack,
	o store.Org,
	def mflow.Container,
	log io.Writer,
) (resolved, error) {
	snap, err := f.snapshot(ctx, t, e, st)
	if err != nil {
		return resolved{}, err
	}
	rr := params.NewResolver(snap)
	r := resolved{name: "stackr-" + t.Slug + "-" + short(t.ID) + "-" + rnd()}

	// PR envs never hold grants (a fork PR runs unreviewed code): a tile
	// asking for host access is not deployed there, and does not park.
	if e.Type == environment.Ephemeral && !HostSet(t).Empty() {
		return r, errs.Conflictf("%s asks for elevated host access; PR envs never get it, so it is not deployed", t.Slug)
	}

	// Host access first: a tile the grant does not cover parks before
	// anything else is read or changed.
	g, err := f.checkAccess(ctx, st, t)
	if err != nil {
		return r, err
	}
	r.privileged = t.Privileged && g.Has(hostgrant.Line(t.Slug, hostgrant.Privileged))
	r.hostNet = t.HostNetwork && g.Has(hostgrant.Line(t.Slug, hostgrant.NetworkHost))

	// An unset value parks the job (errs.Unset); any other failure fails it.
	// A tile never starts with an unresolved reference.
	env, err := envMap(t.EnvJSON)
	if err != nil {
		return r, err
	}
	r.env = append(r.env, def.Env...)
	for _, k := range slices.Sorted(maps.Keys(env)) {
		v, err := rr.Expand(params.InEnv, env[k])
		if err != nil {
			return r, err
		}
		r.env = append(r.env, k+"="+v)
	}
	if err := f.depParked(ctx, t, rr.Deps()); err != nil {
		return r, err
	}

	r.binds = append(r.binds, def.Binds...)
	for _, l := range tile.Lines(t.Volumes) {
		l, err := rr.Expand(params.InEnv, l)
		if err != nil {
			return r, err
		}
		m, err := tile.ParseMount(l)
		if err != nil {
			return r, errs.Invalidf("volumes", "%s", err.Error())
		}
		var src string
		switch m.Kind {
		case tile.MountShare:
			src, err = f.shareBind(ctx, o, e, st, t, m)
		case tile.MountHost:
			src, err = f.hostBind(ctx, st, t, m)
		default:
			src, err = f.volumeBind(ctx, t, e, m)
		}
		if err != nil {
			return r, err
		}
		r.binds = append(r.binds, src)
	}
	if strings.TrimSpace(t.Files) != "" {
		fb, err := f.fileBinds(ctx, t, e, st, rr)
		if err != nil {
			return r, err
		}
		r.binds = append(r.binds, fb...)
	}

	r.cmd = def.Cmd
	if strings.TrimSpace(t.Command) != "" {
		c, err := rr.Expand(params.InCommand, t.Command)
		if err != nil {
			return r, err
		}
		if r.cmd, err = splitCommand(c); err != nil {
			return r, err
		}
	}

	var warn []string
	if !r.hostNet {
		r.ports, warn = publishedPorts(t.PublishedPorts)
	}
	for _, w := range warn {
		logf(log, "warning: %s\n", w)
	}
	for _, l := range tile.Lines(t.Devices) {
		d, err := tile.ParseDevice(l)
		if err != nil {
			return r, err
		}
		r.devices = append(r.devices, d)
	}

	levels := []settings.Settings{}
	def0, err := f.Settings.Defaults(ctx)
	if err != nil {
		return r, err
	}
	levels = append(levels, def0)
	for _, blob := range []string{o.Settings, st.Settings, e.Settings} {
		s, err := settings.Parse(blob)
		if err != nil {
			return r, err
		}
		levels = append(levels, s)
	}
	r.cpu, r.memMB = settings.Resolve(levels...).EffectiveLimits(t.CPULimit, t.MemLimitMB)

	if r.hostNet {
		if len(rr.Networks()) > 0 {
			return r, errs.Conflictf("%s runs on the host network and cannot join another tile's network", t.Slug)
		}
		return r, nil
	}
	net, err := f.Envs.Network(ctx, e)
	if err != nil {
		return r, err
	}
	// No alias on the env network: the slug is the pause container's, so
	// DNS answers with the VIP and never a single replica.
	r.networks = append([]docker.NetAttach{{Name: net}}, def.Networks...)
	for _, n := range rr.Networks() {
		if err := f.Envs.Shared(ctx, n); err != nil {
			return r, err
		}
		r.networks = append(r.networks, docker.NetAttach{Name: n})
	}
	ds, err := f.Domains.ListByTile(ctx, t.ID)
	if err != nil {
		return r, err
	}
	if len(ds) > 0 {
		if err := f.Domains.OpenIngress(ctx, t.ID, nil); err != nil {
			return r, err
		}
		r.networks = append(r.networks, docker.NetAttach{Name: domain.Ingress(t.ID)})
	}
	return r, nil
}

// depParked parks this tile on the param a dependency is parked on, so one
// value releases the whole chain.
func (f *Flow) depParked(ctx context.Context, t store.Tile, refDeps []string) error {
	deps := refDeps
	for _, l := range tile.Lines(t.DependsOn) {
		if s, _, err := tile.ParseDep(l); err == nil {
			deps = append(deps, s)
		}
	}
	for _, s := range deps {
		dt, err := f.Tiles.GetBySlug(ctx, t.EnvironmentID, s)
		if errors.Is(err, errs.ErrNotFound) {
			continue // a shared source: not this env's job queue
		}
		if err != nil {
			return err
		}
		j, ok, err := f.Jobs.Last(ctx, dt.ID)
		if err != nil {
			return err
		}
		if ok && j.State == job.Waiting && j.WaitingParam != nil {
			return errs.Unset{Param: *j.WaitingParam}
		}
	}
	return nil
}

func short(id string) string { return id[:min(8, len(id))] }

func rnd() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// The tag is after the last ":" only when that is after the last "/", or a
// registry port would read as the tag.
// RepoOf strips the tag and any digest ("ghcr.io/a/b:1" -> "ghcr.io/a/b"):
// what a release pin's Repo holds.
// stale: the image tile's ref was edited since the pin was taken (an image
// pin's Repo holds the ref it came from, tag included), so a redeploy runs
// the tag and pins it anew (DECIDE 140). Promote and rollback never ask:
// they run the release as pinned.
func stale(t store.Tile, p release.Pin) bool {
	return tile.Pulls(t) && p.Repo != "" && p.Repo != t.ImageRef
}

func RepoOf(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[:i]
	}
	return ref
}

// KnownEngine reports whether flow/managed runs the engine; promote checks
// the file with it (promote may not import flow/managed).
func KnownEngine(name string) bool {
	_, ok := mflow.Engines[name]
	return ok
}

// logf writes a line to the job log; a log write failing never fails a deploy.
func logf(w io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(w, format, a...)
}
