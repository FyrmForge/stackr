# Extract: `infra/backup`

Source: `infra/backup/` — backup.go, work.go, space.go, authz.go, backup_test.go, panelarchive_test.go
Commit: c2423f0
Taken: S3 client/put/get/list, scratch spooling and preflight, tar + pause/stop of the mounting tile, `pg_dump`/psql exec shape, the panel tar writer, the restore order, the per-volume claim.
Cut: `authz.go` whole, `VolumeFor`, `restorable`, prune's keep rules, per-tile cron + schedule loading, work-queue registration and restart cleanup, every store call, every Swarm op.
Cuts belong to: `service/internal/leaf/backup` (VolumeFor, restorable, prune rules — prose below), middleware `authz` (all of authz.go), the volume row (schedules), the job runner (queue, restart cleanup), the store layer (row reads/writes).

Targets: `service/internal/s3` (destination), `service/internal/flow/backup`
(orchestration order), `service/internal/leaf/tile` Exec (the commands).

---

## S3 destination — `service/internal/s3`

```go
// defaultRegion is what an S3-compatible server that ignores regions still
// needs the signer to name.
const defaultRegion = "us-east-1"

func client(d Dest) *s3.Client {
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

// Test verifies a destination by writing and deleting a probe.
func Test(ctx context.Context, d Dest) error {
	cl := client(d)
	key := ".stackr-probe"
	body := []byte("ok")
	if _, err := cl.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(d.Bucket), Key: aws.String(key),
		Body: bytes.NewReader(body), ContentLength: aws.Int64(int64(len(body))),
	}); err != nil {
		return err
	}
	_, err := cl.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(d.Bucket), Key: aws.String(key)})
	return err
}

// Put uploads one archive. A single PutObject with the length known up front:
// by this point the archive is always a finished local file, never a stream.
func Put(ctx context.Context, d Dest, key string, body io.ReadSeeker, size int64) error {
	_, err := client(d).PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(d.Bucket), Key: aws.String(key),
		Body: body, ContentLength: aws.Int64(size),
		ContentType: aws.String("application/gzip"),
	})
	return err
}

func Get(ctx context.Context, d Dest, key string) (io.ReadCloser, error) {
	out, err := client(d).GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(d.Bucket), Key: aws.String(key)})
	if err != nil {
		return nil, err
	}
	return out.Body, nil
}

// List pages the whole prefix and returns the keys sorted. Keys are
// timestamp-first, so lexical order is chronological order — that is what lets
// the caller keep the tail without reading any object metadata.
func List(ctx context.Context, d Dest, prefix string) ([]string, error) {
	cl := client(d)
	var keys []string
	var token *string
	for {
		out, err := cl.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket: aws.String(d.Bucket), Prefix: aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, err
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
	return keys, nil
}

func Delete(ctx context.Context, d Dest, key string) error {
	_, err := client(d).DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(d.Bucket), Key: aws.String(key)})
	return err
}
```

Object key, built by the flow once it has the prefix:

```go
key := path.Join(prefix, fmt.Sprintf("%s-%s",
	time.Now().UTC().Format("20060102-150405"), name))
// extract: dropped prefixFor's tile → stack → org lookups (store calls). The
// flow is handed the prefix. The rule that derives it is in "Rules as today".
```

## Scratch space — `service/internal/flow/backup`

```go
func (s *Service) scratchDir() string { return path.Join(s.dataDir, "backups") }

func (s *Service) scratchFile(id string) (*os.File, error) {
	dir := s.scratchDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return os.CreateTemp(dir, id+"-*")
}

// freeBytes reports the space available to a non-root user on the filesystem
// holding dir. Zero means "could not tell", the caller treats that as no gate
// rather than as a failure.
func freeBytes(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}

// checkScratchSpace fails the run before anything is frozen when the archive
// plainly will not fit. The volume size is uncompressed and docker reports it
// as -1 when it does not know, so this only catches the obvious cases.
func (s *Service) checkScratchSpace(ctx context.Context, vol string) error {
	// extract: docker call, becomes leaf/tile.Exec or docker wrapper method
	size, err := s.c.VolumeSize(ctx, vol)
	if err != nil || size < 0 {
		return nil
	}
	if err := os.MkdirAll(s.scratchDir(), 0o700); err != nil {
		return err
	}
	free, err := freeBytes(s.scratchDir())
	if err != nil || free == 0 {
		return nil
	}
	if free < uint64(size) {
		return fmt.Errorf("not enough scratch space in %s: %s free, volume holds %s",
			s.scratchDir(), FmtBytes(int64(free)), FmtBytes(size))
	}
	return nil
}

// removeScratch deletes the local spool files a backup or restore of this id
// left behind. Nothing was uploaded: PutObject either lands whole or not at all.
func (s *Service) removeScratch(id string) {
	matches, _ := filepath.Glob(filepath.Join(s.scratchDir(), id+"-*"))
	for _, m := range matches {
		_ = os.Remove(m)
	}
}
```

