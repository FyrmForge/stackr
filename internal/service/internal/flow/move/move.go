// Package move moves an env's volumes and containers from one node to
// another as a durable, phased job. Every phase is written to env_moves
// before its work starts, so Run on a half-done move resumes at that phase
// (every phase is safe to repeat).
//
// Cold: stop the env, stream each volume as a tar from the source node's
// Docker to the target's, start on the target.
// Live: rsync each volume while the env runs, stop, a second rsync pass with
// checksums, start on the target.
//
// Before the target starts, any failure rolls back: the env is started on
// the source again and the row ends rolled_back. After it, the old
// containers are removed and the old volumes are kept (the operator deletes
// them); a failure there leaves the phase at cleaning with the error, and
// Run retries it.
//
// The flow holds no Docker handle of its own: the per-node handles and the
// env operations are injected (the orchestrator adapts the router and the
// tile leaves).
package move

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Phases, in order. A phase names the work to do (or in progress).
const (
	Queued     = "queued"
	Syncing    = "syncing" // live: rsync while running
	Stopping   = "stopping"
	Copying    = "copying" // cold: tar stream
	Final      = "final"   // live: checksum pass
	Starting   = "starting"
	Cleaning   = "cleaning" // started on the target; old containers go
	Done       = "done"
	RolledBack = "rolled_back"
)

// RsyncImage is the helper image (images/rsync); both nodes must be able to
// pull it. A var: an air-gapped install points it at an image it has.
var RsyncImage = "stackr-rsync:1"

// RsyncPort is the first host port a target helper publishes; volume i uses
// RsyncPort+i.
const RsyncPort = 8873

// Docker is the slice of one node's Docker handle the move uses.
type Docker interface {
	CreateVolume(ctx context.Context, name, driver string, opts, labels map[string]string) error
	InspectVolume(ctx context.Context, name string) (docker.VolumeInfo, error)
	EnsureTool(ctx context.Context) error
	TarVolume(ctx context.Context, name string, w io.Writer, live bool) error
	UntarVolume(ctx context.Context, name string, src io.Reader) error
	Pull(ctx context.Context, ref, auth string, w io.Writer) error
	LocalDigest(ctx context.Context, ref string) (string, error)
	Run(ctx context.Context, spec docker.ContainerSpec) (string, error)
	Wait(ctx context.Context, id string) (int, error)
	StopRemove(ctx context.Context, id string) error
}

// Nodes resolves a node id: its Docker handle (lane W1's router) and the
// address other nodes reach it on (its WireGuard address).
type Nodes interface {
	Docker(nodeID string) (Docker, error)
	Addr(ctx context.Context, nodeID string) (string, error)
}

// Env is what the move needs done to the env itself.
type Env interface {
	// Volumes are the env's volumes, orphans excluded.
	Volumes(ctx context.Context, envID string) ([]store.Volume, error)
	// Stop stops every container of the env on the node it runs on now.
	Stop(ctx context.Context, envID string) error
	// Start starts the env on nodeID and makes that node its node. Safe to
	// repeat. On the target it is a full deploy-style start in the target's
	// range; on the source it is the rollback.
	Start(ctx context.Context, envID, nodeID string) error
	// Remove removes the env's containers on nodeID, never its volumes.
	Remove(ctx context.Context, envID, nodeID string) error
}

type Flow struct {
	Moves store.MoveStore
	Nodes Nodes
	Env   Env
	Now   func() time.Time // nil = time.Now
}

// Request is one move to begin.
type Request struct {
	EnvID, From, To string
	Live            bool
}

// Begin writes the queued row. One unfinished move per env.
func (f *Flow) Begin(ctx context.Context, r Request) (store.EnvMove, error) {
	if r.From == r.To {
		return store.EnvMove{}, errs.Invalidf("node", "the env is already on that node")
	}
	have, err := f.Moves.ListByEnv(ctx, r.EnvID)
	if err != nil {
		return store.EnvMove{}, err
	}
	for _, m := range have {
		if !ended(m.Phase) {
			return store.EnvMove{}, errs.Conflictf("a move of this env is already running")
		}
	}
	m := store.EnvMove{
		ID: uuid.NewString(), EnvID: r.EnvID, FromNode: r.From, ToNode: r.To,
		Live: r.Live, Phase: Queued, StartedAt: f.now(),
	}
	return m, f.Moves.Create(ctx, m)
}

