// Package backup writes database dumps, docker volumes, and the panel's own
// SQLite file to S3-compatible destinations, restores from them, prunes old
// archives, and runs the schedules that trigger all of it.
//
// Every archive is spooled to a local scratch file before it is uploaded. That
// is what makes the volume modes work: the container is only frozen for the
// length of the local tar, never for the length of the upload. It also gives
// restore something to verify before it wipes anything.
package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"sort"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/robfig/cron/v3"

	"github.com/FyrmForge/stackr/internal/stackrd/config/secrets"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/cluster"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/envnet"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/managedtiles"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// defaultRegion is what an S3-compatible server that ignores regions still
// needs the signer to name.
const defaultRegion = "us-east-1"

type Service struct {
	store repo.Store
	dbs   *managedtiles.Service
	// c is every docker call. A tile's volume and its container are on one
	// node's disk; a backup taken through the manager's own socket for a tile
	// running on a worker tars an empty volume and reports success. The
	// cluster decides the node, and refuses to guess it.
	c       *cluster.Cluster
	db      *sqlx.DB // the panel's own database, for VACUUM INTO
	dataDir string
	version string // build version, written into the panel archive

	mu      sync.Mutex
	cron    *cron.Cron
	entries map[string]cron.EntryID // backupID -> entry
	running map[string]bool         // backupID mid-run
}

// on is the node a tile's data lives on, or an error naming the tile.
func (s *Service) on(ctx context.Context, t *repo.Tile) (string, error) {
	return s.c.NodeOf(ctx, t)
}

func NewService(store repo.Store, dbs *managedtiles.Service, c *cluster.Cluster, db *sqlx.DB, dataDir, version string) *Service {
	s := &Service{
		store: store, dbs: dbs, c: c, db: db, dataDir: dataDir, version: version,
		cron:    cron.New(),
		entries: map[string]cron.EntryID{},
		running: map[string]bool{},
	}
	s.cron.Start()
	return s
}

// LoadSchedules (re)registers cron entries for every enabled backup. Call it
// at startup and after any change to a schedule.
func (s *Service) LoadSchedules(ctx context.Context) error {
	all, err := s.store.ListBackups(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.entries {
		s.cron.Remove(id)
	}
	s.entries = map[string]cron.EntryID{}
	for _, b := range all {
		if !b.Enabled || b.Cron == "" {
			continue
		}
		b := b
		id, err := s.cron.AddFunc(b.Schedule(), func() {
			_, _ = s.Run(context.Background(), b.ID, "schedule")
		})
		if err != nil {
			continue // invalid expression; ValidateCron guards new saves
		}
		s.entries[b.ID] = id
	}
	return nil
}

// --- S3 ---

func client(d *repo.BackupDestination) *s3.Client {
	region := d.Region
	if region == "" {
		region = defaultRegion
	}
	return s3.New(s3.Options{
		Region:       region,
		BaseEndpoint: aws.String(d.Endpoint),
		UsePathStyle: true, // MinIO/RustFS/Garage; real S3 accepts it too
		Credentials:  credentials.NewStaticCredentialsProvider(d.AccessKey, d.SecretKey, ""),
	})
}

// TestDestination verifies a destination by writing and deleting a probe.
func TestDestination(ctx context.Context, d *repo.BackupDestination) error {
	cl := client(d)
	key := ".stackr-probe"
	body := []byte("ok")
	if _, err := cl.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(d.Bucket), Key: aws.String(key),
		Body: bytes.NewReader(body), ContentLength: aws.Int64(int64(len(body))),
	}); err != nil {
		return err
	}
	_, err := cl.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(d.Bucket), Key: aws.String(key)})
	return err
}

// prefixFor is the object-key root for one backup config. It is derived, never
// supplied: prune deletes everything under it beyond the keep count, so a
// caller-chosen prefix on a shared bucket would be a cross-tenant delete.
func (s *Service) prefixFor(ctx context.Context, b *repo.Backup) string {
	if b.Kind == repo.BackupStackr {
		return path.Join("stackr", "_panel", b.ID)
	}
	t, err := s.store.GetTile(ctx, b.TileID.String)
	if err != nil || t == nil {
		return path.Join("stackr", "_orphan", b.ID)
	}
	orgID := ""
	if st, err := s.store.GetStack(ctx, t.StackID); err == nil && st != nil {
		orgID = st.OrgID
	}
	return path.Join("stackr", orgID, t.StackID, t.ID, b.ID)
}

