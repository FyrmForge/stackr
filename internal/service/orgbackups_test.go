package service

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/internal/flow/serverconfig"
	bk "github.com/FyrmForge/stackr/internal/service/internal/leaf/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// An org delete leaves its backups: the destination moves to server scope
// (unshared, keys intact), another org cannot see it, the orphan sweep ages
// the runs out, and the admin's delete clears archives and rows.
func TestOrgDeleteKeepsBackups(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	s3, srv := newFakeS3(t)
	u, _ := url.Parse(srv.URL)

	org := uuid.NewString()
	must(t, w.st.Orgs.Create(ctx, store.Org{ID: org, Name: "beta", Slug: "beta", EnvColors: "{}", Settings: "{}", CreatedAt: time.Now()}))
	d := store.BackupDest{
		ID: uuid.NewString(), OrgID: &org, Kind: "s3", Name: "off", Endpoint: srv.URL, Bucket: "b",
		AccessKey: "a", SecretKey: "s", ArchiveKey: "archive-secret", CreatedAt: time.Now(),
		ApprovedEndpoint: u.Host,
	}
	must(t, w.st.BackupDests.Create(ctx, d))
	// An empty destination goes with the org, as before.
	empty := d
	empty.ID, empty.Name = uuid.NewString(), "empty"
	must(t, w.st.BackupDests.Create(ctx, empty))

	prefix := "stackr/" + org + "/vol/sched"
	old := time.Now().AddDate(0, 0, -60)
	for i, at := range []time.Time{old, old.Add(time.Hour), time.Now()} {
		key := fmt.Sprintf("%s/%d", prefix, i)
		s3.objs[key] = []byte("x")
		must(t, w.st.BackupRuns.Create(ctx, store.BackupRun{
			ID: uuid.NewString(), Kind: "volume", DestID: d.ID, Trigger: "schedule", Status: "done",
			ObjectKey: key, CreatedAt: at,
		}))
	}

	// PITR WAL objects have no run row: the sweep ages them by the kept base,
	// the admin's delete takes the rest. Another org's WAL in the same bucket
	// is never touched.
	root := "stackr/" + org + "/vol/pitr"
	seg := func(n int) string { return fmt.Sprintf("0000000100000000000000%02d", n) }
	staleWAL := bk.WALKey(root, seg(1), seg(2))
	keptWAL := bk.WALKey(root, seg(5), seg(6))
	other := bk.WALKey("stackr/"+uuid.NewString()+"/vol/pitr", seg(1), seg(2))
	for _, k := range []string{staleWAL, keptWAL, other} {
		s3.objs[k] = []byte("x")
	}
	fin, ws := time.Now().Add(-time.Hour), seg(5)
	baseKey := root + "/base/b-" + seg(5) + ".tar.gz.age"
	s3.objs[baseKey] = []byte("x")
	must(t, w.st.BackupRuns.Create(ctx, store.BackupRun{
		ID: uuid.NewString(), Kind: "volume", DestID: d.ID, Trigger: "schedule", Status: "done",
		ObjectKey: baseKey, CreatedAt: fin, FinishedAt: &fin, WalStart: &ws,
	}))

	must(t, w.orch.DeleteOrg(ctx, org))

	got, err := w.st.BackupDests.Get(ctx, d.ID)
	must(t, err)
	if got.OrgID != nil || got.Shared || got.FormerOrg != "beta" || got.ArchiveKey != "archive-secret" {
		t.Fatalf("dest after org delete = %+v", got)
	}
	if _, err := w.st.BackupDests.Get(ctx, empty.ID); err == nil {
		t.Error("a destination with no runs outlived its org")
	}
	if rs, _ := w.st.BackupRuns.ListByDest(ctx, d.ID); len(rs) != 4 {
		t.Fatalf("runs = %d, want 4", len(rs))
	}

	// Admin sees it; another org does not, now or under a re-used slug.
	if g, _ := w.orch.GlobalBackupDests(ctx); len(g) == 0 {
		t.Error("admin does not see the former org's destination")
	}
	must(t, w.st.Orgs.Create(ctx, store.Org{ID: uuid.NewString(), Name: "beta", Slug: "beta", EnvColors: "{}", Settings: "{}", CreatedAt: time.Now()}))
	for _, o := range []string{w.org, ""} {
		vis, _ := w.orch.BackupDests(ctx, o)
		for _, v := range vis {
			if v.ID == d.ID {
				t.Errorf("org %q sees the former org's destination", o)
			}
		}
	}
	if _, err := w.orch.backups.Resolve(ctx, w.org, "${{ stackr.backups."+got.Name+" }}"); err == nil {
		t.Error("another org resolves the former org's destination")
	}

	// Retention: the sweep drops what is older than the window, keeps the rest.
	must(t, w.orch.backup.AgeFormer(ctx, 30*24*time.Hour, time.Now(), io.Discard))
	if rs, _ := w.st.BackupRuns.ListByDest(ctx, d.ID); len(rs) != 2 || len(s3.keys(prefix)) != 1 {
		t.Fatalf("after aging: %d runs, %v", len(rs), s3.keys(prefix))
	}
	if _, ok := s3.objs[staleWAL]; ok {
		t.Error("WAL no kept base needs was not aged")
	}
	if _, ok := s3.objs[keptWAL]; !ok {
		t.Error("WAL a kept base needs was aged")
	}
	if got.FormerOrgID != org {
		t.Errorf("former org id = %q", got.FormerOrgID)
	}

	// A former org's destination is read-only except delete, and is no config.
	if _, err := w.orch.UpdateBackupDest(ctx, "", d.ID, BackupDestSpec{Name: "x", Endpoint: srv.URL, Bucket: "b", AccessKey: "a", SecretKey: "s"}); err == nil {
		t.Error("a former org's destination was edited")
	}
	out, err := w.orch.ExportServerConfig(ctx)
	must(t, err)
	if _, err := serverconfig.Parse(out); err != nil {
		t.Errorf("export after an org delete does not parse: %v\n%s", err, out)
	}

	// The admin's delete takes the archives, the rows and the destination.
	must(t, w.orch.DeleteBackupDest(ctx, "", d.ID))
	if len(s3.keys(prefix)) != 0 {
		t.Errorf("archives left: %v", s3.keys(prefix))
	}
	if left := s3.keys("stackr/" + org + "/"); len(left) != 0 {
		t.Errorf("objects left: %v", left)
	}
	if _, ok := s3.objs[other]; !ok {
		t.Error("another org's WAL was deleted")
	}
	if _, err := w.st.BackupDests.Get(ctx, d.ID); err == nil {
		t.Error("destination kept after the admin delete")
	}
}

