package service

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/imagewatch"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	lrun "github.com/FyrmForge/stackr/internal/service/internal/leaf/run"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type Image = store.Image

// TileStatus is the box's word for a tile plus its newest job (DECIDE 8).
// A cron or function adds its newest run; a cron its next tick and pause.
type TileStatus struct {
	Word     string      `json:"word"` // running | stopped | partial | … (leaf/tile)
	Replicas []Container `json:"replicas"`
	LastJob  *Job        `json:"last_job"`
	LastRun  *Run        `json:"last_run,omitempty"`
	NextRun  *time.Time  `json:"next_run,omitempty"` // unpaused cron only
	Paused   bool        `json:"paused"`
}

func (o *Orchestrator) Tiles(ctx context.Context, envID string) ([]Tile, error) {
	return o.tiles.List(ctx, envID)
}

func (o *Orchestrator) TileStatus(ctx context.Context, id string) (TileStatus, error) {
	t, err := o.tiles.Get(ctx, id)
	if err != nil {
		return TileStatus{}, err
	}
	s, err := o.tiles.State(ctx, t)
	if err != nil {
		return TileStatus{}, err
	}
	out := TileStatus{Word: s.Word, Replicas: s.Replicas}
	if t.Kind == tile.Slice {
		// No containers of its own: it is up when its instance is.
		if out.Word, err = o.engines.SliceWord(ctx, t); err != nil {
			return out, err
		}
	}
	if j, ok, err := o.jobRows.Last(ctx, id); err != nil {
		return out, err
	} else if ok {
		out.LastJob = &j
	}
	if !tile.RunToCompletion(t.Kind) {
		return out, nil
	}
	if r, ok, err := o.runs.Last(ctx, id); err != nil {
		return out, err
	} else if ok {
		out.LastRun = &r
	}
	out.Paused = t.Paused
	if t.Kind == tile.Cron && !t.Paused {
		if n, err := lrun.Next(t.Schedule, time.Now()); err == nil {
			out.NextRun = &n
		}
	}
	return out, nil
}

// CreateTile is the one tile-create path for image and service tiles (B26);
// managed tiles go through CreateManagedTile. Nothing runs until Deploy.
func (o *Orchestrator) CreateTile(ctx context.Context, t Tile) (Tile, error) {
	if t.Kind == tile.Managed {
		return Tile{}, errs.Invalidf("kind", "Create a managed tile with its engine.")
	}
	if err := o.checkLimits(ctx, t.CPULimit, t.MemLimitMB, "limits.cpu", "limits.memory_mb"); err != nil {
		return Tile{}, err
	}
	return o.tiles.Create(ctx, t)
}

// UpdateTile reads the row, applies edit, and writes the whole row back, so
// a field the caller did not touch is never blanked (B1, B21). What the
// write earns runs: a proxy push, a redeploy job when the tile runs.
func (o *Orchestrator) UpdateTile(ctx context.Context, id string, edit func(*Tile) error) (Tile, *Job, error) {
	old, err := o.tiles.Get(ctx, id)
	if err != nil {
		return old, nil, err
	}
	cur := old
	if err := edit(&cur); err != nil {
		return old, nil, err
	}
	// only a limit this edit changed: a tile already over the host's size
	// can still be renamed or retargeted
	if cur.CPULimit != old.CPULimit || cur.MemLimitMB != old.MemLimitMB {
		cpu, mem := cur.CPULimit, cur.MemLimitMB
		if cur.CPULimit == old.CPULimit {
			cpu = 0
		}
		if cur.MemLimitMB == old.MemLimitMB {
			mem = 0
		}
		if err := o.checkLimits(ctx, cpu, mem, "limits.cpu", "limits.memory_mb"); err != nil {
			return old, nil, err
		}
	}
	t, effects, err := o.tiles.Update(ctx, old, cur)
	if err != nil {
		return t, nil, err
	}
	var j *Job
	for _, e := range effects {
		switch e {
		case tile.Route:
			err = o.sync.Sync(ctx)
		case tile.Redeploy:
			j, err = o.redeployIfRunning(ctx, t)
		case tile.CronReload:
			o.sched.Reload(ctx)
		}
		if err != nil {
			return t, j, err
		}
	}
	if old.Kind == tile.Cron && t.Kind != tile.Cron {
		o.sched.Reload(ctx) // its entry goes
	}
	return t, j, nil
}