// Unfinished are the moves to hand to Run after a restart.
func (f *Flow) Unfinished(ctx context.Context) ([]store.EnvMove, error) {
	return f.Moves.ListUnfinished(ctx)
}

// Run drives the move from its recorded phase to done or rolled_back. A
// cancelled ctx stops it where it is (no rollback): the row says where, and
// Run again resumes. The returned error is the cause of a rollback, or of a
// cleaning failure.
func (f *Flow) Run(ctx context.Context, id string, log io.Writer) (store.EnvMove, error) {
	m, err := f.Moves.Get(ctx, id)
	if err != nil {
		return m, err
	}
	for !ended(m.Phase) {
		before := m.Phase
		if err := f.step(ctx, &m, log); err != nil {
			if ctx.Err() != nil {
				return m, err
			}
			if m.Phase == Cleaning {
				m.Error = err.Error()
				return m, errors.Join(err, f.Moves.Update(ctx, m))
			}
			return f.Rollback(ctx, m, err, log)
		}
		logf(log, "move %s: %s done\n", m.ID, before)
	}
	return m, nil
}

// Rollback ends an unfinished move that has not started on the target:
// the env runs on the source again, the row ends rolled_back with cause. A
// move already past the target start cannot be rolled back.
func (f *Flow) Rollback(ctx context.Context, m store.EnvMove, cause error, log io.Writer) (store.EnvMove, error) {
	if m.Phase == Cleaning || ended(m.Phase) {
		return m, errs.Conflictf("the move already started on the target")
	}
	ctx = context.WithoutCancel(ctx)
	logf(log, "move %s: rolling back: %v\n", m.ID, cause)
	var rerr error
	if m.Phase == Starting {
		// ponytail: best effort, a partial target start is cleared by the next move's Start
		_ = f.Env.Remove(ctx, m.EnvID, m.ToNode)
	}
	f.dropHelpers(ctx, m)
	if reachedStop(m) {
		rerr = f.Env.Start(ctx, m.EnvID, m.FromNode)
	}
	if rerr != nil {
		m.Error = fmt.Sprintf("%v; rollback: %v", cause, rerr)
		return m, errors.Join(cause, rerr, f.Moves.Update(ctx, m))
	}
	now := f.now()
	m.Phase, m.FinishedAt = RolledBack, &now
	m.Error = ""
	if cause != nil {
		m.Error = cause.Error()
	}
	return m, errors.Join(cause, f.Moves.Update(ctx, m))
}

func reachedStop(m store.EnvMove) bool {
	switch m.Phase {
	case Stopping, Copying, Final, Starting:
		return true
	}
	return false
}

func ended(phase string) bool { return phase == Done || phase == RolledBack }

// next is the phase after p in this move's variant.
func next(m store.EnvMove) string {
	order := []string{Queued, Stopping, Copying, Starting, Cleaning, Done}
	if m.Live {
		order = []string{Queued, Syncing, Stopping, Final, Starting, Cleaning, Done}
	}
	for i, p := range order {
		if p == m.Phase {
			return order[i+1]
		}
	}
	return Done
}

