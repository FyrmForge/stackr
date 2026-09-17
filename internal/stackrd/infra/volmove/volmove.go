// Package volmove moves every volume a tile mounts from one node to another.
//
// Docker has no move-volume API and never will: a local volume is a
// directory on one host's disk. This is the two-pass copy VM live migration
// uses, run between the two nodes' agents over the stkr overlay with rsync
// (docs/plans/30-docker-swarm.md, "Moving a volume after the fact"):
//
//  1. the tile keeps running, rsync each volume across, dirty
//  2. stop the tile, rsync each again: only the delta, so seconds
//  3. flip the home node and start on the target
//  4. leave the source volumes alone, and only now
//
// Step 4's ordering is not optional. Deleting the source before the target
// is verified and running is how the volume is lost. If anything fails
// before then, the move aborts with the source volumes untouched and the tile
// back where it was, which is why every failure path below leaves them alone.
//
// Every volume, not the first one found. A tile can mount several volume
// subtiles, and a move that carried one of them set the home node anyway; the
// task came up on the target against an empty directory and swarm called it
// healthy.
package volmove

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/config/settings"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/cluster"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/envnet"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/managedtiles"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/placement"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/workqueue"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Phase is where a move has got to. The modal renders one line per phase.
type Phase string

const (
	PhaseQueued Phase = "queued" // waiting for a free slot
	PhaseCopy   Phase = "copy"   // first pass, tile still running
	PhaseStop   Phase = "stop"   // stopping for the delta pass
	PhaseDelta  Phase = "delta"  // second pass, tile down
	PhaseStart  Phase = "start"  // starting on the target
	PhaseDone   Phase = "done"
	PhaseFailed Phase = "failed"
)

// Move is one volume move in flight.
type Move struct {
	ID     string
	TileID string
	From   string // source node ID
	To     string // target node ID
	// Volumes is every volume the tile mounts, not the first one found. A
	// tile with two volume subtiles that moved only one of them came up
	// healthy on the target reading an empty directory, which is the silent
	// loss placement.go opens by warning about.
	Volumes []string
	Phase   Phase
	Bytes   int64
	Total   int64
	Error   string
	Started time.Time
}

// ETA is the remaining time at the rate achieved so far, and whether there is
// enough information to say. Shown next to the bar; a move with no total yet
// shows bytes only.
func (m Move) ETA() (time.Duration, bool) {
	if m.Total <= 0 || m.Bytes <= 0 || m.Bytes >= m.Total {
		return 0, false
	}
	elapsed := time.Since(m.Started)
	if elapsed <= 0 {
		return 0, false
	}
	rate := float64(m.Bytes) / elapsed.Seconds()
	if rate <= 0 {
		return 0, false
	}
	return time.Duration(float64(m.Total-m.Bytes)/rate) * time.Second, true
}

// Redeployer restarts one tile so it comes up under its new placement. The
// deploy engine behind an interface, because infra/deploy would otherwise
// import this package back.
type Redeployer interface {
	EnqueueCurrent(ctx context.Context, t *repo.Tile, trigger string) (string, error)
}

// Kind is the work-queue kind a move runs under.
const Kind = "volume.move"

// moveJob is the payload: everything Start decided before anything moved.
type moveJob struct {
	TileID  string   `json:"tile_id"`
	From    string   `json:"from"`
	To      string   `json:"to"`
	Volumes []string `json:"volumes"`
}

// moveProgress is the item's progress blob, so the bar survives a restart.
type moveProgress struct {
	Bytes int64 `json:"bytes"`
	Total int64 `json:"total"`
}

// Service runs moves on the work queue. The phase is the item's step and the
// byte counters its progress, so the modal reads the same thing after a
// restart. A restart mid-move fails it and puts the tile back where it
// started (cleanup): resuming a half-move blind is how a volume is lost.
type Service struct {
	Store  repo.Store
	C      *cluster.Cluster // every docker call, and the two agents an rsync runs between
	Deploy Redeployer
	// Instances starts a managed instance again. It needs its own path: an
	// instance is provisioned, never built, so it has no deployment row and
	// the deploy engine's EnqueueCurrent answers "" and no error for it. That
	// read as success and left a moved postgres scaled to zero, still
	// constrained to the node it had just left, while the modal said "Moved".
	Instances *managedtiles.Service

	work *workqueue.Queue

	// moving is the tile ids with a move running. Start's check reads the
	// table and can race a second click; this cannot, and two rsync passes and
	// two home-node flips on one tile is how a volume is lost.
	mu     sync.Mutex
	moving map[string]bool
}

