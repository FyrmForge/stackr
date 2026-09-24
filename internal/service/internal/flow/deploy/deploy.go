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
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/credential"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/image"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/managed"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/org"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/settings"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/stack"
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
	Managed  *managed.Leaf
	Domains  *domain.Leaf
	Creds    *credential.Leaf
	Settings *settings.Leaf
	Jobs     *job.Leaf

	// Sync re-pushes the proxy config (leaf/domain Syncer.Sync); nil = none.
	Sync func(context.Context) error
	// Engine is a managed tile's definition (flow/managed); nil = no
	// managed tiles here.
	Engine func(ctx context.Context, t store.Tile) (Definition, error)
	// Reconcile re-provisions the tile's dropped slices before its values
	// resolve, so a self-healed secret is visible to this same deploy
	// (flow/managed); nil = nothing to reconcile.
	Reconcile func(ctx context.Context, t store.Tile, log io.Writer) error
}

// Definition is what a managed engine fixes for its instance's container.
type Definition struct {
	Image  string
	Cmd    []string
	Env    []string
	Mounts []string // "volume-slug:/path", declared by the engine
	Health string
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
	digest, err := f.Run(ctx, t, ref, log, swap)
	if err != nil || pinned || t.Kind != tile.Image {
		return err
	}
	// An image tile's first run: pin what the tag meant, as a release.
	r, err := f.Releases.Derive(ctx, t.StackID, deref(e.ReleaseID), "deploy",
		release.Pin{Slug: t.Slug, Repo: repoOf(t.ImageRef), Digest: digest})
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
				repo := p.Repo
				if repo == "" {
					repo = repoOf(t.ImageRef)
				}
				return repo + "@" + p.Digest, true, nil
			}
		}
	}
	switch t.Kind {
	case tile.Image:
		return t.ImageRef, false, nil
	case tile.Managed:
		return "", false, nil // the engine's image
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
	e, err := f.Envs.Get(ctx, t.EnvironmentID)
	if err != nil {
		return "", err
	}
	st, err := f.Stacks.Get(ctx, t.StackID)
	if err != nil {
		return "", err
	}
	o, err := f.Orgs.Get(ctx, st.OrgID)
	if err != nil {
		return "", err
	}
	var def Definition
	if t.Kind == tile.Managed {
		if f.Engine == nil {
			return "", fmt.Errorf("%s: no managed engines are wired", t.Slug)
		}
		if def, err = f.Engine(ctx, t); err != nil {
			return "", err
		}
		if ref == "" {
			ref = def.Image
		}
	}
	if ref == "" {
		return "", fmt.Errorf("%s: no image to run", t.Slug)
	}
	if strings.TrimSpace(t.Files) != "" {
		// ponytail: files: mounts are not materialized yet (DECIDE 27).
		return "", errs.Conflictf("%s: files: mounts are not supported yet", t.Slug)
	}
	if f.Reconcile != nil {
		if err := f.Reconcile(ctx, t, log); err != nil {
			return "", err
		}
	}

	r, err := f.resolve(ctx, t, e, st, o, def, log)
	if err != nil {
		return "", err
	}
	auth := ""
	if c, ok, err := f.Creds.For(ctx, o.ID, ref); err != nil {
		return "", err
	} else if ok {
		auth = credential.Auth(c)
	}
	logf(log, "pulling %s\n", ref)
	digest, err := f.Images.Ensure(ctx, ref, auth, log)
	if err != nil {
		return "", fmt.Errorf("pull %s: %w", ref, err)
	}
	if _, err := f.Images.Ensure(ctx, tile.PauseImage, "", log); err != nil {
		return "", fmt.Errorf("pull %s: %w", tile.PauseImage, err)
	}
	r.image = ref
	return digest, f.rollout(ctx, t, e, r, log, swap)
}

// rollout swaps the tile's replicas for ones running r.
func (f *Flow) rollout(ctx context.Context, t store.Tile, e store.Environment, r resolved, log io.Writer, swap func() error) error {
	old, err := f.Tiles.Replicas(ctx, t)
	if err != nil {
		return err
	}
	overlap := t.Kind != tile.Managed && len(r.binds) == 0
	n := max(t.Replicas, 1)
	if !overlap {
		n = 1
	}
	if err := swap(); err != nil {
		return err
	}
	if !overlap {
		logf(log, "stopping %d replica(s): this tile holds data, so it stops before it starts\n", len(old))
		if err := f.remove(ctx, t, old); err != nil {
			return err
		}
		old = nil
	}
	var started []string
	for i := range n {
		s := spec(t, r)
		s.Name = fmt.Sprintf("%s-%d", r.name, i+1)
		logf(log, "starting %s\n", s.Name)
		id, err := f.Tiles.Start(ctx, t, s)
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

// route points the VIP and the proxy at the running replicas.
func (f *Flow) route(ctx context.Context, t store.Tile, e store.Environment) error {
	if err := f.Tiles.Route(ctx, t, e.Network); err != nil {
		return err
	}
	if f.Sync == nil {
		return nil
	}
	return f.Sync(ctx)
}

// resolve gathers every fact the spec needs, in the order that matters:
// values, then volume lines, then the command, then networks.
func (f *Flow) resolve(ctx context.Context, t store.Tile, e store.Environment, st store.Stack, o store.Org, def Definition, log io.Writer) (resolved, error) {
	snap, err := f.snapshot(ctx, t, e, st)
	if err != nil {
		return resolved{}, err
	}
	rr := params.NewResolver(snap)
	r := resolved{name: "stackr-" + t.Slug + "-" + short(t.ID) + "-" + rnd()}

	// An unset value parks the job (errs.Unset); any other failure fails it.
	// A tile never starts with an unresolved reference.
	env, err := envMap(t.EnvJSON)
	if err != nil {
		return r, err
	}
	r.env = append(r.env, def.Env...)
	for _, k := range sortedKeys(env) {
		v, err := rr.Expand(params.InEnv, env[k])
		if err != nil {
			return r, err
		}
		r.env = append(r.env, k+"="+v)
	}
	if err := f.depParked(ctx, t, rr.Deps()); err != nil {
		return r, err
	}

	mounts := append(append([]string{}, def.Mounts...), tile.Lines(t.Volumes)...)
	for _, l := range mounts {
		l, err := rr.Expand(params.InEnv, l)
		if err != nil {
			return r, err
		}
		sl, rest, _ := strings.Cut(l, ":")
		v, err := f.Volumes.BySlug(ctx, volume.Scope{Kind: "env", ID: e.ID}, sl)
		if errors.Is(err, errs.ErrNotFound) {
			return r, errs.Conflictf("%s mounts volume %q, which this environment does not declare", t.Slug, sl)
		}
		if err != nil {
			return r, err
		}
		name, err := f.Volumes.Ensure(ctx, v)
		if err != nil {
			return r, err
		}
		r.binds = append(r.binds, name+":"+rest)
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
	r.ports, warn = publishedPorts(t.PublishedPorts)
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

	net, err := f.Envs.Network(ctx, e)
	if err != nil {
		return r, err
	}
	// No alias on the env network: the slug is the pause container's, so
	// DNS answers with the VIP and never a single replica.
	r.networks = []docker.NetAttach{{Name: net}}
	for _, n := range rr.Networks() {
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

// repoOf strips the tag and any digest: "ghcr.io/a/b:1" -> "ghcr.io/a/b".
// The tag is after the last ":" only when that is after the last "/", or a
// registry port would read as the tag.
func repoOf(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[:i]
	}
	return ref
}

// logf writes a line to the job log; a log write failing never fails a deploy.
func logf(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
