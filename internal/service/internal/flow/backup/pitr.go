package backup

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	bk "github.com/FyrmForge/stackr/internal/service/internal/leaf/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Point-in-time recovery: the shipper moves finished WAL segments from the
// instance's archive volume to its backup destination, and the window
// retention prunes what a recovery can no longer use. The layout and the
// rules about names are leaf/backup's (pitr.go).

// shipMax caps the segments in one bundle, so a long outage ships in several
// objects and an argv stays short.
const shipMax = 256

// Ship is one instance's archive, as the caller resolved it.
type Ship struct {
	Engine store.Tile
	Dest   store.BackupDest // with its archive key
	Root   string           // leaf/backup PITRRoot
	Dir    string           // the archive dir inside the container
}

// Shipped is what one tick did and saw.
type Shipped struct {
	Bundles  int
	Bytes    int64  // shipped now, as stored
	Last     string // newest segment shipped now, "" when none went
	Waiting  int    // files still in the archive dir
	DirBytes int64  // the archive dir's size when the tick started
}

// run execs argv in the engine's first running replica and returns stdout.
func (f *Flow) run(ctx context.Context, t store.Tile, argv []string) (string, error) {
	out, wait, err := f.Tiles.Stream(ctx, t, argv, nil)
	if err != nil {
		return "", err
	}
	b, rerr := io.ReadAll(out)
	if werr := wait(); werr != nil {
		return "", werr
	}
	return string(b), rerr
}

// Guard refuses an archive that already holds objects this panel has no run
// for: a copy of an instance (a restore, a clone of its volume) must never
// write into the history of the one it came from. Empty, or the instance's
// own, passes.
func (f *Flow) Guard(ctx context.Context, d store.BackupDest, root string) error {
	keys, err := open(d).List(ctx, root+"/")
	if err != nil || len(keys) == 0 {
		return err
	}
	own, err := f.Backups.Owns(ctx, root, d.ID)
	if err != nil || own {
		return err
	}
	return errs.Refusedf("the archive at %s already holds objects this panel has no backup of; "+
		"another instance wrote there, so archiving will not start into it", root)
}

// ErrNoArchive: the running container has no archive dir, so it does not
// archive (not yet redeployed with PITR on, or already redeployed with it off).
var ErrNoArchive = errors.New("the container has no archive directory")

// ShipWAL ships what the archive dir holds, once. Segments go in bundles of
// up to shipMax, <first>-<last>.tar.gz.age under wal/; timeline history files
// in a bundle of their own. A bundle's files are removed only after its Put
// succeeded. On an error the files stay and the next tick ships them again
// (same names, same keys).
func (f *Flow) ShipWAL(ctx context.Context, s Ship, log io.Writer) (Shipped, error) {
	var res Shipped
	out, err := f.run(ctx, s.Engine, []string{"ls", "-1", s.Dir})
	if err != nil {
		return res, fmt.Errorf("list the archive: %w: %w", ErrNoArchive, err)
	}
	var segs, hist []string
	for _, n := range strings.Fields(out) {
		switch {
		case !bk.WALName(n):
		case bk.IsHistory(n):
			hist = append(hist, n)
		default:
			segs = append(segs, n)
		}
	}
	slices.Sort(segs)
	slices.Sort(hist)
	res.Waiting = len(segs) + len(hist)
	if du, err := f.run(ctx, s.Engine, []string{"du", "-sk", s.Dir}); err == nil {
		if cols := strings.Fields(du); len(cols) > 0 {
			if kb, err := strconv.ParseInt(cols[0], 10, 64); err == nil {
				res.DirBytes = kb << 10
			}
		}
	}
	var bundles [][]string
	// A bundle never spans timelines (the names sort by timeline first), so
	// restore reads each bundle's timeline off its first segment.
	for len(segs) > 0 {
		n := 1
		for n < len(segs) && n < shipMax && segs[n][:8] == segs[0][:8] {
			n++
		}
		bundles = append(bundles, segs[:n])
		segs = segs[n:]
	}
	if len(hist) > 0 {
		bundles = append(bundles, hist)
	}
	for _, files := range bundles {
		first, last := files[0], files[len(files)-1]
		if seg := bk.Segment(first); seg != "" {
			first, last = seg, bk.Segment(last)
		}
		key := s.Root + "/" + bk.WALDir + "/" + first + "-" + last + bk.BundleExt
		n, err := f.shipBundle(ctx, s, key, files, true)
		if err != nil {
			return res, err
		}
		res.Bundles++
		res.Bytes += n
		res.Waiting -= len(files)
		if seg := bk.Segment(last); seg != "" {
			res.Last = max(res.Last, seg)
		}
		logf(log, "shipped %d file(s) as %s\n", len(files), key)
	}
	return res, nil
}

// shipBundle tars files from the archive dir on the instance's node, gzips
// and encrypts them with the destination's key, and puts them under key; only
// then are the files removed.
func (f *Flow) shipBundle(ctx context.Context, s Ship, key string, files []string, remove bool) (int64, error) {
	argv := append([]string{"tar", "-C", s.Dir, "-c"}, files...)
	produce := func(ctx context.Context, w io.Writer) error {
		enc, err := encrypt(w, s.Dest.ArchiveKey, keyWorkFactor)
		if err != nil {
			return err
		}
		out, wait, err := f.Tiles.Stream(ctx, s.Engine, argv, nil)
		if err != nil {
			return err
		}
		gz := gzip.NewWriter(enc)
		if _, err := io.Copy(gz, out); err != nil {
			_ = wait()
			return err
		}
		if err := gz.Close(); err != nil {
			_ = wait()
			return err
		}
		if err := wait(); err != nil { // tar died halfway: no object
			return err
		}
		return enc.Close()
	}
	size, err := f.stream(ctx, open(s.Dest), key, produce)
	if err != nil {
		return 0, err
	}
	if !remove {
		return size, nil
	}
	rm := []string{"rm", "-f"}
	for _, n := range files {
		rm = append(rm, s.Dir+"/"+n)
	}
	if _, err := f.run(ctx, s.Engine, rm); err != nil {
		return size, fmt.Errorf("remove the shipped files: %w", err)
	}
	return size, nil
}