func New(store repo.Store, c *cluster.Cluster, d Redeployer, inst *managedtiles.Service) *Service {
	return &Service{Store: store, C: c, Deploy: d, Instances: inst, moving: map[string]bool{}}
}

func (s *Service) begin(tileID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.moving[tileID] {
		return false
	}
	s.moving[tileID] = true
	return true
}

func (s *Service) end(tileID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.moving, tileID)
}

// WithWork registers the move kind. Call before the queue starts.
func (s *Service) WithWork(q *workqueue.Queue) *Service {
	s.work = q
	q.Register(Kind, s.handle, workqueue.KindOpts{
		// Two rsync passes of an unbounded volume, plus a deploy wait.
		Timeout:   24 * time.Hour,
		Limit:     func(r settings.Resolved) int { return r.VolumeMoveConcurrency },
		OnRestart: workqueue.Fail,
		Cleanup:   s.cleanup,
	})
	return s
}

// finishedShown is how long a finished move stays on the card and in the
// modal, long enough for the UI to have shown how it ended.
const finishedShown = 30 * time.Second

// ForTile returns the move in flight for a tile, or one that finished in the
// last few seconds, or nil. The canvas card and the drawer both read this to
// show the old to new arrow and the bar.
func (s *Service) ForTile(ctx context.Context, tileID string) *Move {
	w, err := s.Store.LatestWorkItem(ctx, Kind, tileID)
	if err != nil || w == nil {
		return nil
	}
	return moveOf(w)
}

// moveOf renders a work item as the Move the UI reads. nil for a move that is
// over and no longer worth showing.
func moveOf(w *repo.WorkItem) *Move {
	var p moveJob
	_ = json.Unmarshal([]byte(w.Payload), &p)
	var pr moveProgress
	_ = json.Unmarshal([]byte(w.Progress), &pr)
	m := &Move{ID: w.ID, TileID: p.TileID, From: p.From, To: p.To, Volumes: p.Volumes,
		Bytes: pr.Bytes, Total: pr.Total, Started: w.CreatedAt}
	if w.StartedAt.Valid {
		m.Started = w.StartedAt.Time
	}
	switch w.Status {
	case "queued":
		m.Phase = PhaseQueued
	case "running":
		m.Phase = Phase(w.Step)
		if m.Phase == "" {
			m.Phase = PhaseCopy
		}
	case "done", "error":
		if !w.FinishedAt.Valid || time.Since(w.FinishedAt.Time) > finishedShown {
			return nil
		}
		m.Phase = PhaseDone
		if w.Status == "error" {
			m.Phase, m.Error = PhaseFailed, w.Error
		}
	default:
		return nil
	}
	return m
}

// volumesOf finds every docker volume a tile's data lives in. The Move modal
// opens from the service tile, but the volumes belong to its volume subtiles;
// a volume tile clicked directly is its own answer.
//
// Every one of them, not the first: a service tile may mount several volume
// subtiles, and moving one while leaving the rest on the old node is data
// loss that swarm reports as a healthy task.
//
// Sorted, so a move copies them in the same order every time and the modal
// lists them the same way twice running.
func (s *Service) volumesOf(ctx context.Context, t *repo.Tile) ([]string, error) {
	if t.IsVolume() {
		return []string{t.DockerVolume()}, nil
	}
	// A managed instance is the third shape: its data volume is named after
	// the tile and there is no volume subtile pointing at it, so the sibling
	// walk below found nothing and the modal told an operator a postgres
	// instance holding twelve databases had "no volume to move", while the
	// plan that opened the modal said it holds one.
	if t.IsManaged() {
		return []string{managedtiles.VolumeName(t)}, nil
	}
	siblings, err := s.Store.ListTilesByEnv(ctx, t.EnvironmentID)
	if err != nil {
		return nil, err
	}
	var vols []string
	for i := range siblings {
		if siblings[i].IsVolume() && siblings[i].AttachedTileID == t.ID {
			vols = append(vols, siblings[i].DockerVolume())
		}
	}
	if len(vols) == 0 {
		return nil, fmt.Errorf("%s has no volume to move", t.Slug)
	}
	slices.Sort(vols)
	return vols, nil
}

