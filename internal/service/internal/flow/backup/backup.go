// Package backup runs volume backups and restores and the panel
// self-backup. Archives are gzip, then age, then spooled to a local scratch
// file and uploaded: a frozen container waits only for the local tar, and a
// restore has a whole file to verify before anything is wiped.
package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"filippo.io/age"

	"github.com/FyrmForge/stackr/internal/service/errs"
	bk "github.com/FyrmForge/stackr/internal/service/internal/leaf/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/s3"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type Flow struct {
	Backups *bk.Leaf
	Volumes *volume.Leaf
	Tiles   *tile.Leaf
	Scratch string // spool dir, e.g. <data>/backups/scratch

	mu      sync.Mutex
	running map[string]bool // volume id → claimed
}

// Subject is one volume as the caller resolved it: who holds it open (to
// pause or stop) and, for an engine that dumps, the argv and where to run it.
type Subject struct {
	Volume  store.Volume
	Holders []store.Tile // tiles whose replicas mount the volume
	Engine  store.Tile   // the managed tile Dump and Load exec in
	Dump    []string     // nil = tar the volume
	Load    []string     // restore argv, reads the dump on stdin
}

func (s Subject) dumps() bool { return len(s.Dump) > 0 }

// Spec is one backup: where it goes and how it is kept.
type Spec struct {
	Dest       store.BackupDest
	Prefix     string  // from leaf/backup Prefix/OrphanPrefix, never a caller's
	ScheduleID *string // nil = manual, pre-restore or orphan
	Trigger    string  // schedule | manual | pre-restore | orphan
	Mode       string  // pause | stop | live; ignored when the subject dumps
	Keep       int
}

// Backup claims the volume, writes one archive and prunes the schedule's
// prefix. The run row records the outcome either way.
func (f *Flow) Backup(ctx context.Context, s Subject, sp Spec, log io.Writer) (store.BackupRun, error) {
	if !f.claim(s.Volume.ID) {
		return store.BackupRun{}, errs.Conflictf("a backup or restore of %s is already running", s.Volume.Slug)
	}
	defer f.release(s.Volume.ID)
	return f.backup(ctx, s, sp, log)
}

func (f *Flow) backup(ctx context.Context, s Subject, sp Spec, log io.Writer) (store.BackupRun, error) {
	r, err := f.Backups.Start(ctx, bk.KindVolume, &s.Volume.ID, sp.ScheduleID, sp.Dest.ID, sp.Trigger)
	if err != nil {
		return r, err
	}
	key, size, err := f.write(ctx, s, sp, log)
	if r, ferr := f.Backups.Finish(ctx, r, key, size, err); ferr != nil || err != nil {
		return r, errors.Join(err, ferr)
	}
	// Never on the pre-restore run: it is the newest under the prefix, and
	// pruning to keep could delete the very archive about to be restored.
	if sp.Trigger != bk.PreRestore {
		if gone := f.Backups.Prune(ctx, open(sp.Dest), sp.Prefix, sp.Keep); len(gone) > 0 {
			logf(log, "pruned %d old archive(s)\n", len(gone))
		}
	}
	return f.Backups.GetRun(ctx, r.ID)
}

// write produces the archive into a spool file and uploads it.
func (f *Flow) write(ctx context.Context, s Subject, sp Spec, log io.Writer) (string, int64, error) {
	name, ext := s.Volume.Slug, ".tar.gz.age"
	if s.dumps() {
		ext = ".sql.gz.age"
	} else if err := f.checkSpace(ctx, s.Volume); err != nil {
		return "", 0, err
	}
	spool, err := f.spool(s.Volume.ID)
	if err != nil {
		return "", 0, err
	}
	defer drop(spool)
	enc, err := encrypt(spool, sp.Dest.ArchiveKey, keyWorkFactor)
	if err != nil {
		return "", 0, err
	}
	if s.dumps() {
		err = f.dump(ctx, s, enc, log)
	} else {
		err = f.tar(ctx, s, sp.Mode, enc, log)
	}
	if err == nil {
		err = enc.Close()
	}
	if err != nil {
		return "", 0, err
	}
	size, err := spool.Seek(0, io.SeekCurrent)
	if err != nil {
		return "", 0, err
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return "", 0, err
	}
	key := bk.ObjectKey(sp.Prefix, name+ext, time.Now())
	logf(log, "uploading %s (%d bytes)\n", key, size)
	if err := open(sp.Dest).Put(ctx, key, spool); err != nil {
		return "", 0, fmt.Errorf("upload: %w", err)
	}
	return key, size, nil
}