// ShipHistory ships the timeline history files in s.Dir (a restored cluster's
// pg_wal) and leaves them there: the cluster needs them itself. A promoted
// restore calls it so the archive can follow the new timeline; the restored
// copy never archived, so nothing else would. It refuses when the archive
// already holds the copy's timeline: another copy of the same source was
// promoted first, and shipping would overwrite its history.
func (f *Flow) ShipHistory(ctx context.Context, s Ship, log io.Writer) error {
	return f.history(ctx, s, log, false)
}

// CheckHistory refuses what ShipHistory would refuse, and ships nothing.
func (f *Flow) CheckHistory(ctx context.Context, s Ship) error {
	return f.history(ctx, s, io.Discard, true)
}

func (f *Flow) history(ctx context.Context, s Ship, log io.Writer, dry bool) error {
	out, err := f.run(ctx, s.Engine, []string{"ls", "-1", s.Dir})
	if err != nil {
		return fmt.Errorf("list %s: %w", s.Dir, err)
	}
	var hist []string
	for _, n := range strings.Fields(out) {
		if bk.IsHistory(n) {
			hist = append(hist, n)
		}
	}
	slices.Sort(hist)
	if len(hist) == 0 {
		return nil
	}
	// A restored copy picks its new timeline from the histories it saw at
	// restore time, so two copies of one source end on the same number. The
	// archive holding that history already means another one was promoted.
	tl := historyTimeline(hist[len(hist)-1])
	keys, err := open(s.Dest).List(ctx, s.Root+"/"+bk.WALDir+"/")
	if err != nil {
		return fmt.Errorf("list the archive: %w", err)
	}
	for _, k := range keys {
		if n, ok := bundleHistoryTimeline(k); ok && n >= tl {
			return errs.Conflictf("the archive already holds timeline %d (another restored copy was promoted); restore again from the current state", n)
		}
	}
	if dry {
		return nil
	}
	key := s.Root + "/" + bk.WALDir + "/" + hist[0] + "-" + hist[len(hist)-1] + bk.BundleExt
	if _, err := f.shipBundle(ctx, s, key, hist, false); err != nil {
		return err
	}
	logf(log, "shipped the timeline history (%s) as %s\n", strings.Join(hist, ", "), key)
	return nil
}

// historyTimeline is the timeline a 00000002.history name stands for.
func historyTimeline(file string) int {
	n, _ := strconv.ParseUint(strings.TrimSuffix(file, ".history"), 16, 32)
	return int(n)
}

// bundleHistoryTimeline is the newest timeline a history bundle key holds
// (<first>.history-<last>.history<ext>); ok is false for any other key.
func bundleHistoryTimeline(key string) (int, bool) {
	name, found := strings.CutSuffix(key[strings.LastIndex(key, "/")+1:], bk.BundleExt)
	if !found || !strings.Contains(name, ".history") {
		return 0, false
	}
	last := name[strings.LastIndex(name, "-")+1:]
	if !bk.IsHistory(last) {
		return 0, false
	}
	return historyTimeline(last), true
}

// Usage is how many WAL bundles the archive holds and their bytes together.
func (f *Flow) Usage(ctx context.Context, d store.BackupDest, root string) (n int, size int64, err error) {
	o := open(d)
	if u, ok := o.(interface {
		Usage(context.Context, string) (int, int64, error)
	}); ok {
		return u.Usage(ctx, root+"/"+bk.WALDir+"/")
	}
	keys, err := o.List(ctx, root+"/"+bk.WALDir+"/")
	return len(keys), 0, err
}

// PruneWindow applies the recovery window after a good base backup: bases
// outside it (keeping the anchor) go, then the WAL bundles that end before
// the oldest kept base started. History bundles stay. n is the objects
// deleted. No good base, nothing goes.
func (f *Flow) PruneWindow(ctx context.Context, d store.BackupDest, root string, days int, now time.Time, log io.Writer) (int, error) {
	if days <= 0 {
		return 0, nil
	}
	if f.held(root) { // a restore is reading this archive: its bases and WAL stay
		logf(log, "not pruning %s: a restore is reading it\n", root)
		return 0, nil
	}
	bases, keys, lin, err := f.ListArchive(ctx, d, root, time.Time{})
	if err != nil {
		return 0, err
	}
	drop, walBefore, ok := bk.Window(bases, lin, now, days)
	if !ok {
		return 0, nil
	}
	o := open(d)
	n := 0
	for _, r := range drop {
		if err := o.Delete(ctx, r.ObjectKey); err != nil {
			logf(log, "not pruned %s: %v\n", r.ObjectKey, err)
			continue
		}
		_ = f.Backups.DropRun(ctx, r.ID)
		n++
	}
	for _, k := range bk.StaleWAL(keys, walBefore) {
		if err := o.Delete(ctx, k); err != nil {
			logf(log, "not pruned %s: %v\n", k, err)
			continue
		}
		n++
	}
	return n, nil
}
