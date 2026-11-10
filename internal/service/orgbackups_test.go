package service

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"

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

	must(t, w.orch.DeleteOrg(ctx, org))

	got, err := w.st.BackupDests.Get(ctx, d.ID)
	must(t, err)
	if got.OrgID != nil || got.Shared || got.FormerOrg != "beta" || got.ArchiveKey != "archive-secret" {
		t.Fatalf("dest after org delete = %+v", got)
	}
	if _, err := w.st.BackupDests.Get(ctx, empty.ID); err == nil {
		t.Error("a destination with no runs outlived its org")
	}
	if rs, _ := w.st.BackupRuns.ListByDest(ctx, d.ID); len(rs) != 3 {
		t.Fatalf("runs = %d, want 3", len(rs))
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
	must(t, w.orch.backup.AgeFormer(ctx, 30*24*time.Hour, time.Now()))
	if rs, _ := w.st.BackupRuns.ListByDest(ctx, d.ID); len(rs) != 1 || len(s3.keys(prefix)) != 1 {
		t.Fatalf("after aging: %d runs, %v", len(rs), s3.keys(prefix))
	}

	// The admin's delete takes the archives, the rows and the destination.
	must(t, w.orch.DeleteBackupDest(ctx, "", d.ID))
	if len(s3.keys(prefix)) != 0 {
		t.Errorf("archives left: %v", s3.keys(prefix))
	}
	if _, err := w.st.BackupDests.Get(ctx, d.ID); err == nil {
		t.Error("destination kept after the admin delete")
	}
}
