//go:build integration

// PITR against a real postgres:17 with the instance's own archiving command:
// make test-integration, or go test -tags integration -run PITRAgainstPostgres.
package service

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	fbackup "github.com/FyrmForge/stackr/internal/service/internal/flow/backup"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/operated"
	bk "github.com/FyrmForge/stackr/internal/service/internal/leaf/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/s3"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/internal/storetest"
)

// readBundle decrypts and gunzips an archive object into its tar entries.
func readBundle(t *testing.T, d store.BackupDest, key string, gz bool) map[string][]byte {
	t.Helper()
	obj, err := s3.Local{Dir: d.Endpoint}.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = obj.Close() }()
	id, err := age.NewScryptIdentity(d.ArchiveKey)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := age.Decrypt(obj, id)
	if err != nil {
		t.Fatalf("%s does not decrypt with the destination's key: %v", key, err)
	}
	var r io.Reader = plain
	if gz {
		zr, err := gzip.NewReader(plain)
		if err != nil {
			t.Fatal(err)
		}
		r = zr
	}
	out := map[string][]byte{}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Size > 1<<20 {
			_, _ = io.Copy(io.Discard, tr) // a 16 MB segment: its name is the assertion
			out[h.Name] = nil
			continue
		}
		out[h.Name], _ = io.ReadAll(tr)
	}
}