// --- running one backup ---

// Run executes one backup end to end: produce the archive locally, upload it,
// prune what falls outside the keep count. trigger is recorded on the run row
// ("schedule", "manual", or "pre-restore").
func (s *Service) Run(ctx context.Context, backupID, trigger string) (*repo.BackupRun, error) {
	// hamr caps every request context at 30 seconds, and a manual run is
	// triggered from a request. Cut loose from it: a tar and an upload measured
	// in minutes must not die halfway, least of all a restore's pre-backup.
	// the caller still blocks for the whole run, detach the web
	// trigger if a long backup holding a browser request open matters.
	ctx = context.WithoutCancel(ctx)
	b, err := s.store.GetBackup(ctx, backupID)
	if err != nil || b == nil {
		return nil, fmt.Errorf("backup not found")
	}
	if !s.begin(b.ID) {
		return nil, fmt.Errorf("a run for this backup is already in progress")
	}
	defer s.end(b.ID)
	return s.run(ctx, b, trigger)
}

// begin claims a backup id for the caller, false when something else holds it.
// Restore holds the same claim across its whole flow, so a second restore
// cannot start a parallel wipe-and-extract on one volume, which is what the
// detached context makes reachable, since a browser that gives up on a slow
// restore leaves it running and invites a second click.
func (s *Service) begin(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running[id] {
		return false
	}
	s.running[id] = true
	return true
}

func (s *Service) end(id string) {
	s.mu.Lock()
	delete(s.running, id)
	s.mu.Unlock()
}

// run is Run without the claim, so a restore can reuse it for its pre-restore
// backup while still holding the claim itself.
func (s *Service) run(ctx context.Context, b *repo.Backup, trigger string) (*repo.BackupRun, error) {
	dest, err := s.store.GetBackupDestination(ctx, b.DestinationID)
	if err != nil || dest == nil {
		return nil, fmt.Errorf("destination not found")
	}

	run := &repo.BackupRun{
		ID: uuid.New().String(), BackupID: b.ID, Trigger: trigger,
		Status: "running", CreatedAt: time.Now().UTC(),
	}
	if err := s.store.CreateBackupRun(ctx, run); err != nil {
		return nil, err
	}
	finish := func(err error, key string, size int64) (*repo.BackupRun, error) {
		run.FinishedAt = sql.NullTime{Time: time.Now().UTC(), Valid: true}
		if err != nil {
			run.Status, run.Error = "error", err.Error()
		} else {
			run.Status, run.ObjectKey, run.SizeBytes = "done", key, size
		}
		// context.Background: the run row must be closed out even when the
		// request that started it has already gone away.
		if uerr := s.store.UpdateBackupRun(context.Background(), run); uerr != nil {
			slog.Error("backup run not closed out", "run", run.ID, "status", run.Status, "error", uerr)
		}
		return run, err
	}

	name, produce, err := s.producer(ctx, b)
	if err != nil {
		return finish(err, "", 0)
	}
	f, err := s.scratchFile(b.ID)
	if err != nil {
		return finish(err, "", 0)
	}
	defer func() { _ = os.Remove(f.Name()); _ = f.Close() }()

	if err := produce(f); err != nil {
		return finish(err, "", 0)
	}
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return finish(err, "", 0)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return finish(err, "", 0)
	}

	key := path.Join(s.prefixFor(ctx, b), fmt.Sprintf("%s-%s", time.Now().UTC().Format("20060102-150405"), name))
	if _, err := client(dest).PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(dest.Bucket), Key: aws.String(key),
		Body: f, ContentLength: aws.Int64(size), ContentType: aws.String("application/gzip"),
	}); err != nil {
		return finish(fmt.Errorf("upload: %w", err), "", 0)
	}
	// Never prune on the pre-restore run: it is the newest archive under the
	// prefix, so pruning to the keep count would delete the oldest, which,
	// when the oldest is the archive being restored, destroys it before the
	// download and leaves the user with neither.
	if trigger != "pre-restore" {
		s.prune(ctx, b, dest)
	}
	return finish(nil, key, size)
}