// sourceNode is the node a tile's data is actually on.
//
// Not t.HomeNode: a volume tile has none of its own, because the volume is a
// mount on the tile that attaches it. Reading the column directly is how Move
// came to tell an operator that a volume tile holding 238 MB had "no home
// node, so there is nothing to move from".
func (s *Service) sourceNode(ctx context.Context, t *repo.Tile) string {
	return placement.NodeOf(ctx, s.Store, t)
}

// VolumeSize is one volume of a move, named and sized, for the modal.
type VolumeSize struct {
	Name  string
	Bytes int64
}

// Estimate is what the Move modal shows before the operator commits: every
// volume that is about to move, named and sized from the node holding them,
// and the total.
//
// Named rather than only totalled, because the operator has to be able to see
// that a move covers all of them. A single "Volume size" line is what let a
// half-completed move look correct.
func (s *Service) Estimate(ctx context.Context, t *repo.Tile) ([]VolumeSize, int64, error) {
	vols, err := s.volumesOf(ctx, t)
	if err != nil {
		return nil, 0, err
	}
	node := s.sourceNode(ctx, t)
	out := make([]VolumeSize, 0, len(vols))
	var total int64
	for _, v := range vols {
		n, err := s.C.VolumeSize(ctx, node, v)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, VolumeSize{Name: v, Bytes: n})
		total += n
	}
	return out, total, nil
}

// Start queues a move of the tile's volume to target. It returns as soon as
// the move is queued; progress is read back through ForTile.
func (s *Service) Start(ctx context.Context, t *repo.Tile, targetNode string) (*Move, error) {
	from := s.sourceNode(ctx, t)
	if from == "" {
		return nil, fmt.Errorf("%s has no home node, so there is nothing to move from", t.Slug)
	}
	// Empty is not "wherever it is" here. agent.Nodes.IsSelf answers true for
	// the empty string, so an empty target would sail past the reachability
	// check below and resolve to the manager, a node the operator never
	// picked.
	if targetNode == "" {
		return nil, fmt.Errorf("pick a node to move %s to", t.Slug)
	}
	if from == targetNode {
		return nil, fmt.Errorf("%s is already on that node", t.Slug)
	}
	vols, err := s.volumesOf(ctx, t)
	if err != nil {
		return nil, err
	}
	// Both agents have to be up before anything is stopped or copied.
	if !s.C.Reachable(ctx, from) {
		return nil, fmt.Errorf("the agent on the source node is not reachable")
	}
	if !s.C.Reachable(ctx, targetNode) {
		return nil, fmt.Errorf("the agent on the target node is not reachable")
	}

	if s.work == nil {
		return nil, fmt.Errorf("volume moves are not available")
	}
	// One move per tile. The queue only dedupes waiting items, so a move
	// already running has to be refused here.
	if w, err := s.Store.LatestWorkItem(ctx, Kind, t.ID); err != nil {
		return nil, err
	} else if w != nil && !w.Done() {
		return nil, fmt.Errorf("%s is already being moved", t.Slug)
	}
	id, err := s.work.Enqueue(ctx, Kind, t.ID, moveJob{TileID: t.ID, From: from, To: targetNode, Volumes: vols})
	if err != nil {
		return nil, err
	}
	w, err := s.Store.GetWorkItem(ctx, id)
	if err != nil || w == nil {
		return nil, fmt.Errorf("the move was queued but cannot be read back")
	}
	return moveOf(w), nil
}