// dump gzips the engine's dump from the running instance into w. wait runs
// on every path: it reaps the exec and reports a dump that died halfway.
func (f *Flow) dump(ctx context.Context, s Subject, w io.Writer, log io.Writer) error {
	logf(log, "dumping %s\n", s.Engine.Slug)
	out, wait, err := f.Tiles.Stream(ctx, s.Engine, s.Dump, nil)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(w)
	if _, err := io.Copy(gz, out); err != nil {
		_ = wait()
		return err
	}
	if err := gz.Close(); err != nil {
		_ = wait()
		return err
	}
	return wait()
}

// tar freezes the holders by mode and tars the volume (the helper gzips).
func (f *Flow) tar(ctx context.Context, s Subject, mode string, w io.Writer, log io.Writer) error {
	// Before the freeze: a pull inside the pause window is downtime.
	if err := f.Volumes.EnsureTool(ctx); err != nil {
		return err
	}
	switch mode {
	case bk.Live:
		logf(log, "copying %s live (a torn copy is possible)\n", s.Volume.Slug)
	case bk.Stop:
		logf(log, "stopping holders of %s\n", s.Volume.Slug)
		resume, err := f.Tiles.Quiesce(ctx, s.Holders)
		if err != nil {
			return err
		}
		defer resume()
	default:
		logf(log, "pausing holders of %s\n", s.Volume.Slug)
		thaw, err := f.Tiles.Freeze(ctx, s.Holders)
		if err != nil {
			return err
		}
		defer thaw()
	}
	return f.Volumes.Tar(ctx, s.Volume, w, mode == bk.Live)
}

// Restore is the input of a restore job: a run and where it lands. The
// target may be another volume of the same kind (cross-volume restore).
type Restore struct {
	RunID, SourceVolumeID string
	Target                Subject
	Pre                   Spec // the pre-restore backup of the target
}

// Restore, in the order that makes every recoverable failure come first:
// claim the target, check the run, back the target up (the way back),
// download, verify, then stop, wipe and untar (or load the dump), start.
func (f *Flow) Restore(ctx context.Context, in Restore, log io.Writer) error {
	t := in.Target
	if !f.claim(t.Volume.ID) {
		return errs.Conflictf("a backup or restore of %s is already running", t.Volume.Slug)
	}
	defer f.release(t.Volume.ID)
	r, err := f.Backups.Restorable(ctx, in.RunID, in.SourceVolumeID)
	if err != nil {
		return err
	}
	if strings.HasSuffix(r.ObjectKey, ".sql.gz.age") != t.dumps() {
		return errs.Refusedf("that backup is not the same kind as %s", t.Volume.Slug)
	}
	dest, err := f.Backups.Get(ctx, r.DestID)
	if err != nil {
		return err
	}
	pre := in.Pre
	pre.Trigger, pre.ScheduleID = bk.PreRestore, nil
	logf(log, "backing up %s before the restore\n", t.Volume.Slug)
	if _, err := f.backup(ctx, t, pre, log); err != nil {
		return fmt.Errorf("pre-restore backup (nothing was touched): %w", err)
	}

	logf(log, "downloading %s\n", r.ObjectKey)
	plain, err := f.fetch(ctx, dest, r.ObjectKey, t.Volume.ID)
	if err != nil {
		return err
	}
	defer drop(plain)
	if err := verify(plain, !t.dumps()); err != nil {
		return fmt.Errorf("archive check (nothing was touched): %w", err)
	}
	if _, err := plain.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if t.dumps() {
		gz, err := gzip.NewReader(plain)
		if err != nil {
			return err
		}
		logf(log, "loading the dump into %s\n", t.Engine.Slug)
		out, wait, err := f.Tiles.Stream(ctx, t.Engine, t.Load, gz)
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, out)
		return wait()
	}
	logf(log, "stopping holders of %s\n", t.Volume.Slug)
	resume, err := f.Tiles.Quiesce(ctx, t.Holders)
	if err != nil {
		return err
	}
	defer resume()
	logf(log, "restoring into %s\n", t.Volume.Slug)
	return f.Volumes.Untar(ctx, t.Volume, plain)
}