// A former org's destination whose bucket is gone: the sweep skips it, a
// plain delete fails with the rows kept, and the forced delete drops both.
func TestFormerDestUnreachable(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	ep := "http://127.0.0.1:1"
	u, _ := url.Parse(ep)
	d := store.BackupDest{
		ID: uuid.NewString(), Kind: "s3", Name: "gone", Endpoint: ep, Bucket: "b",
		AccessKey: "a", SecretKey: "s", ArchiveKey: "k", CreatedAt: time.Now(),
		ApprovedEndpoint: u.Host, FormerOrg: "beta", FormerOrgID: uuid.NewString(),
	}
	must(t, w.st.BackupDests.Create(ctx, d))
	must(t, w.st.BackupRuns.Create(ctx, store.BackupRun{
		ID: uuid.NewString(), Kind: "volume", DestID: d.ID, Trigger: "schedule", Status: "done",
		ObjectKey: "stackr/x/vol/s/0", CreatedAt: time.Now(),
	}))
	must(t, w.orch.backup.AgeFormer(ctx, time.Hour, time.Now(), io.Discard))
	if err := w.orch.DeleteBackupDest(ctx, "", d.ID); err == nil {
		t.Fatal("plain delete of an unreachable destination succeeded")
	}
	if rs, _ := w.st.BackupRuns.ListByDest(ctx, d.ID); len(rs) != 1 {
		t.Fatalf("runs after failed delete = %d", len(rs))
	}
	must(t, w.orch.ForceDeleteBackupDest(ctx, d.ID))
	if _, err := w.st.BackupDests.Get(ctx, d.ID); err == nil {
		t.Error("destination kept after the forced delete")
	}
}