// producer returns the archive's file name and a function that writes it.
func (s *Service) producer(ctx context.Context, b *repo.Backup) (string, func(io.Writer) error, error) {
	switch b.Kind {
	case repo.BackupStackr:
		return "stackr.tar.gz", s.writePanelArchive, nil
	case repo.BackupDump:
		t, err := s.tile(ctx, b)
		if err != nil {
			return "", nil, err
		}
		return t.Slug + ".dump.gz", func(w io.Writer) error { return s.writeDump(ctx, t, w) }, nil
	case repo.BackupVolume:
		t, err := s.tile(ctx, b)
		if err != nil {
			return "", nil, err
		}
		vol, err := VolumeFor(t)
		if err != nil {
			return "", nil, err
		}
		return t.Slug + ".tar.gz", func(w io.Writer) error { return s.writeVolume(ctx, b, t, vol, w) }, nil
	}
	return "", nil, fmt.Errorf("unknown backup kind %q", b.Kind)
}

func (s *Service) tile(ctx context.Context, b *repo.Backup) (*repo.Tile, error) {
	t, err := s.store.GetTile(ctx, b.TileID.String)
	if err != nil || t == nil {
		return nil, fmt.Errorf("tile not found")
	}
	return t, nil
}

// VolumeFor resolves the docker volume a tile's data lives in. A service tile
// has none of its own, its data sits in the volume tiles attached to it, and
// those are what carry the backup config.
func VolumeFor(t *repo.Tile) (string, error) {
	switch {
	case t.IsVolume():
		return t.DockerVolume(), nil
	case t.IsManaged():
		return managedtiles.VolumeName(t), nil
	}
	return "", fmt.Errorf("%s has no volume of its own; back up its attached volumes instead", t.Name)
}

// writeDump execs the engine's dump command in the running container and
// gzips its stdout into w.
func (s *Service) writeDump(ctx context.Context, t *repo.Tile, w io.Writer) error {
	eng, ok := managedtiles.Engines[t.Engine]
	if !ok || eng.DumpCmd == nil {
		return fmt.Errorf("dumps are not supported for %s", t.Engine)
	}
	cid, err := s.dbs.ContainerID(ctx, t)
	if err != nil || cid == "" {
		return fmt.Errorf("database container is not running")
	}
	node, err := s.on(ctx, t)
	if err != nil {
		return err
	}
	rd, wait, err := s.c.ExecStream(ctx, node, cid, eng.DumpCmd(t), nil)
	if err != nil {
		return err
	}
	// wait on every path, not just the happy one: it is what reaps the exec
	// and reports a dump that died halfway. Returning early on a copy error
	// left it running and the volume held open.
	gz := gzip.NewWriter(w)
	if _, err := io.Copy(gz, rd); err != nil {
		_ = wait()
		return err
	}
	if err := gz.Close(); err != nil {
		_ = wait()
		return err
	}
	return wait()
}

// writeVolume tars the volume into w under the config's container mode:
// pause (freeze, the default), stop, or live (no interference, torn copies
// possible). The container is always released before the caller uploads.
func (s *Service) writeVolume(ctx context.Context, b *repo.Backup, t *repo.Tile, vol string, w io.Writer) error {
	node, err := s.on(ctx, t)
	if err != nil {
		return err
	}
	if err := s.checkScratchSpace(ctx, node, vol); err != nil {
		return err
	}
	// Both before the freeze: a pull inside the pause window is downtime.
	if err := s.c.EnsureVolumeTool(ctx); err != nil {
		return err
	}
	cids, err := s.containersFor(ctx, t)
	if err != nil {
		return err
	}
	switch b.ContainerMode {
	case repo.ModeStop:
		resume, err := s.quiesce(ctx, t)
		if err != nil {
			return err
		}
		defer resume()
	case repo.ModeLive:
		// nothing to do, the documented torn-copy mode
	default: // pause
		for _, cid := range cids {
			if err := s.c.PauseContainer(ctx, node, cid); err != nil {
				return fmt.Errorf("pausing container: %w", err)
			}
		}
		defer func() {
			for _, cid := range cids {
				_ = s.c.UnpauseContainer(context.Background(), node, cid)
			}
		}()
	}
	return s.c.TarVolume(ctx, node, vol, w, b.ContainerMode == repo.ModeLive)
}