// step does the work of m.Phase, then records the next phase.
func (f *Flow) step(ctx context.Context, m *store.EnvMove, log io.Writer) error {
	var err error
	switch m.Phase {
	case Queued:
		err = f.prepare(ctx, *m)
	case Syncing:
		err = f.sync(ctx, m, false, log)
	case Stopping:
		err = f.Env.Stop(ctx, m.EnvID)
	case Copying:
		err = f.copyCold(ctx, m, log)
	case Final:
		err = f.sync(ctx, m, true, log)
	case Starting:
		err = f.Env.Start(ctx, m.EnvID, m.ToNode)
	case Cleaning:
		err = f.Env.Remove(ctx, m.EnvID, m.FromNode)
	}
	if err != nil {
		return err
	}
	m.Phase = next(*m)
	m.Error = ""
	if ended(m.Phase) {
		now := f.now()
		m.FinishedAt = &now
	}
	return f.Moves.Update(ctx, *m)
}

// prepare gets everything slow or fallible done while the env still runs:
// both nodes reachable, helper image pulled, target volumes created.
func (f *Flow) prepare(ctx context.Context, m store.EnvMove) error {
	src, dst, err := f.docks(m)
	if err != nil {
		return err
	}
	vols, err := f.Env.Volumes(ctx, m.EnvID)
	if err != nil {
		return err
	}
	for _, d := range []Docker{src, dst} {
		if m.Live {
			// a node that has the helper (loaded with the release) keeps it;
			// only a missing one is pulled
			if _, err := d.LocalDigest(ctx, RsyncImage); err == nil {
				continue
			}
			if err := d.Pull(ctx, RsyncImage, "", io.Discard); err != nil {
				return fmt.Errorf("pull %s: %w", RsyncImage, err)
			}
		} else if err := d.EnsureTool(ctx); err != nil {
			return err
		}
	}
	for _, v := range vols {
		if err := dst.CreateVolume(ctx, v.Name, "local", nil, map[string]string{"stackr.volume": v.ID}); err != nil {
			return err
		}
	}
	return nil
}

func (f *Flow) docks(m store.EnvMove) (src, dst Docker, err error) {
	if src, err = f.Nodes.Docker(m.FromNode); err != nil {
		return nil, nil, err
	}
	dst, err = f.Nodes.Docker(m.ToNode)
	return src, dst, err
}

// copyCold streams each volume's tar from the source to the target.
func (f *Flow) copyCold(ctx context.Context, m *store.EnvMove, log io.Writer) error {
	src, dst, err := f.docks(*m)
	if err != nil {
		return err
	}
	vols, err := f.Env.Volumes(ctx, m.EnvID)
	if err != nil {
		return err
	}
	m.Bytes = 0
	for _, v := range vols {
		logf(log, "copying %s\n", v.Slug)
		n, err := stream(ctx, src, dst, v.Name)
		if err != nil {
			return err
		}
		m.Bytes += n
	}
	return nil
}

// stream pipes TarVolume on src into UntarVolume on dst. The untar wipes the
// target volume first, so a repeat after a crash starts clean.
func stream(ctx context.Context, src, dst Docker, name string) (int64, error) {
	pr, pw := io.Pipe()
	cw := &counter{w: pw}
	tarErr := make(chan error, 1)
	go func() {
		err := src.TarVolume(ctx, name, cw, false)
		_ = pw.CloseWithError(err) // nil = EOF
		tarErr <- err
	}()
	uerr := dst.UntarVolume(ctx, name, pr)
	_ = pr.CloseWithError(io.ErrClosedPipe) // unblock the tar side if untar stopped early
	terr := <-tarErr
	if terr != nil {
		return 0, terr // the cause; an untar error after it is its echo
	}
	return cw.n, uerr
}

type counter struct {
	w io.Writer
	n int64
}