// fetch downloads and decrypts an archive into a spool file.
func (f *Flow) fetch(ctx context.Context, d store.BackupDest, key, id string) (*os.File, error) {
	obj, err := open(d).Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = obj.Close() }()
	dec, err := decrypt(obj, d.ArchiveKey)
	if err != nil {
		return nil, err
	}
	spool, err := f.spool(id + "-restore")
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(spool, dec); err != nil {
		drop(spool)
		return nil, fmt.Errorf("download: %w", err)
	}
	return spool, nil
}

// verify reads the archive end to end: a gzip stream, and for a volume a
// tar whose headers all parse. A truncated download fails here.
func verify(f *os.File, isTar bool) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("not a gzip archive: %w", err)
	}
	if !isTar {
		_, err = io.Copy(io.Discard, gz)
		return err
	}
	tr := tar.NewReader(gz)
	for {
		if _, err := tr.Next(); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return fmt.Errorf("corrupt archive: %w", err)
		}
	}
}

// Orphans backs every expired orphan up to dest, then deletes it. A volume
// whose backup fails is kept for the next run.
func (f *Flow) Orphans(ctx context.Context, retention time.Duration, dest store.BackupDest, log io.Writer) error {
	vs, err := f.Volumes.Expired(ctx, retention, time.Now())
	if err != nil {
		return err
	}
	var failed []error
	for _, v := range vs {
		if f.Volumes.HoldsData(ctx, v) {
			sp := Spec{
				Dest:    dest,
				Prefix:  bk.OrphanPrefix(v.ID),
				Trigger: "orphan",
				Mode:    bk.Live,
				Keep:    1,
			}
			if _, err := f.Backup(ctx, Subject{Volume: v}, sp, log); err != nil {
				failed = append(failed, fmt.Errorf("%s: %w", v.Slug, err))
				continue
			}
		}
		if err := f.Volumes.Delete(ctx, v, nil); err != nil {
			failed = append(failed, fmt.Errorf("%s: %w", v.Slug, err))
			continue
		}
		logf(log, "deleted orphan %s\n", v.Slug)
	}
	return errors.Join(failed...)
}

// Panel is the self-backup's facts: a consistent copy of the database
// (VACUUM INTO, taken by the store), the master key, the build, and the
// recovery passphrase the installer printed.
type Panel struct {
	Vacuum     func(ctx context.Context, path string) error
	MasterKey  string
	Version    string
	Passphrase string
	InstallID  string
}

// PanelBackup writes the panel archive (stackr.db, keys/master.key,
// VERSION) encrypted with the recovery passphrase. Admin-only; restored on
// the host with the restore script, never from the panel.
func (f *Flow) PanelBackup(
	ctx context.Context,
	p Panel,
	dest store.BackupDest,
	keep int,
	log io.Writer,
) (store.BackupRun, error) {
	r, err := f.Backups.Start(ctx, bk.KindPanel, nil, nil, dest.ID, "manual")
	if err != nil {
		return r, err
	}
	key, size, err := f.writePanel(ctx, p, dest)
	if r, ferr := f.Backups.Finish(ctx, r, key, size, err); ferr != nil || err != nil {
		return r, errors.Join(err, ferr)
	}
	prefix := bk.PanelPrefix(p.InstallID)
	f.Backups.Prune(ctx, open(dest), prefix, keep)
	logf(log, "panel backup %s\n", key)
	return f.Backups.GetRun(ctx, r.ID)
}

func (f *Flow) writePanel(ctx context.Context, p Panel, dest store.BackupDest) (string, int64, error) {
	if p.MasterKey == "" {
		// A backup that cannot be restored must not report success.
		return "", 0, errors.New("the master key is not loaded; refusing a panel backup that cannot be restored")
	}
	if p.Passphrase == "" {
		return "", 0, errors.New("no recovery passphrase is set")
	}
	if err := os.MkdirAll(f.Scratch, 0o700); err != nil {
		return "", 0, err
	}
	db := filepath.Join(f.Scratch, fmt.Sprintf("panel-%d.db", time.Now().UnixNano()))
	defer func() { _ = os.Remove(db) }()
	if err := p.Vacuum(ctx, db); err != nil {
		return "", 0, fmt.Errorf("vacuum into: %w", err)
	}
	spool, err := f.spool("panel")
	if err != nil {
		return "", 0, err
	}
	defer drop(spool)
	enc, err := encrypt(spool, p.Passphrase, 0)
	if err != nil {
		return "", 0, err
	}
	if err := panelTar(enc, db, p); err != nil {
		return "", 0, err
	}
	if err := enc.Close(); err != nil {
		return "", 0, err
	}
	size, _ := spool.Seek(0, io.SeekCurrent)
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return "", 0, err
	}
	key := bk.ObjectKey(bk.PanelPrefix(p.InstallID), "panel.tar.gz.age", time.Now())
	return key, size, open(dest).Put(ctx, key, spool)
}