// containersFor lists the running containers holding a tile's volume open. A
// volume tile's data is written by the app it is attached to, so that is what
// gets frozen, not the volume tile, which has no container of its own.
// quiesce takes the tile that owns this volume down for the length of the
// copy and returns the undo. Stopping the task's container is not enough
// under swarm: the service starts a replacement within seconds, which remounts
// the volume in the middle of the tar, a torn backup, or a restore writing
// under a live process. Scaling the service to zero is the only stop that
// holds, and it is the only stop used here: managed instances are services
// too.
func (s *Service) quiesce(ctx context.Context, t *repo.Tile) (func(), error) {
	target := t
	if t.IsVolume() && t.AttachedTileID != "" {
		if att, err := s.store.GetTile(ctx, t.AttachedTileID); err == nil && att != nil {
			target = att
		}
	}
	name := envnet.ServiceFor(ctx, s.store, target)
	if name == "" {
		return func() {}, nil
	}
	// StopService, not ScaleService: the scale call returns as soon as the
	// spec is posted, and a database goes on writing for its whole stop grace
	// after that. Tarring or restoring under a live writer is how a backup
	// comes back green and torn.
	if err := s.c.StopService(ctx, name); err != nil {
		return nil, fmt.Errorf("stopping service: %w", err)
	}
	return func() { _ = s.c.ScaleService(context.Background(), name, 1) }, nil
}

func (s *Service) containersFor(ctx context.Context, t *repo.Tile) ([]string, error) {
	target := t
	if t.IsVolume() && t.AttachedTileID != "" {
		att, err := s.store.GetTile(ctx, t.AttachedTileID)
		if err != nil {
			return nil, err
		}
		if att != nil {
			target = att
		}
	}
	// Swarm task state, not a label lookup on the local socket. The socket
	// only ever answers for the manager, so a tile running on a worker came
	// back with no containers at all, nothing was frozen and the tar ran
	// against a volume that is not on this machine.
	tasks, err := s.c.RunningTasks(ctx, envnet.ServiceFor(ctx, s.store, target))
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, task := range tasks {
		if task.ContainerID != "" {
			ids = append(ids, task.ContainerID)
		}
	}
	return ids, nil
}