## Per-volume claim — `service/internal/flow/backup`

```go
// begin claims a volume for the caller, false when something else holds it.
// Restore holds the same claim across its whole flow, so a second restore
// cannot start a parallel wipe-and-extract on one volume. A queue that only
// dedupes waiting items does not stop a run and a restore racing on one volume.
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
```

## Run order — `service/internal/flow/backup`

```go
// run produces the archive into a local scratch file, uploads it, then prunes.
// Spooling first is what makes the modes work: the container is frozen for the
// length of the local tar, never for the length of the upload. It also gives
// restore something to verify before it wipes anything.
func (s *Service) run(ctx context.Context, b Backup, dest Dest, prefix, trigger string, step func(string)) error {
	// extract: dropped run-row open/save/close (store calls, belong to the job
	// runner) and the destination lookup.
	name, produce, err := s.producer(ctx, b, step)
	if err != nil {
		return err
	}
	f, err := s.scratchFile(b.VolumeID)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()); _ = f.Close() }()

	if err := produce(f); err != nil {
		return err
	}
	// The containers are released by now; cleanup has nothing to put back.
	step("uploading")
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	key := path.Join(prefix, fmt.Sprintf("%s-%s", time.Now().UTC().Format("20060102-150405"), name))
	if err := s3.Put(ctx, dest, key, f, size); err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	// Never prune on the pre-restore run: it is the newest archive under the
	// prefix, so pruning to the keep count would delete the oldest, which,
	// when the oldest is the archive being restored, destroys it before the
	// download and leaves the user with neither.
	if trigger != "pre-restore" {
		s.prune(ctx, b, dest, prefix)
	}
	return nil
}
```

## Dump and restore exec — `service/internal/leaf/tile` Exec

```go
// writeDump execs the engine's dump command in the running container and gzips
// its stdout into w.
func (s *Service) writeDump(ctx context.Context, t Tile, w io.Writer) error {
	// extract: dropped the engine lookup (managedtiles.Engines[t.Engine].DumpCmd)
	// and the "is the container running" check. See Notes.
	// extract: docker call, becomes leaf/tile.Exec or docker wrapper method
	rd, wait, err := s.c.ExecStream(ctx, cid, dumpCmd, nil)
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

// restoreDump streams the downloaded archive through gunzip straight into the
// engine's restore command's stdin. No local spool: the dump goes in as it
// arrives.
func (s *Service) restoreDump(ctx context.Context, t Tile, dest Dest, key string, step func(string)) error {
	obj, err := s3.Get(ctx, dest, key)
	if err != nil {
		return err
	}
	defer func() { _ = obj.Close() }()
	gz, err := gzip.NewReader(obj)
	if err != nil {
		return fmt.Errorf("corrupt archive: %w", err)
	}
	step(stepRestoring)
	// extract: docker call, becomes leaf/tile.Exec or docker wrapper method
	rd, wait, err := s.c.ExecStream(ctx, cid, restoreCmd, gz)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, rd)
	return wait()
}
```

## Volume tar — `service/internal/flow/backup`