// panelTar: keys/master.key is where the panel reads it back from, so the
// archive unpacks straight over the data dir.
func panelTar(w io.Writer, db string, p Panel) error {
	f, err := os.Open(db)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	now := time.Now().UTC()
	if err := tw.WriteHeader(&tar.Header{
		Name:    "stackr.db",
		Mode:    0o600,
		Size:    fi.Size(),
		ModTime: now,
	}); err != nil {
		return err
	}
	if _, err := io.Copy(tw, f); err != nil {
		return err
	}
	for _, m := range []struct {
		name string
		mode int64
		body string
	}{
		{"keys/master.key", 0o600, p.MasterKey + "\n"},
		{"VERSION", 0o644, p.Version + "\n"},
	} {
		if err := tw.WriteHeader(&tar.Header{
			Name:    m.name,
			Mode:    m.mode,
			Size:    int64(len(m.body)),
			ModTime: now,
		}); err != nil {
			return err
		}
		if _, err := io.WriteString(tw, m.body); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// keyWorkFactor: a destination key is 256 random bits, so scrypt's cost buys
// nothing there; the human recovery passphrase keeps age's default.
const keyWorkFactor = 10

func encrypt(w io.Writer, pass string, workFactor int) (io.WriteCloser, error) {
	r, err := age.NewScryptRecipient(pass)
	if err != nil {
		return nil, err
	}
	if workFactor > 0 {
		r.SetWorkFactor(workFactor)
	}
	return age.Encrypt(w, r)
}

func decrypt(src io.Reader, pass string) (io.Reader, error) {
	id, err := age.NewScryptIdentity(pass)
	if err != nil {
		return nil, err
	}
	out, err := age.Decrypt(src, id)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	return out, nil
}

// open is the destination's object store; local and S3 share every path.
func open(d store.BackupDest) s3.Destination {
	if d.Kind == bk.Local {
		return s3.Local{Dir: d.Endpoint}
	}
	return s3.Bucket{
		Endpoint:  d.Endpoint,
		Region:    d.Region,
		Bucket:    d.Bucket,
		AccessKey: d.AccessKey,
		SecretKey: d.SecretKey,
	}
}

func (f *Flow) claim(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running == nil {
		f.running = map[string]bool{}
	}
	if f.running[id] {
		return false
	}
	f.running[id] = true
	return true
}

func (f *Flow) release(id string) {
	f.mu.Lock()
	delete(f.running, id)
	f.mu.Unlock()
}

func (f *Flow) spool(id string) (*os.File, error) {
	if err := os.MkdirAll(f.Scratch, 0o700); err != nil {
		return nil, err
	}
	return os.CreateTemp(f.Scratch, id+"-*")
}

func drop(f *os.File) {
	_ = f.Close()
	_ = os.Remove(f.Name())
}

// checkSpace fails before anything is frozen when the archive plainly will
// not fit. Unknown sizes pass: this only catches the obvious case.
func (f *Flow) checkSpace(ctx context.Context, v store.Volume) error {
	size := f.Volumes.SizeBytes(ctx, v)
	if size <= 0 {
		return nil
	}
	if err := os.MkdirAll(f.Scratch, 0o700); err != nil {
		return err
	}
	var st syscall.Statfs_t
	if syscall.Statfs(f.Scratch, &st) != nil {
		return nil
	}
	if free := st.Bavail * uint64(st.Bsize); free > 0 && free < uint64(size) {
		return fmt.Errorf("not enough scratch space in %s: %d bytes free, the volume holds %d", f.Scratch, free, size)
	}
	return nil
}

func logf(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