// writePanelArchive writes the panel's whole restorable state: the database,
// the master key that decrypts everything in it, and the build version that
// wrote it.
//
// The key is in the archive on purpose. Without it every secret, database
// password and deploy key in the database is ciphertext no other host can
// read, and the backup would report success while losing all of them. The
// cost is that the archive is as sensitive as the panel itself: whoever holds
// it holds the keys. Operators who care encrypt the bucket.
//
// The version is what restore.sh prints before it touches anything, so an
// operator sees an archive written by a different build than the one running.
func (s *Service) writePanelArchive(w io.Writer) error {
	if s.db == nil {
		return fmt.Errorf("panel database is not available")
	}
	// VACUUM INTO takes the copy inside a read transaction, so it is safe
	// while the panel is serving traffic. Copying the file directly is not.
	tmp := path.Join(s.scratchDir(), "panel-"+uuid.New().String()+".db")
	defer func() { _ = os.Remove(tmp) }()
	if _, err := s.db.Exec(`VACUUM INTO ?`, tmp); err != nil {
		return fmt.Errorf("vacuum into: %w", err)
	}
	f, err := os.Open(tmp)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	// tar needs the size up front, so the database is streamed rather than
	// read in: the panel database is the one archive member with no ceiling
	// on it.
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	key := secrets.Key()
	if key == "" {
		// Refuse rather than ship an archive that cannot be restored. A
		// silent half-backup is the whole failure this replaced.
		return fmt.Errorf("master key is not loaded, refusing to write a panel backup that cannot be restored")
	}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	now := time.Now().UTC()
	if err := tw.WriteHeader(&tar.Header{
		Name: "stackr.db", Mode: 0o600, Size: fi.Size(), ModTime: now,
	}); err != nil {
		return err
	}
	if _, err := io.Copy(tw, f); err != nil {
		return err
	}
	// keys/master.key is the path Load reads it back from, so the archive
	// unpacks straight over the data dir with no moving afterwards.
	small := []struct {
		name string
		mode int64
		body []byte
	}{
		{"keys/master.key", 0o600, []byte(key + "\n")},
		{"VERSION", 0o644, []byte(s.version + "\n")},
	}
	for _, m := range small {
		if err := tw.WriteHeader(&tar.Header{
			Name: m.name, Mode: m.mode, Size: int64(len(m.body)), ModTime: now,
		}); err != nil {
			return err
		}
		if _, err := tw.Write(m.body); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// WritePanelArchiveTo writes the panel archive to a local file. It lands under
// a temporary name first, so a failed write never leaves a truncated archive
// that looks restorable.
func (s *Service) WritePanelArchiveTo(p string) error {
	if err := os.MkdirAll(path.Dir(p), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(path.Dir(p), path.Base(p)+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if err := s.writePanelArchive(f); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), p)
}

// --- scratch space ---

func (s *Service) scratchDir() string { return path.Join(s.dataDir, "backups") }

func (s *Service) scratchFile(backupID string) (*os.File, error) {
	dir := s.scratchDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return os.CreateTemp(dir, backupID+"-*")
}

// checkScratchSpace fails the run before anything is frozen when the archive
// plainly will not fit. The volume size is uncompressed and docker reports it
// as -1 when it does not know, so this only catches the obvious cases.
func (s *Service) checkScratchSpace(ctx context.Context, node, vol string) error {
	// Asked of the node that holds it: the manager's own daemon knows nothing
	// about a volume on a worker, and its "no such volume" would silently
	// skip the check.
	size, err := s.c.VolumeSize(ctx, node, vol)
	if err != nil || size < 0 {
		return nil
	}
	info := struct{ SizeBytes int64 }{SizeBytes: size}
	if err := os.MkdirAll(s.scratchDir(), 0o700); err != nil {
		return err
	}
	free, err := freeBytes(s.scratchDir())
	if err != nil || free == 0 {
		return nil
	}
	if free < uint64(info.SizeBytes) {
		return fmt.Errorf("not enough scratch space in %s: %s free, volume holds %s",
			s.scratchDir(), runtime.FmtBytes(int64(free)), runtime.FmtBytes(info.SizeBytes))
	}
	return nil
}

// --- retention ---

// prune deletes everything under this backup's prefix beyond the newest
// KeepLatest archives. Keys are timestamp-first, so lexical order is
// chronological order.
func (s *Service) prune(ctx context.Context, b *repo.Backup, dest *repo.BackupDestination) {
	if b.KeepLatest <= 0 {
		return
	}
	cl := client(dest)
	prefix := s.prefixFor(ctx, b) + "/"
	var keys []string
	var token *string
	for {
		out, err := cl.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket: aws.String(dest.Bucket), Prefix: aws.String(prefix), ContinuationToken: token,
		})
		if err != nil {
			return
		}
		for _, o := range out.Contents {
			if o.Key != nil {
				keys = append(keys, *o.Key)
			}
		}
		if out.NextContinuationToken == nil {
			break
		}
		token = out.NextContinuationToken
	}
	sort.Strings(keys)
	for i := 0; i < len(keys)-b.KeepLatest; i++ {
		_, _ = cl.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(dest.Bucket), Key: aws.String(keys[i])})
	}
}

// --- restore ---