```go
// writeVolume tars the volume into w under the mode: pause (freeze, the
// default), stop, or live (no interference, torn copies possible). The
// container is always released before the caller uploads.
func (s *Service) writeVolume(ctx context.Context, mode, vol string, t Tile, w io.Writer, step func(string)) error {
	if err := s.checkScratchSpace(ctx, vol); err != nil {
		return err
	}
	// Both before the freeze: a pull inside the pause window is downtime.
	// extract: docker call, becomes leaf/tile.Exec or docker wrapper method
	if err := s.c.EnsureVolumeTool(ctx); err != nil {
		return err
	}
	cids, err := s.containersFor(ctx, t) // the tile that mounts the volume
	if err != nil {
		return err
	}
	switch mode {
	case ModeStop:
		step(stepStopped)
		resume, err := s.quiesce(ctx, t)
		if err != nil {
			return err
		}
		defer resume()
	case ModeLive:
		// nothing to do, the documented torn-copy mode
	default: // pause
		step(stepPaused)
		for _, cid := range cids {
			// extract: docker call, becomes leaf/tile.Exec or docker wrapper method
			if err := s.c.PauseContainer(ctx, cid); err != nil {
				return fmt.Errorf("pausing container: %w", err)
			}
		}
		defer func() {
			for _, cid := range cids {
				// extract: docker call, becomes leaf/tile.Exec or docker wrapper method
				_ = s.c.UnpauseContainer(context.Background(), cid)
			}
		}()
	}
	// extract: docker call, becomes leaf/tile.Exec or docker wrapper method
	// (helper container over the volume; body is in infra/cluster, see Notes)
	return s.c.TarVolume(ctx, vol, w, mode == ModeLive)
}
```

Steps written while a job runs; restart cleanup reads them to decide what has
to be put back:

```go
const (
	stepPaused    = "paused"    // containers frozen for the tar
	stepStopped   = "stopped"   // mounting tile down for the tar
	stepRestoring = "restoring" // writing over live data
)
```

## Volume restore — `service/internal/flow/backup`

```go
// restoreVolume downloads to scratch and verifies the archive before anything
// is stopped or wiped, so every recoverable failure happens first.
func (s *Service) restoreVolume(ctx context.Context, vol string, t Tile, dest Dest, key string, step func(string)) error {
	obj, err := s3.Get(ctx, dest, key)
	if err != nil {
		return err
	}
	f, err := s.scratchFile(vol + "-restore")
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
	// extract: docker call, becomes leaf/tile.Exec or docker wrapper method
	if err := s.c.VerifyTar(ctx, f.Name()); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	step(stepRestoring)
	resume, err := s.quiesce(ctx, t)
	if err != nil {
		return err
	}
	defer resume()
	// extract: docker call, becomes leaf/tile.Exec or docker wrapper method
	// UntarVolume wipes the volume and extracts in one helper container.
	return s.c.UntarVolume(ctx, vol, f)
}
```

## Panel archive — `service/internal/flow/backup`

```go
// writePanelArchive writes the panel's whole restorable state: the database,
// the master key that decrypts everything in it, and the build version that
// wrote it.
//
// The key is in the archive on purpose. Without it every secret, database
// password and deploy key in the database is ciphertext no other host can
// read, and the backup would report success while losing all of them. The
// cost is that the archive is as sensitive as the panel itself.
//
// The version is what the restore script prints before it touches anything, so
// an operator sees an archive written by a different build than the one running.
func (s *Service) writePanelArchive(w io.Writer) error {
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
	// read in: it is the one archive member with no ceiling on it.
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

// WritePanelArchiveTo writes it to a local file. It lands under a temporary
// name first, so a failed write never leaves a truncated archive that looks
// restorable.
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
```

## Restore order as today

Two branches. The volume branch is the one the "verify before anything is
wiped" promise is about; the dump branch is weaker, and that is how it shipped.

Both branches, in order:

1. **Claim the volume** (`begin`), held for the whole flow including the
   pre-restore backup, and released only at the end. Two concurrent restores
   would otherwise run two wipe-and-extract passes over one volume.
2. **Check the run is restorable** — the rule is in "Rules as today". Checked
   when the restore is queued so the button answers at once, and again here.
3. **Pre-restore backup**, a full run with trigger `pre-restore`, which
   therefore does not prune. This is the way back, and what the confirm dialog
   promises. It runs for both branches: a dump restore is as irreversible as
   untarring over a volume. A failure here aborts the restore — going on would
   leave no way back.

Then, volume:

4. **Download** the archive to a scratch file.
5. **Verify the archive** (`VerifyTar`) while nothing has been touched.
6. **Stop** the mounting tile and hold it down.
7. **Wipe and untar** in one pass (`UntarVolume`).
8. **Start** the tile again (deferred resume, so it runs on failure too).

Or, dump:

4. **Download** and wrap in a gunzip reader. `gzip.NewReader` failing is the
   only verification this branch has.
5. **Exec the engine's restore command** with that reader as stdin, against the
   **live, running** database. No stop, no wipe — the restore command's own
   `DROP SCHEMA` is what clears the way.
6. Nothing to start: it never went down. An interrupted dump restore is the one
   case cleanup has to stop the tile itself, because half-restored data serving
   traffic is worse than the tile being down.