// RenameTile moves name and slug, and the tile's auto domains with them.
func (o *Orchestrator) RenameTile(ctx context.Context, id, name string) (Tile, error) {
	t, err := o.tiles.Get(ctx, id)
	if err != nil {
		return t, err
	}
	if t, err = o.tiles.Rename(ctx, t, name); err != nil {
		return t, err
	}
	return t, o.refreshAutoHosts(ctx, []Tile{t})
}

// DeleteTile queues the tile's removal: containers, ingress, slices, row.
func (o *Orchestrator) DeleteTile(ctx context.Context, id string) (Job, error) {
	return o.enqueue(ctx, kindDelete, tileJob{TileID: id}, id)
}

// Deploy queues a (re)deploy on the image the env's release pins (B34); a
// newer deploy of the tile supersedes a queued one. One gate for deploy and
// redeploy (B29).
func (o *Orchestrator) Deploy(ctx context.Context, id string) (Job, error) {
	return o.enqueue(ctx, kindDeploy, tileJob{TileID: id}, id)
}

// RestartTile and StartTile: a cron or function has runs, not replicas;
// refused (tilelifecycle.md wording).
func (o *Orchestrator) RestartTile(ctx context.Context, id string) (Job, error) {
	return o.replicaVerb(ctx, id, "restart", kindRestart)
}

func (o *Orchestrator) StartTile(ctx context.Context, id string) (Job, error) {
	return o.replicaVerb(ctx, id, "start", kindStart)
}

func (o *Orchestrator) replicaVerb(ctx context.Context, id, verb string, kind jobs.Kind) (Job, error) {
	t, err := o.tiles.Get(ctx, id)
	if err != nil {
		return Job{}, err
	}
	switch {
	case t.Kind == tile.Slice:
		return Job{}, errs.Invalidf("kind", "a slice has no containers")
	case tile.RunToCompletion(t.Kind):
		return Job{}, errs.Invalidf("kind", "a %s has no long-running container to %s; use run instead", t.Kind, verb)
	}
	return o.enqueue(ctx, kind, tileJob{TileID: id}, id)
}

// StopTile: a cron parks (paused, no job: the zero Job comes back); a
// function has nothing to stop (StopRun stops a run).
func (o *Orchestrator) StopTile(ctx context.Context, id string) (Job, error) {
	t, err := o.tiles.Get(ctx, id)
	if err != nil {
		return Job{}, err
	}
	switch t.Kind {
	case tile.Cron:
		_, err = o.PauseTile(ctx, id, true)
		return Job{}, err
	case tile.Function:
		return Job{}, errs.Invalidf("kind", "nothing to stop")
	case tile.Slice:
		return Job{}, errs.Invalidf("kind", "a slice has no containers")
	}
	return o.enqueue(ctx, kindStop, tileJob{TileID: id}, id)
}

// Logs is the tail of one replica's log ("" = a running replica first);
// FollowLogs streams it. A tile with no container answers the tail of its
// last job's log: a replica that failed its start left its output there.
func (o *Orchestrator) Logs(ctx context.Context, tileID, containerID string, tail int) (string, error) {
	id, err := o.replica(ctx, tileID, containerID)
	if !errors.Is(err, errNoContainer) {
		if err != nil {
			return "", err
		}
		return o.tiles.Logs(ctx, tileID, id, tail)
	}
	// The newest job that starts containers: a restart refused since then
	// must not hide the crash.
	// ponytail: scans the newest 20; more refused verbs than that since the
	// last deploy reads as never deployed.
	js, err := o.jobRows.History(ctx, []string{tileID}, 20)
	if err != nil {
		return "", err
	}
	i := slices.IndexFunc(js, func(j Job) bool {
		return j.Kind == string(kindDeploy) || j.Kind == string(kindPromote) ||
			j.Kind == string(kindEnvSync)
	})
	if i < 0 {
		return "", errs.Conflictf("the tile has no container and no deploy yet; deploy it first")
	}
	j := js[i]
	var b []byte
	for off := int64(0); ; {
		_, l, err := o.jobRows.Poll(ctx, j.ID, off)
		if err != nil {
			return "", err
		}
		if len(l.Chunk) == 0 {
			break
		}
		b, off = append(b[max(0, len(b)-job.MaxChunk):], l.Chunk...), l.Next
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if tail > 0 && len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}
	head := fmt.Sprintf("no container; the log of its last job (%s %s):\n", j.Kind, j.State)
	return head + strings.Join(lines, "\n") + "\n", nil
}