// handle runs one queued move.
func (s *Service) handle(ctx context.Context, j *workqueue.Job) error {
	var p moveJob
	if err := j.Payload(&p); err != nil {
		return err
	}
	t, err := s.Store.GetTile(ctx, p.TileID)
	if err != nil || t == nil {
		return fmt.Errorf("tile %s is gone", p.TileID)
	}
	if !s.begin(t.ID) {
		return fmt.Errorf("%s is already being moved", t.Slug)
	}
	defer s.end(t.ID)
	m := Move{ID: j.Item.ID, TileID: t.ID, From: p.From, To: p.To, Volumes: p.Volumes}
	if err := s.do(ctx, j, t, m); err != nil {
		slog.Error("volume move failed", "tile", t.Slug, "error", err)
		// A failure anywhere past the stop leaves the service at zero: the
		// paths that scale it back do so themselves, but do() can also return
		// before reaching one of them. Putting it back is safe either way,
		// scaling a running service to one is a no-op.
		back := context.WithoutCancel(ctx)
		if name, nerr := s.serviceName(back, t); nerr == nil && name != "" {
			if serr := s.C.ScaleService(back, name, 1); serr != nil {
				slog.Error("volume move: restarting after a failure", "tile", t.Slug, "error", serr)
			}
		}
		return err
	}
	return nil
}

// cleanup puts back a move a restart interrupted, by the phase it reached.
// Every phase closes the receivers on the target. Once the tile was stopped it
// is started on the source again, and once the home node moved it moves back:
// the source copy is whole until a move finishes, so the source is the safe
// end. Copies on the target stay, as on any failed move.
func (s *Service) cleanup(ctx context.Context, j *workqueue.Job) string {
	var p moveJob
	if err := j.Payload(&p); err != nil {
		return ""
	}
	if dst, err := s.C.Client(ctx, p.To); err == nil {
		for i := range p.Volumes {
			if err := dst.CloseMoveReceiver(ctx, fmt.Sprintf("%s-%d", j.Item.ID, i)); err != nil {
				slog.Error("volume move cleanup: closing the receiver", "move", j.Item.ID, "error", err)
			}
		}
	} else {
		slog.Error("volume move cleanup: target agent", "move", j.Item.ID, "error", err)
	}
	t, err := s.Store.GetTile(ctx, p.TileID)
	if err != nil || t == nil {
		return ""
	}
	name, err := s.serviceName(ctx, t)
	if err != nil || name == "" {
		return ""
	}
	switch Phase(j.Item.Step) {
	case PhaseStart:
		// Already up on the target: every volume arrived before this phase,
		// so the move is done in all but the row.
		if s.runningOn(ctx, name, p.To) {
			return "The panel restarted as the move finished. " + t.Name + " is running on its new node."
		}
		if err := s.C.StopService(ctx, name); err != nil {
			slog.Error("volume move cleanup: stopping", "tile", t.Slug, "error", err)
		}
		if err := s.Store.SetTileHomeNode(ctx, t.ID, p.From); err != nil {
			slog.Error("volume move cleanup: putting the home node back", "tile", t.Slug, "error", err)
			return ""
		}
		t.HomeNode = p.From
		// A managed instance waits for its service to settle, which can
		// outlast the cap boot recovery puts on cleanup.
		go func() {
			if err := s.start(context.Background(), t); err != nil {
				slog.Error("volume move cleanup: starting on the source", "tile", t.Slug, "error", err)
			}
		}()
	case PhaseStop, PhaseDelta:
		if err := s.C.ScaleService(ctx, name, 1); err != nil {
			slog.Error("volume move cleanup: starting on the source", "tile", t.Slug, "error", err)
		}
	}
	return "The panel restarted mid move. " + t.Name + " is back on its old node, and partial copies were left on the target."
}

