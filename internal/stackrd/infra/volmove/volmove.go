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
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/cluster"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/envnet"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/managedtiles"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/placement"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Phase is where a move has got to. The modal renders one line per phase.
type Phase string

const (
	PhaseCopy   Phase = "copy"  // first pass, tile still running
	PhaseStop   Phase = "stop"  // stopping for the delta pass
	PhaseDelta  Phase = "delta" // second pass, tile down
	PhaseStart  Phase = "start" // starting on the target
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

// Service runs moves and remembers the ones in flight.
//
// In memory on purpose: a panel restart mid-move leaves the source volume
// intact and the tile where it started, which is the safe end of the failure.
// Persisting a half-move would mean resuming one, and a resume that guesses
// wrong deletes a volume.
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

	mu    sync.Mutex
	moves map[string]*Move // by tile id, one at a time per tile
}

func New(store repo.Store, c *cluster.Cluster, d Redeployer, inst *managedtiles.Service) *Service {
	return &Service{Store: store, C: c, Deploy: d, Instances: inst, moves: map[string]*Move{}}
}

// ForTile returns the move in flight for a tile, or nil. The canvas card and
// the drawer both read this to show the old to new arrow and the bar.
func (s *Service) ForTile(tileID string) *Move {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.moves[tileID]; ok {
		cp := *m
		return &cp
	}
	return nil
}

// All returns every move in flight.
func (s *Service) All() []Move {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Move, 0, len(s.moves))
	for _, m := range s.moves {
		out = append(out, *m)
	}
	return out
}

func (s *Service) update(tileID string, f func(*Move)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.moves[tileID]; ok {
		f(m)
	}
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

// Start begins a move of the tile's volume to target. It returns as soon as
// the move is registered; progress is read back through ForTile.
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

	s.mu.Lock()
	if _, busy := s.moves[t.ID]; busy {
		s.mu.Unlock()
		return nil, fmt.Errorf("%s is already being moved", t.Slug)
	}
	m := &Move{
		ID: uuid.New().String(), TileID: t.ID, From: from, To: targetNode,
		Volumes: vols, Phase: PhaseCopy, Started: time.Now(),
	}
	s.moves[t.ID] = m
	s.mu.Unlock()

	tile := *t
	go s.run(context.WithoutCancel(ctx), &tile, *m)
	cp := *m
	return &cp, nil
}

func (s *Service) run(ctx context.Context, t *repo.Tile, m Move) {
	if err := s.do(ctx, t, m); err != nil {
		slog.Error("volume move failed", "tile", t.Slug, "error", err)
		s.update(t.ID, func(mv *Move) { mv.Phase, mv.Error = PhaseFailed, err.Error() })
		// A failure anywhere past the stop leaves the service at zero: the
		// paths that scale it back do so themselves, but do() can also return
		// before reaching one of them. Putting it back is safe either way,
		// scaling a running service to one is a no-op.
		if name, nerr := s.serviceName(context.WithoutCancel(ctx), t); nerr == nil && name != "" {
			if serr := s.C.ScaleService(context.WithoutCancel(ctx), name, 1); serr != nil {
				slog.Error("volume move: restarting after a failure", "tile", t.Slug, "error", serr)
			}
		}
		// Left in the map so the UI can show why, and cleared on the same
		// timer as a success: a failed move that never leaves the map blocks
		// every later move of that tile with "is already being moved", with
		// no way to clear it short of restarting the panel.
		s.forget(t.ID, PhaseFailed)
		return
	}
	s.update(t.ID, func(mv *Move) { mv.Phase = PhaseDone })
	s.forget(t.ID, PhaseDone)
}

// forget drops a finished move from the map after a pause long enough for the
// UI to have shown how it ended. Only if it is still in the phase it finished
// in: a new move started in the meantime owns the entry now.
func (s *Service) forget(tileID string, phase Phase) {
	go func() {
		time.Sleep(30 * time.Second)
		s.mu.Lock()
		if mv, ok := s.moves[tileID]; ok && mv.Phase == phase {
			delete(s.moves, tileID)
		}
		s.mu.Unlock()
	}()
}

func (s *Service) do(ctx context.Context, t *repo.Tile, m Move) error {
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
	publish := func() {
		d, tot := tal.sum()
		s.update(t.ID, func(mv *Move) { mv.Bytes, mv.Total = d, tot })
	}
	publish()
	progress := func(i int) func(int64, int64) {
		return func(d, tot int64) {
			tal.report(i, d, tot)
			publish()
		}
	}
	complete := func(i int) {
		tal.complete(i)
		publish()
	}

	// Pass one, tile still running. The copies are dirty by definition; that
	// is what pass two is for.
	for i, l := range legs {
		if err := src.SendMove(ctx, l.id, l.vol, runtime.MoveAliasPrefix+l.id, progress(i)); err != nil {
			return fmt.Errorf("first copy of %s: %w", l.vol, err)
		}
		complete(i)
	}

	// Pass two. The tile is down for this and only this.
	s.update(t.ID, func(mv *Move) { mv.Phase = PhaseStop })
	svcName, err := s.serviceName(ctx, t)
	if err != nil {
		return err
	}
	// Waits for the container to be gone, not just for the spec to be posted:
	// the delta rsync below has to run against a volume nothing is writing.
	if err := s.C.StopService(ctx, svcName); err != nil {
		return fmt.Errorf("stopping %s: %w", t.Slug, err)
	}

	s.update(t.ID, func(mv *Move) { mv.Phase = PhaseDelta })
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
	s.update(t.ID, func(mv *Move) { mv.Phase = PhaseStart })
	if err := s.Store.SetTileHomeNode(ctx, t.ID, m.To); err != nil {
		if scaleErr := s.C.ScaleService(context.WithoutCancel(ctx), svcName, 1); scaleErr != nil {
			slog.Error("volume move: restarting after a failed home-node write", "tile", t.Slug, "error", scaleErr)
		}
		return fmt.Errorf("recording the new home node: %w", err)
	}
	t.HomeNode = m.To
	if err := s.startOnTarget(ctx, t); err != nil {
		// The data is on both nodes and the source copy is still whole, so the
		// safe end is where it started. Leaving home_node on the target with a
		// failed deploy left the service stopped on both machines while the
		// modal said Moved (docs/plans/39-codex-review-fixes.md, point 3).
		back := context.WithoutCancel(ctx)
		if e := s.Store.SetTileHomeNode(back, t.ID, m.From); e != nil {
			slog.Error("volume move: putting the home node back", "tile", t.Slug, "error", e)
		} else {
			t.HomeNode = m.From
		}
		if e := s.C.ScaleService(back, svcName, 1); e != nil {
			slog.Error("volume move: restarting on the source after a failed start", "tile", t.Slug, "error", e)
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

// startOnTarget brings the tile up where its data now is.
//
// A managed instance goes through managedtiles.Deploy, which rebuilds the
// service spec from placement.For, so the node constraint follows the home
// node that was just written and the replica comes back. A service tile goes
// through the deploy engine on the image it already runs.
func (s *Service) startOnTarget(ctx context.Context, t *repo.Tile) error {
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