func (c *counter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// sync runs one rsync pass per volume: a daemon helper on the target node
// publishing the volume, a client helper on the source reading it read-only.
// check = compare by checksum (the final pass, env stopped).
func (f *Flow) sync(ctx context.Context, m *store.EnvMove, check bool, log io.Writer) error {
	src, dst, err := f.docks(*m)
	if err != nil {
		return err
	}
	srcAddr, err := f.Nodes.Addr(ctx, m.FromNode)
	if err != nil {
		return err
	}
	dstAddr, err := f.Nodes.Addr(ctx, m.ToNode)
	if err != nil {
		return err
	}
	if !safeAddr(srcAddr) || !safeAddr(dstAddr) {
		return fmt.Errorf("node address %q / %q is not a plain address", srcAddr, dstAddr)
	}
	vols, err := f.Env.Volumes(ctx, m.EnvID)
	if err != nil {
		return err
	}
	m.Bytes = 0
	for i, v := range vols {
		logf(log, "rsync %s (checksum=%t)\n", v.Slug, check)
		if err := pass(ctx, src, dst, m.ID, i, v, srcAddr, dstAddr, check); err != nil {
			return err
		}
		if info, err := src.InspectVolume(ctx, v.Name); err == nil && info.SizeBytes > 0 {
			m.Bytes += info.SizeBytes
		}
	}
	return nil
}

func helperName(moveID string, i int, role string) string {
	return fmt.Sprintf("stkr-rsync-%s-%d-%s", moveID, i, role)
}

// retry covers the daemon still starting (rsync exit 10: connection refused).
const clientScript = `i=0; while :; do rsync %s /data/ rsync://%s:%d/data/; rc=$?; if [ $rc -ne 10 ] || [ $i -ge 30 ]; then exit $rc; fi; i=$((i+1)); sleep 1; done`

func pass(ctx context.Context, src, dst Docker, moveID string, i int, v store.Volume, srcAddr, dstAddr string, check bool) error {
	port := RsyncPort + i
	daemon, client := helperName(moveID, i, "d"), helperName(moveID, i, "c")
	// A crash can leave last run's helpers behind under the same names.
	_ = dst.StopRemove(ctx, daemon)
	_ = src.StopRemove(ctx, client)
	did, err := dst.Run(ctx, docker.ContainerSpec{
		Name:    daemon,
		Image:   RsyncImage,
		Cmd:     []string{"daemon"},
		Env:     []string{"RSYNC_ALLOW=" + srcAddr},
		Labels:  map[string]string{docker.LabelManaged: "true"},
		Volumes: []string{v.Name + ":/data"},
		Ports:   map[string]string{fmt.Sprint(port): "873"},
	})
	if err != nil {
		return fmt.Errorf("rsync daemon for %s: %w", v.Slug, err)
	}
	defer func() { _ = dst.StopRemove(context.WithoutCancel(ctx), did) }()

	flags := "-aH --numeric-ids --delete"
	if check {
		flags += " --checksum"
	}
	cid, err := src.Run(ctx, docker.ContainerSpec{
		Name:    client,
		Image:   RsyncImage,
		Cmd:     []string{"sh", "-c", fmt.Sprintf(clientScript, flags, dstAddr, port)},
		Labels:  map[string]string{docker.LabelManaged: "true"},
		Volumes: []string{v.Name + ":/data:ro"},
	})
	if err != nil {
		return fmt.Errorf("rsync client for %s: %w", v.Slug, err)
	}
	defer func() { _ = src.StopRemove(context.WithoutCancel(ctx), cid) }()
	code, err := src.Wait(ctx, cid)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("rsync of %s exited %d", v.Slug, code)
	}
	return nil
}

// dropHelpers removes the rsync helpers of a move, best effort.
func (f *Flow) dropHelpers(ctx context.Context, m store.EnvMove) {
	if !m.Live {
		return
	}
	src, dst, err := f.docks(m)
	if err != nil {
		return
	}
	vols, err := f.Env.Volumes(ctx, m.EnvID)
	if err != nil {
		return
	}
	for i := range vols {
		_ = dst.StopRemove(ctx, helperName(m.ID, i, "d"))
		_ = src.StopRemove(ctx, helperName(m.ID, i, "c"))
	}
}

// safeAddr: the address goes into a shell line.
func safeAddr(a string) bool {
	return a != "" && strings.Trim(a, "0123456789abcdefABCDEF.:") == ""
}

func (f *Flow) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now().UTC()
}

func logf(log io.Writer, format string, a ...any) {
	if log != nil {
		fmt.Fprintf(log, format, a...)
	}
}