func (s *Service) do(ctx context.Context, j *workqueue.Job, t *repo.Tile, m Move) error {
	src, err := s.C.Client(ctx, m.From)
	if err != nil {
		return fmt.Errorf("source agent: %w", err)
	}
	dst, err := s.C.Client(ctx, m.To)
	if err != nil {
		return fmt.Errorf("target agent: %w", err)
	}

	// One receiver per volume. The receiver is keyed by the id it is opened
	// with, container name, network alias and the volume bound at /data all
	// come from it, so each volume needs its own, and reusing the move's id
	// for all of them would point every pass at the first volume's daemon.
	legs := make([]leg, len(m.Volumes))
	for i, vol := range m.Volumes {
		legs[i] = leg{vol: vol, id: fmt.Sprintf("%s-%d", m.ID, i)}
	}

	// Opened before anything is copied and closed together, so a failure
	// half way through the set does not leave a daemon holding a volume.
	for _, l := range legs {
		if err := dst.OpenMoveReceiver(ctx, l.id, l.vol); err != nil {
			return fmt.Errorf("opening the target for %s: %w", l.vol, err)
		}
	}
	defer func() {
		for _, l := range legs {
			if err := dst.CloseMoveReceiver(context.WithoutCancel(ctx), l.id); err != nil {
				slog.Error("volume move: closing the receiver", "move", l.id, "volume", l.vol, "error", err)
			}
		}
	}()

	// Progress is the whole set, not the volume in hand: an operator watching
	// a two-volume move should see one bar that fills once. Seeded from the
	// sizes rather than waited for on the wire, see tally.
	tal := newTally(len(legs))
	for i, l := range legs {
		if n, err := s.C.VolumeSize(ctx, m.From, l.vol); err == nil {
			tal.seed(i, n)
		}
	}
	// Written to the row at most once a second: rsync reports far more often
	// than the modal polls. A leg finishing always writes.
	var last time.Time
	publish := func(force bool) {
		if !force && time.Since(last) < time.Second {
			return
		}
		last = time.Now()
		d, tot := tal.sum()
		j.SetProgress(ctx, moveProgress{Bytes: d, Total: tot})
	}
	publish(true)
	progress := func(i int) func(int64, int64) {
		return func(d, tot int64) {
			tal.report(i, d, tot)
			publish(false)
		}
	}
	complete := func(i int) {
		tal.complete(i)
		publish(true)
	}
	j.SetStep(ctx, string(PhaseCopy))

	// Pass one, tile still running. The copies are dirty by definition; that
	// is what pass two is for.
	for i, l := range legs {
		if err := src.SendMove(ctx, l.id, l.vol, runtime.MoveAliasPrefix+l.id, progress(i)); err != nil {
			return fmt.Errorf("first copy of %s: %w", l.vol, err)
		}
		complete(i)
	}

	// Pass two. The tile is down for this and only this.
	j.SetStep(ctx, string(PhaseStop))
	svcName, err := s.serviceName(ctx, t)
	if err != nil {
		return err
	}
	// Waits for the container to be gone, not just for the spec to be posted:
	// the delta rsync below has to run against a volume nothing is writing.
	if err := s.C.StopService(ctx, svcName); err != nil {
		return fmt.Errorf("stopping %s: %w", t.Slug, err)
	}

	j.SetStep(ctx, string(PhaseDelta))
	// Every volume, before the home node moves. Stopping at the first failure
	// leaves the tile on the source with all of its source volumes untouched,
	// which is the safe end: the copies already on the target are fresh
	// volumes nothing mounts.
	for i, l := range legs {
		if err := src.SendMove(ctx, l.id, l.vol, runtime.MoveAliasPrefix+l.id, progress(i)); err != nil {
			// The tile is down and its data is still whole on the source. Put
			// it back up where it was rather than leaving it stopped.
			if scaleErr := s.C.ScaleService(context.WithoutCancel(ctx), svcName, 1); scaleErr != nil {
				slog.Error("volume move: restarting after a failed delta pass", "tile", t.Slug, "error", scaleErr)
			}
			return fmt.Errorf("final sync of %s: %w", l.vol, err)
		}
		complete(i)
	}

	// Only now does the tile belong to the other node, and only because every
	// volume arrived.
	j.SetStep(ctx, string(PhaseStart))
	if err := s.Store.SetTileHomeNode(ctx, t.ID, m.To); err != nil {
		if scaleErr := s.C.ScaleService(context.WithoutCancel(ctx), svcName, 1); scaleErr != nil {
			slog.Error("volume move: restarting after a failed home-node write", "tile", t.Slug, "error", scaleErr)
		}
		return fmt.Errorf("recording the new home node: %w", err)
	}
	t.HomeNode = m.To
	if err := s.start(ctx, t); err != nil {
		back := context.WithoutCancel(ctx)
		// A start that timed out is not a start that failed. On the rig a
		// worker pulling postgres for the first time outlived the 90 second
		// settle wait, the move rolled back, and the task came up on the target
		// a moment later anyway, running on the copy while home_node said the
		// source. The next deploy would have gone back to the stale source copy
		// and dropped every write made in between.
		if s.cameUp(back, svcName, m.To) {
			slog.Warn("volume move: the start timed out but the tile came up on the target", "tile", t.Slug, "error", err)
			// start marked a managed instance "error" on the timeout.
			if t.IsManaged() {
				if serr := s.Store.UpdateTileStatus(back, t.ID, "running"); serr != nil {
					slog.Error("moved instance status not saved", "tile", t.ID, "error", serr)
				}
			}
			return nil
		}
		// The data is on both nodes and the source copy is still whole, so the
		// safe end is where it started. Leaving home_node on the target with a
		// failed deploy left the service stopped on both machines while the
		// modal said Moved (docs/plans/39-codex-review-fixes.md, point 3).
		if rerr := s.rollBack(back, t, svcName, m.From); rerr != nil {
			return fmt.Errorf("starting on the target: %w, and putting it back on the source failed too: %v", err, rerr)
		}
		return fmt.Errorf("starting on the target: %w", err)
	}

	// The source volumes stay. Removing them here would race the deploy that
	// has only just been queued, and a volume deleted before its replacement
	// is proven is a volume gone.
	//
	// Nothing reclaims them later either: Runtime.Prune takes containers,
	// dangling images and build cache, never volumes. They sit on the source
	// node until someone deletes them from its server page, which the Move
	// modal says.
	//
	// no automatic source cleanup. Add one when a "reclaim space
	// on <node>" action exists to hang it off.
	return nil
}