func (o *Orchestrator) FollowLogs(
	ctx context.Context,
	tileID, containerID string,
	tail int,
) (<-chan string, func(), error) {
	id, err := o.replica(ctx, tileID, containerID)
	if errors.Is(err, errNoContainer) {
		return nil, nil, errs.Conflictf("the tile has no container to follow; stackr logs shows its last job's log")
	}
	if err != nil {
		return nil, nil, err
	}
	return o.tiles.Follow(ctx, tileID, id, tail)
}

var errNoContainer = errors.New("no container")

// replica is id, or for "" the tile's first running replica, else its
// first of any state; errNoContainer when it has none.
func (o *Orchestrator) replica(ctx context.Context, tileID, id string) (string, error) {
	if id != "" {
		return id, nil
	}
	t, err := o.tiles.Get(ctx, tileID)
	if err != nil {
		return "", err
	}
	cs, err := o.tiles.Replicas(ctx, t)
	if err != nil || len(cs) == 0 {
		return "", cmp.Or(err, errNoContainer)
	}
	i := slices.IndexFunc(cs, func(c docker.Container) bool { return c.State == "running" })
	return cs[max(i, 0)].ID, nil
}

// Terminal is an interactive exec in one replica.
func (o *Orchestrator) Terminal(
	ctx context.Context,
	tileID, containerID string,
	cmd []string,
	stdin io.Reader,
) (io.Reader, func() error, error) {
	return o.tiles.Terminal(ctx, tileID, containerID, cmd, stdin)
}

// ExitCode is the command's non-zero exit when a Terminal wait reports one.
func ExitCode(err error) (int, bool) {
	e, ok := docker.IsExit(err)
	return e.Code, ok
}

// CheckImages queues an image-watch check of one stack or tile ("" = all).
func (o *Orchestrator) CheckImages(ctx context.Context, stackID, tileID string) (Job, error) {
	return o.enqueue(ctx, kindImageWatch, imagewatch.Scope{StackID: stackID, TileID: tileID},
		"imagewatch", "imagewatch:"+stackID+"/"+tileID)
}

// Images is every image row with its watch cache.
func (o *Orchestrator) Images(ctx context.Context) ([]Image, error) { return o.images.List(ctx) }

// redeployIfRunning queues a deploy for a tile with replicas, or one a
// failed deploy left with none (reachedByRedeploy); nil job = not running.
func (o *Orchestrator) redeployIfRunning(ctx context.Context, t Tile) (*Job, error) {
	if ok, err := o.reachedByRedeploy(ctx, t); err != nil || !ok {
		return nil, err
	}
	j, err := o.Deploy(ctx, t.ID)
	return &j, err
}

// Redeploy names one tile a change queued a deploy for.
type Redeploy struct {
	Env  string `json:"env"`
	Tile string `json:"tile"`
	Job  string `json:"job"`
}

// redeployRunning: a param or settings change reaches every running tile
// it may touch, on the image its env's release pins (B34).
func (o *Orchestrator) redeployRunning(ctx context.Context, ts []Tile) error {
	_, err := o.redeploy(ctx, ts)
	return err
}

// redeploy is redeployRunning that names what it queued.
func (o *Orchestrator) redeploy(ctx context.Context, ts []Tile) ([]Redeploy, error) {
	out := []Redeploy{}
	for _, t := range ts {
		j, err := o.redeployIfRunning(ctx, t)
		if err != nil {
			return out, err
		}
		if j == nil {
			continue
		}
		e, err := o.envs.Get(ctx, t.EnvironmentID)
		if err != nil {
			return out, err
		}
		out = append(out, Redeploy{Env: e.Slug, Tile: t.Slug, Job: j.ID})
		noteRedeploy(ctx, out[len(out)-1])
	}
	return out, nil
}

// TileImage is what image watch knows of the tile's image ref; the zero
// Image when it was never built, pulled or checked.
func (o *Orchestrator) TileImage(ctx context.Context, tileID string) (Image, error) {
	t, err := o.tiles.Get(ctx, tileID)
	if err != nil || t.ImageRef == "" {
		return Image{}, err
	}
	i, err := o.images.GetByRef(ctx, t.ImageRef)
	if errors.Is(err, errs.ErrNotFound) {
		i, err = Image{Ref: t.ImageRef}, nil
	}
	if err != nil || !tile.Pulls(t) {
		return i, err
	}
	e, err := o.envs.Get(ctx, t.EnvironmentID)
	if err != nil {
		return i, err
	}
	i.Digest, err = o.releases.Digest(ctx, e.ReleaseID, t.Slug) // what runs is the pin
	return i, err
}