// Restore puts one recorded run back. The run is loaded by id and must belong
// to this backup, the object key is never taken from the caller, because on a
// shared bucket that would be a cross-tenant read.
//
// Volume restores are destructive and ordered so that every recoverable
// failure happens before the wipe: take a pre-restore backup, download,
// verify, and only then stop → wipe → extract → start.
func (s *Service) Restore(ctx context.Context, backupID, runID string) error {
	ctx = context.WithoutCancel(ctx) // see Run: a restore must not die mid-wipe
	b, err := s.store.GetBackup(ctx, backupID)
	if err != nil || b == nil {
		return fmt.Errorf("backup not found")
	}
	// Held for the whole flow, pre-restore backup included: two concurrent
	// restores would run two `rm -rf` + untar passes over one volume.
	if !s.begin(b.ID) {
		return fmt.Errorf("a run or restore for this backup is already in progress")
	}
	defer s.end(b.ID)
	run, err := s.store.GetBackupRun(ctx, runID)
	if err != nil || run == nil || run.BackupID != b.ID {
		return fmt.Errorf("backup run not found")
	}
	if run.Status != "done" || run.ObjectKey == "" {
		return fmt.Errorf("that run did not produce an archive")
	}
	if b.Kind == repo.BackupStackr {
		return fmt.Errorf("a panel backup is restored on the host with scripts/restore.sh, not from here: the archive carries the master key and has to land in the data dir before stackr starts")
	}
	dest, err := s.store.GetBackupDestination(ctx, b.DestinationID)
	if err != nil || dest == nil {
		return fmt.Errorf("destination not found")
	}
	t, err := s.tile(ctx, b)
	if err != nil {
		return err
	}
	// The way back if the archive turns out to hold the wrong contents, and
	// what the confirm dialog promises. Both kinds: a dump restore is
	// `pg_dump | psql` over a live database, which is every bit as
	// irreversible as untarring over a volume. A failure here aborts the
	// restore, going on would leave no way back.
	if _, err := s.run(ctx, b, "pre-restore"); err != nil {
		return fmt.Errorf("pre-restore backup failed, restore aborted: %w", err)
	}
	if b.Kind == repo.BackupDump {
		return s.restoreDump(ctx, t, dest, run.ObjectKey)
	}
	return s.restoreVolume(ctx, b, t, dest, run.ObjectKey)
}

func (s *Service) restoreDump(ctx context.Context, t *repo.Tile, dest *repo.BackupDestination, key string) error {
	eng, ok := managedtiles.Engines[t.Engine]
	if !ok || eng.RestoreCmd == nil {
		return fmt.Errorf("restore is not supported for %s", t.Engine)
	}
	cid, err := s.dbs.ContainerID(ctx, t)
	if err != nil || cid == "" {
		return fmt.Errorf("database container is not running")
	}
	obj, err := s.download(ctx, dest, key)
	if err != nil {
		return err
	}
	defer func() { _ = obj.Close() }()
	gz, err := gzip.NewReader(obj)
	if err != nil {
		return fmt.Errorf("corrupt archive: %w", err)
	}
	node, err := s.on(ctx, t)
	if err != nil {
		return err
	}
	rd, wait, err := s.c.ExecStream(ctx, node, cid, eng.RestoreCmd(t), gz)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, rd)
	return wait()
}

func (s *Service) restoreVolume(ctx context.Context, b *repo.Backup, t *repo.Tile, dest *repo.BackupDestination, key string) error {
	vol, err := VolumeFor(t)
	if err != nil {
		return err
	}
	obj, err := s.download(ctx, dest, key)
	if err != nil {
		return err
	}
	f, err := s.scratchFile(b.ID + "-restore")
	if err != nil {
		_ = obj.Close()
		return err
	}
	defer func() { _ = os.Remove(f.Name()); _ = f.Close() }()
	_, err = io.Copy(f, obj)
	_ = obj.Close()
	if err != nil {
		return err
	}
	if err := s.c.VerifyTar(ctx, f.Name()); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}

	resume, err := s.quiesce(ctx, t)
	if err != nil {
		return err
	}
	defer resume()
	node, err := s.on(ctx, t)
	if err != nil {
		return err
	}
	return s.c.UntarVolume(ctx, node, vol, f)
}

func (s *Service) download(ctx context.Context, dest *repo.BackupDestination, key string) (io.ReadCloser, error) {
	out, err := client(dest).GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(dest.Bucket), Key: aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	return out.Body, nil
}
