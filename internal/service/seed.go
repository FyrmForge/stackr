package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// seedJob deploys a new operated tile and its slice, then loads a dump
// into the slice's database.
type seedJob struct {
	TileID  string     `json:"tile_id"`
	SliceID string     `json:"slice_id"`
	Format  string     `json:"format"` // mflow.DumpCustom | mflow.DumpPlain
	Source  seedSource `json:"source"`
}

// seedSource is where a seed's dump comes from. Kind "upload" is a file
// spooled under the data dir; kind "slice" is another slice's database,
// dumped and loaded in one stream (runSeed). A backup run would be a new
// kind here and a case in open.
type seedSource struct {
	Kind    string `json:"kind"`
	Path    string `json:"path,omitempty"`
	SliceID string `json:"slice_id,omitempty"` // kind slice: the slice tile copied
	At      string `json:"at,omitempty"`       // kind slice: as of this time (RFC3339 UTC, or latest); "" = now
}

// open is the source's dump; closing it lets an upload's file go.
func (s seedSource) open() (io.ReadCloser, error) {
	if s.Kind != "upload" {
		return nil, fmt.Errorf("seed source %q is not known", s.Kind)
	}
	f, err := os.Open(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errs.Conflictf("the uploaded dump is gone; create the tile again from it")
	}
	return f, err
}

// drop removes what the source left on disk.
func (s seedSource) drop() {
	if s.Kind == "upload" {
		_ = os.Remove(s.Path)
	}
}

// CreateOperatedTileFromDump makes a postgres operated tile and slice tile
// slice cut from it ("" = <tile>-db), then queues the job that deploys
// both, waits for postgres and loads dump into the slice's database. The
// dump goes to disk as it streams in, never whole into memory; an empty
// one or a format stackr does not load is refused before anything is made.
func (o *Orchestrator) CreateOperatedTileFromDump(
	ctx context.Context,
	t Tile,
	engine, slice string,
	dump io.Reader,
) (Tile, Job, error) {
	if engine != "postgres" {
		return Tile{}, Job{}, errs.Invalidf("engine", "only postgres starts from a dump")
	}
	if err := o.gateEnv(ctx, t.EnvironmentID, true); err != nil {
		return Tile{}, Job{}, err
	}
	br := bufio.NewReaderSize(dump, 1<<16)
	head, _ := br.Peek(mflow.SniffLen)
	format, err := mflow.SniffDump(head)
	if err != nil {
		return Tile{}, Job{}, err
	}
	limit, err := o.SeedLimit(ctx)
	if err != nil {
		return Tile{}, Job{}, err
	}
	src, err := o.spool(br, limit)
	if err != nil {
		return Tile{}, Job{}, err
	}
	if slice == "" {
		slice = "-"
	}
	out, sl, err := o.createOperated(ctx, t, engine, slice)
	if err != nil {
		src.drop()
		return out, Job{}, err
	}
	j, err := o.enqueue(ctx, kindSeed, seedJob{TileID: out.ID, SliceID: sl.ID, Format: format, Source: src}, out.ID, sl.ID)
	if err != nil {
		src.drop()
	}
	return out, j, err
}

// spool writes an upload under <data>/seeds for the seed job to read.
// The job drops it when it ends however it ends (seedEnded), and the boot
// sweep (sweepSeeds) takes whatever a crash left.
func (o *Orchestrator) spool(r io.Reader, limit int64) (seedSource, error) {
	dir := filepath.Join(o.cfg.DataDir, "seeds")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return seedSource{}, err
	}
	f, err := os.CreateTemp(dir, "*.dump")
	if err != nil {
		return seedSource{}, err
	}
	src := seedSource{Kind: "upload", Path: f.Name()}
	n, err := io.Copy(f, io.LimitReader(r, limit+1))
	if err = errors.Join(err, f.Close()); err != nil {
		src.drop()
		return seedSource{}, fmt.Errorf("receiving the dump: %w", err)
	}
	if n > limit {
		src.drop()
		return seedSource{}, errs.TooLarge{Msg: "That dump is larger than this server takes."}
	}
	return src, nil
}

// seedReserve is the free space an upload leaves on the data disk.
const seedReserve = 1 << 30