// startGrace is how long a failed start is given to come up on the target
// anyway before the move rolls back: long enough for a node's first pull of a
// large image. A var so the test does not wait it out.
//
// ponytail: fixed wait, read the task's pull progress if a big image ever
// needs longer.
var startGrace = 5 * time.Minute

// cameUp waits up to startGrace for a running task of the service on node.
func (s *Service) cameUp(ctx context.Context, svcName, node string) bool {
	deadline := time.Now().Add(startGrace)
	for {
		if s.runningOn(ctx, svcName, node) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(deployPoll):
		}
	}
}

// runningOn reports a running task of the service on node, right now.
func (s *Service) runningOn(ctx context.Context, svcName, node string) bool {
	tasks, err := s.C.RunningTasks(ctx, svcName)
	if err != nil {
		return false
	}
	for _, task := range tasks {
		if task.NodeID == node {
			return true
		}
	}
	return false
}

// rollBack puts a tile whose start on the target failed back on the source.
//
// Stopped first, so a task that starts on the target late cannot take writes
// that the source copy will never see. Then deployed again, not only scaled:
// the service spec still pins the target, and scaling it back up without a
// redeploy is how the tile ended up running on the target while home_node
// said the source.
func (s *Service) rollBack(ctx context.Context, t *repo.Tile, svcName, from string) error {
	if err := s.C.StopService(ctx, svcName); err != nil {
		return fmt.Errorf("stopping %s: %w", t.Slug, err)
	}
	if err := s.Store.SetTileHomeNode(ctx, t.ID, from); err != nil {
		return fmt.Errorf("putting the home node back: %w", err)
	}
	t.HomeNode = from
	return s.start(ctx, t)
}

// start brings the tile up where its home node says its data is.
//
// A managed instance goes through managedtiles.Deploy, which rebuilds the
// service spec from placement.For, so the node constraint follows the home
// node that was just written and the replica comes back. A service tile goes
// through the deploy engine on the image it already runs.
func (s *Service) start(ctx context.Context, t *repo.Tile) error {
	if t.IsManaged() {
		if s.Instances == nil {
			return fmt.Errorf("no managed-instance deployer wired")
		}
		// The status write belongs to the caller: managedtiles.Deploy does
		// not touch tiles.status, and the drawer's own Deploy handler is
		// where "running" is set. Without it here a moved instance comes up
		// healthy still marked "stopped", which is the one state a config
		// apply will not touch, so the move would look fixed and leave the
		// instance unreachable from the file for ever.
		if err := s.Instances.Deploy(ctx, t); err != nil {
			if serr := s.Store.UpdateTileStatus(ctx, t.ID, "error"); serr != nil {
				slog.Error("moved instance status not saved", "tile", t.ID, "status", "error", "error", serr)
			}
			return err
		}
		return s.Store.UpdateTileStatus(ctx, t.ID, "running")
	}
	id, err := s.Deploy.EnqueueCurrent(ctx, t, "volume-move")
	if err != nil {
		return err
	}
	if id == "" {
		// Nothing to wait on: EnqueueCurrent answers "" for a tile with no
		// deployment of its own.
		return nil
	}
	return s.waitForDeployment(ctx, id)
}