`checkScratchSpace` runs on the backup path only. Neither restore branch calls
it.

## Rules as today (spec) — `service/internal/leaf/backup`

**VolumeFor** — which docker volume a tile's data lives in. A volume tile
answers with its own docker volume; a managed instance answers with the volume
the managed-tile package names for it. Anything else has no volume of its own
and is refused with a message pointing at its attached volumes instead. In the
rewrite the schedule already hangs off the volume, so this collapses to "the
volume row names its volume" for plain volumes; the managed case stays a lookup
because the engine names the volume.

**restorable** — a run may be restored when: it loads by id, it belongs to this
schedule, its status is `done`, and it recorded an object key. The object key is
never taken from the caller — on a shared bucket that is a cross-tenant read.
Panel backups are refused outright: the archive carries the master key and has
to land in the data dir before the panel starts, so it is restored on the host
with the restore script, not from the panel.

**Prune / retention** — keep-last-N under one prefix. List every key under the
prefix, sort (timestamp-first keys make lexical order chronological), delete
everything except the newest `keep` entries. `keep <= 0` means keep everything,
prune does nothing. Delete failures are ignored: retention is best-effort and
must never fail a run that already uploaded. The pre-restore run never prunes
(reason in the kept `run` code).

**Prefix derivation** — the object-key root is derived from the row, never
supplied by a caller, because prune deletes everything under it: a
caller-chosen prefix on a shared bucket is a cross-tenant delete. Shape today:
`stackr/<org>/<stack>/<tile>/<backup id>` for a tile, `stackr/_panel/<id>` for
the panel, `stackr/_orphan/<id>` when the tile is gone. Two rules the tests
pin: two schedules on one tile never share a prefix (pruning one would delete
the other's archives), and the panel's prefix never lands under an org's.

## Notes for the builder

- **Three TAKE items are not in this directory.** Each needs its own extract row:
  - Tar mechanics: `cluster.TarVolume`, `UntarVolume`, `VerifyTar`,
    `EnsureVolumeTool` live in `infra/cluster`. Only the call
    sites are above. The helper-container invocation and the wipe inside
    `UntarVolume` are unread.
  - The `pg_dump`/psql argv: `managedtiles.Engines[<engine>].DumpCmd(t)` and
    `.RestoreCmd(t)` in `infra/managedtiles`. The call *shape*
    is above (stdout→gzip for dump, gunzip→stdin for restore, `wait()` on every
    path); the commands themselves are not.
  - **Multipart upload does not exist in the old code.** One `PutObject` with
    `ContentLength` from the spooled file. Do not read the row's mention of it
    as something to port.
- **Swarm is gone in the rewrite, so three helpers above are argv to drop, not
  port:** `quiesce` (Swarm `StopService` then `ScaleService(…, 1)`),
  `containersFor` (`RunningTasks` on a Swarm service — the Swarm-task lookup
  dies, the question does not: pause mode still needs "which containers hold
  this volume open right now" to have something to freeze), and the `node` parameter
  threaded through `writeVolume`/`checkScratchSpace`/`writeDump` (it existed
  because a volume and its container sit on one node's disk and the manager's
  socket would happily tar an empty volume for a tile running elsewhere). Keep
  the reason, not the calls: **a stop that returns before the writer is
  actually down gives a torn tar.** Scaling to zero returned as soon as the
  spec was posted and the database went on writing for its whole stop grace —
  that is why the code stops and waits rather than scales. `stop` mode in the
  rewrite is a plain container stop that waits for exit.
- **`authz.go` is dropped whole**, but it also held `Validate`, `ValidMode`,
  `ResolveNamed` and `ParseRef` — and `ParseRef` is what parses the
  `${{ org.backups.NAME }}` / `${{ stackr.backups.NAME }}` syntax the stack
  file still uses (REWRITE.md "Backups"). That parser needs a home; it is not
  auth. `ResolveDestination` and `OrgOf` are the auth half and stay dropped.
- **`PutObject` needs the length up front.** That is the constraint the spool
  file satisfies, and the only thing the kept mechanic says about where the
  plan's `age` writer can sit.
- Schedules move to the volume, so `LoadSchedules`, the cron registry and the
  `entries` map are not in this extract. The `method` column and cross-volume
  restore are new in the plan; nothing here anticipates them.
- Timeout today is 24h per job, on purpose: a volume archive has no size
  ceiling.

Size: source 1414 lines, extract 608 lines