// SeedLimit is the most bytes an upload may be: the seed_max_gb knob, cut to
// the data disk's free space less seedReserve (an unreadable disk is not
// judged).
func (o *Orchestrator) SeedLimit(ctx context.Context) (int64, error) {
	gbs, err := o.settings.Int(ctx, "seed_max_gb")
	if err != nil {
		return 0, err
	}
	limit := int64(gbs) << 30
	if free, total := o.dataDirDisk(); total > 0 {
		room := int64(min(free, 1<<62)) - seedReserve
		if room <= 0 {
			return 0, errs.TooLarge{Msg: "This server has no room for a dump."}
		}
		limit = min(limit, room)
	}
	return limit, nil
}

// runSeed deploys the instance (which waits until postgres answers), then
// the slice (its database and owner), then loads the dump. The dump is
// dropped once the job ends, kept only while it waits on an approval.
func (o *Orchestrator) runSeed(ctx context.Context, r *jobs.Run, p seedJob) (err error) {
	defer func() {
		if _, parked := errs.IsNeedsApproval(err); !parked {
			p.Source.drop()
		}
	}()
	ctx, err = o.withTileNode(ctx, p.TileID)
	if err != nil {
		return err
	}
	o.ensureMesh(ctx)
	ids := []string{p.TileID, p.SliceID}
	if p.Source.Kind == "slice" {
		ids = ids[1:] // a branch goes onto an instance that is already running
	}
	for _, id := range ids {
		if err := o.deploy.Redeploy(ctx, id, r.Log, r.Swap); err != nil {
			o.downAfterFailedDeploy(ctx, id, err)
			return o.parkOnApproval(ctx, r, err)
		}
	}
	if err := o.afterDeploy(ctx, ids, false); err != nil {
		return err
	}
	s, err := o.tiles.Get(ctx, p.SliceID)
	if err != nil {
		return err
	}
	if p.Source.Kind == "slice" && p.Source.At != "" {
		return o.seedFromSliceAt(ctx, r, s, p)
	}
	if p.Source.Kind == "slice" {
		return o.seedFromSlice(ctx, r, s, p)
	}
	dump, err := p.Source.open()
	if err != nil {
		return err
	}
	defer func() { _ = dump.Close() }()
	return o.engines.Seed(ctx, s, p.Format, dump, r.Log)
}

// seedFromSlice pipes pg_dump of the source slice, run on its instance,
// into the load of s: no file. A failure on either side fails the job.
func (o *Orchestrator) seedFromSlice(ctx context.Context, r *jobs.Run, s Tile, p seedJob) error {
	src, err := o.tiles.Get(ctx, p.Source.SliceID)
	if err != nil {
		return err
	}
	srcInst, err := o.postgresOf(ctx, src)
	if err != nil {
		return err
	}
	dctx, err := o.withTileNode(ctx, srcInst.ID) // the dump runs where the source does, the load where s does
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(r.Log, "dumping %s\n", src.Slug)
	out, wait, err := o.engines.DumpSlice(dctx, src)
	if err != nil {
		return err
	}
	err = o.engines.Seed(ctx, s, p.Format, out, r.Log)
	// wait closes the pipe first, so a load that stopped early frees the dump
	return errors.Join(err, wait())
}

// seedEnded lets a finished seed job's dump go: one cancelled or
// superseded before its handler ran never reached runSeed's drop.
func seedEnded(j store.Job) {
	if j.Kind != string(kindSeed) {
		return
	}
	var p seedJob
	if json.Unmarshal([]byte(j.Payload), &p) == nil {
		p.Source.drop()
	}
}

// sweepSeeds removes every file in <data>/seeds no live seed job names.
// It runs at boot before the job runner starts, so no seed reads one now;
// a running row there is one Start fails, and seedEnded drops its file.
func (o *Orchestrator) sweepSeeds(ctx context.Context) error {
	dir := filepath.Join(o.cfg.DataDir, "seeds")
	es, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	live, err := o.jobRows.List(ctx, job.Queued, job.Waiting, job.Running)
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, j := range live {
		var p seedJob
		if j.Kind == string(kindSeed) && json.Unmarshal([]byte(j.Payload), &p) == nil && p.Source.Path != "" {
			// by name: the data dir may be spelled another way this boot
			keep[filepath.Base(p.Source.Path)] = true
		}
	}
	for _, e := range es {
		if e.IsDir() || keep[e.Name()] {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("seeds: dump not removed", "file", e.Name(), "err", err)
		}
	}
	return nil
}