func TestPITRAgainstPostgres(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no docker")
	}
	ctx := context.Background()
	name := fmt.Sprintf("stkr-it-pitr-%d", time.Now().UnixNano())
	wal := name + "-wal"
	def := mflow.Engines["postgres"].Definition()
	args := []string{"run", "-d", "--name", name,
		"--label", tile.LabelTile + "=t1", "--label", tile.LabelRole + "=replica",
		"-v", wal + ":" + def.ArchiveDir,
		"-e", "POSTGRES_USER=stackr", "-e", "POSTGRES_PASSWORD=admin", "-e", "POSTGRES_DB=postgres",
		def.Image}
	if out, err := exec.Command("docker", append(args, def.ArchiveCmd...)...).CombinedOutput(); err != nil {
		t.Skipf("no postgres:17 container: %v %s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", "-v", name).Run()
		_ = exec.Command("docker", "volume", "rm", "-f", wal).Run()
	})
	in := mflow.Instance{AdminUser: "stackr", AdminPassword: "admin", AdminDB: "postgres", PITRDays: 7}
	dexec := func(argv ...string) (string, error) {
		out, err := exec.Command("docker", append([]string{"exec", name}, argv...)...).CombinedOutput()
		if err != nil {
			err = fmt.Errorf("%w: %s", err, out)
		}
		return string(out), err
	}
	psql := func(q string) string {
		out, err := dexec("env", "PGPASSWORD=admin", "psql", "-q", "-tA", "-U", "stackr", "-d", "postgres", "-c", q)
		if err != nil {
			t.Fatalf("psql: %v", err)
		}
		return strings.TrimSpace(out)
	}
	for end := time.Now().Add(time.Minute); ; time.Sleep(time.Second) {
		if _, err := dexec("env", "PGPASSWORD=admin", "psql", "-q", "-U", "stackr", "-d", "postgres", "-c", "SELECT 1"); err == nil {
			break
		}
		if time.Now().After(end) {
			t.Fatal("postgres never came up")
		}
	}

	// the flow, over the real Docker daemon and a local destination
	d, err := docker.New()
	if err != nil {
		t.Fatal(err)
	}
	st := storetest.Store(t)
	dir := t.TempDir()
	f := &fbackup.Flow{
		Backups: bk.New(st.BackupDests, st.BackupSchedules, st.BackupRuns),
		Volumes: volume.New(st.Volumes, d),
		Tiles:   tile.New(st.Tiles, d, vipStub{}),
		Scratch: filepath.Join(dir, "scratch"),
	}
	dest, err := f.Backups.EnsureLocal(ctx, filepath.Join(dir, "archives"))
	if err != nil {
		t.Fatal(err)
	}
	vol, _, err := f.Volumes.Declare(ctx, volume.Scope{Kind: "env", ID: "e1"}, "pg-data", nil)
	if err != nil {
		t.Fatal(err)
	}
	engine := store.Tile{ID: "t1", Slug: "pg", EnvironmentID: "e1"}
	root := bk.PITRRoot("o1", vol.ID)

	// archiving works: rows, then a forced switch, then a segment in the archive dir
	psql("CREATE TABLE t (id int, v text)")
	psql("INSERT INTO t SELECT g, repeat('x', 100) FROM generate_series(1, 1000) g")
	psql("SELECT pg_switch_wal()")
	var waiting []string
	for end := time.Now().Add(30 * time.Second); ; time.Sleep(time.Second) {
		out, _ := dexec("ls", "-1", def.ArchiveDir)
		waiting = nil
		for _, n := range strings.Fields(out) {
			if bk.WALName(n) {
				waiting = append(waiting, n)
			}
		}
		if len(waiting) > 0 {
			break
		}
		if time.Now().After(end) {
			failed := psql("SELECT failed_count || ' ' || coalesce(last_failed_wal, '') FROM pg_stat_archiver")
			t.Fatalf("no segment reached the archive dir; failed_count = %s", failed)
		}
	}
	if got := psql("SELECT failed_count FROM pg_stat_archiver"); got != "0" {
		t.Errorf("archive_command failed %s time(s)", got)
	}
	if got := psql("SELECT archived_count > 0 FROM pg_stat_archiver"); got != "t" {
		t.Errorf("archived_count > 0 = %s", got)
	}

	// base backup: pg_basebackup through the flow, wal_start recorded
	be := mflow.Engines["postgres"]
	base := be.Backup("base", in)
	if base == nil {
		t.Fatal("no base backup argv")
	}
	walStart := be.(interface{ WalStartArgv(mflow.Instance) []string }).WalStartArgv(in)
	sub := fbackup.Subject{Volume: vol, Engine: engine, Base: true, Dump: base, WalStart: walStart}
	run, err := f.Backup(ctx, sub, fbackup.Spec{Dest: dest, Prefix: root + "/" + bk.BaseDir, Trigger: "manual", PITRDays: 7}, io.Discard)
	if err != nil {
		t.Fatalf("base backup: %v", err)
	}
	if run.WalStart == nil || !regexp.MustCompile(`^[0-9A-F]{24}$`).MatchString(*run.WalStart) {
		t.Fatalf("wal_start = %v", run.WalStart)
	}
	if !strings.HasPrefix(run.ObjectKey, root+"/base/") || !strings.HasSuffix(run.ObjectKey, ".tar.gz.age") {
		t.Errorf("base key = %s", run.ObjectKey)
	}
	files := readBundle(t, dest, run.ObjectKey, true)
	for _, want := range []string{"PG_VERSION", "backup_label", "global/pg_control"} {
		if _, ok := files[want]; !ok {
			t.Errorf("the base backup lacks %s; it has %d entries", want, len(files))
		}
	}
	if m := regexp.MustCompile(`\(file ([0-9A-F]{24})\)`).FindSubmatch(files["backup_label"]); m == nil {
		t.Error("backup_label names no start segment")
	} else if *run.WalStart > string(m[1]) {
		t.Errorf("recorded wal_start %s is after the backup's own start %s: pruning could cut the WAL it needs", *run.WalStart, m[1])
	}

	// shipper tick: the segments leave the box as one bundle, and only then
	psql("INSERT INTO t SELECT g, 'later' FROM generate_series(1001, 1010) g")
	psql("SELECT pg_switch_wal()")
	time.Sleep(3 * time.Second) // archive_command is asynchronous
	res, err := f.ShipWAL(ctx, fbackup.Ship{Engine: engine, Dest: dest, Root: root, Dir: def.ArchiveDir}, io.Discard)
	if err != nil {
		t.Fatalf("ship: %v", err)
	}
	if res.Bundles != 1 || res.Last == "" || res.Waiting != 0 || res.Bytes <= 0 {
		t.Fatalf("shipped = %+v", res)
	}
	keys, _ := s3.Local{Dir: dest.Endpoint}.List(ctx, root+"/"+bk.WALDir+"/")
	if len(keys) != 1 {
		t.Fatalf("bundles = %v", keys)
	}
	first, last, _ := strings.Cut(strings.TrimSuffix(keys[0][strings.LastIndex(keys[0], "/")+1:], bk.BundleExt), "-")
	segs := readBundle(t, dest, keys[0], true)
	if _, ok := segs[first]; !ok {
		t.Errorf("the bundle lacks its first segment %s: %v", first, slices.Sorted(mapKeys(segs)))
	}
	if _, ok := segs[last]; !ok {
		t.Errorf("the bundle lacks its last segment %s", last)
	}
	for _, n := range waiting {
		if bk.Segment(n) != "" && n == bk.Segment(n) {
			if _, ok := segs[n]; !ok {
				t.Errorf("segment %s was waiting but is not in the bundle", n)
			}
		}
	}
	for n := range segs {
		if !bk.WALName(n) {
			t.Errorf("bundle holds %q, not a WAL file", n)
		}
	}
	left, _ := dexec("ls", "-1", def.ArchiveDir)
	for _, n := range strings.Fields(left) {
		if _, shipped := segs[n]; shipped {
			t.Errorf("%s was shipped but is still on the box", n)
		}
	}
	// the guard now sees the instance's own history
	if err := f.Guard(ctx, dest, root); err != nil {
		t.Errorf("guard refused our own archive: %v", err)
	}
	if got := psql("SELECT count(*) FROM t"); got != "1010" {
		t.Errorf("rows = %s", got)
	}
}

func mapKeys[V any](m map[string]V) func(yield func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}