// waitForDeployment blocks until the deployment finishes, and reports anything
// but success as an error.
//
// Enqueue only puts a row in the queue. Returning there let the move go to
// done while the deploy was still pending, so a build that then failed left
// the tile stopped on both nodes with home_node already on the target.
//
// polling, because the deploy engine publishes status on a hub this
// package does not hold. A subscription if a move ever needs to be snappier
// than one second.
func (s *Service) waitForDeployment(ctx context.Context, id string) error {
	tick := time.NewTicker(deployPoll)
	defer tick.Stop()
	// Long enough for a cold image pull on a slow link; a move that is still
	// unresolved after this is not going to resolve itself.
	deadline := time.After(10 * time.Minute)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("deployment %s did not finish within 10 minutes", id)
		case <-tick.C:
		}
		d, err := s.Store.GetDeployment(ctx, id)
		if err != nil {
			return err
		}
		if d == nil {
			return fmt.Errorf("deployment %s vanished", id)
		}
		switch d.Status {
		case "done":
			return nil
		case "error", "cancelled", "stopped":
			if d.Error != "" {
				return fmt.Errorf("deployment %s: %s", d.Status, d.Error)
			}
			return fmt.Errorf("deployment %s", d.Status)
		}
	}
}

// deployPoll is how often waitForDeployment reads the deployment row. A var so
// the test does not spend a second per case.
var deployPoll = time.Second

// leg is one volume's half of a move: the volume, and the id its receiver
// container, network alias and rsync target are all named after.
type leg struct {
	vol string
	id  string
}

// serviceName is the swarm service backing a tile. Volume tiles have none of
// their own, the volume is a mount on the service that attaches it, and
// that is the service to stop.
func (s *Service) serviceName(ctx context.Context, t *repo.Tile) (string, error) {
	target := t
	if t.IsVolume() && t.AttachedTileID != "" {
		att, err := s.Store.GetTile(ctx, t.AttachedTileID)
		if err != nil || att == nil {
			return "", fmt.Errorf("cannot find the tile that mounts %s", t.Slug)
		}
		target = att
	}
	sc, err := envnet.Resolve(ctx, s.Store, target)
	if err != nil {
		return "", err
	}
	return sc.ServiceName(target.Slug), nil
}

// tally accumulates one move's byte counters across its volumes.
//
// It exists because neither end of the wire reports the finish. The agent's
// closing frame is MoveProgress{Done: true}, zero bytes and zero total by
// construction, and rsync may report no intermediate frame at all for a few
// megabytes over a fast link. Between them the modal counted 0 B for a 6 MB
// move and still said 0 B next to "done", and Move.ETA, which divides by
// Bytes, therefore never appeared.
//
// So the totals are seeded from the sizes the modal already measured, and a
// leg that returns without error is counted whole.
type tally struct {
	done  []int64
	total []int64
}

func newTally(n int) *tally {
	return &tally{done: make([]int64, n), total: make([]int64, n)}
}

// seed sets a leg's size before any bytes move, so the denominator is right
// from the first frame rather than growing as the passes report in.
func (t *tally) seed(i int, n int64) { t.total[i] = n }

// report takes a frame from the wire. A zero total is ignored: it would throw
// away the seeded size and put the denominator back to nothing.
func (t *tally) report(i int, done, total int64) {
	t.done[i] = done
	if total > 0 {
		t.total[i] = total
	}
}

// complete counts a finished leg whole, never backwards: the delta pass
// reports fewer bytes than the first pass and must not shrink the bar.
func (t *tally) complete(i int) {
	if t.done[i] < t.total[i] {
		t.done[i] = t.total[i]
	}
}

func (t *tally) sum() (done, total int64) {
	for i := range t.done {
		done += t.done[i]
		total += t.total[i]
	}
	return done, total
}
