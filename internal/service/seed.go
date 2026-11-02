package service

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/managed"
)

// seedJob deploys a new managed tile and its slice, then loads a dump
// into the slice's database.
type seedJob struct {
	TileID  string     `json:"tile_id"`
	SliceID string     `json:"slice_id"`
	Format  string     `json:"format"` // mflow.DumpCustom | mflow.DumpPlain
	Source  seedSource `json:"source"`
}

// seedSource is where a seed's dump comes from. Kind "upload" is a file
// spooled under the data dir; a backup run or another tile's dump is a
// new kind here and a case in open.
type seedSource struct {
	Kind string `json:"kind"`
	Path string `json:"path,omitempty"`
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

// CreateManagedTileFromDump makes a postgres managed tile and slice tile
// slice cut from it ("" = <tile>-db), then queues the job that deploys
// both, waits for postgres and loads dump into the slice's database. The
// dump goes to disk as it streams in, never whole into memory; an empty
// one or a format stackr does not load is refused before anything is made.
func (o *Orchestrator) CreateManagedTileFromDump(
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
	out, sl, err := o.createManaged(ctx, t, engine, slice)
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
// ponytail: a seed job cancelled before it runs leaves its file; sweep
// <data>/seeds when that shows up.
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
	for _, id := range ids {
		if err := o.deploy.Redeploy(ctx, id, r.Log, r.Swap); err != nil {
			o.downAfterFailedDeploy(ctx, id, err)
			return parkOnApproval(r, err)
		}
	}
	if err := o.afterDeploy(ctx, ids, false); err != nil {
		return err
	}
	s, err := o.tiles.Get(ctx, p.SliceID)
	if err != nil {
		return err
	}
	dump, err := p.Source.open()
	if err != nil {
		return err
	}
	defer func() { _ = dump.Close() }()
	return o.engines.Seed(ctx, s, p.Format, dump, r.Log)
}
