package backup_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

var ctx = context.Background()

func seedOrg(t *testing.T, st *store.Store) string {
	t.Helper()
	id := uuid.NewString()
	if err := st.Orgs.Create(ctx, store.Org{ID: id, Name: id, Slug: id[:8], EnvColors: "{}", Settings: "{}", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return id
}

func seedVolume(t *testing.T, st *store.Store, org string) string {
	t.Helper()
	id := uuid.NewString()
	if err := st.Volumes.Create(ctx, store.Volume{ID: id, ScopeKind: "org", ScopeID: org, Slug: id[:8], Name: id[:8], CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return id
}

func leaf(st *store.Store) *backup.Leaf {
	return backup.New(st.BackupDests, st.BackupSchedules, st.BackupRuns)
}

func s3(name string, shared bool) backup.Dest {
	return backup.Dest{Name: name, Endpoint: "https://s3.example.com", Bucket: "b", AccessKey: "ak", SecretKey: "sk", Shared: shared}
}

func TestVisibility(t *testing.T) {
	st := servicetest.Store(t)
	l := leaf(st)
	org, other := seedOrg(t, st), seedOrg(t, st)

	local, err := l.EnsureLocal(ctx, "/var/lib/stackr/backups")
	if err != nil || len(local.ArchiveKey) != 64 {
		t.Fatalf("local = %+v, %v", local, err)
	}
	if again, _ := l.EnsureLocal(ctx, "/var/lib/stackr/backups"); again.ID != local.ID {
		t.Error("EnsureLocal made a second local destination")
	}
	own, err := l.Create(ctx, &org, s3("hetzner", true))
	if err != nil || own.Shared {
		t.Fatalf("own = %+v, %v (an org destination is never shared)", own, err)
	}
	shared, _ := l.Create(ctx, nil, s3("offsite", true))
	private, _ := l.Create(ctx, nil, s3("panel", false))
	if own.ArchiveKey == shared.ArchiveKey || own.ArchiveKey == "" {
		t.Error("destinations share an archive key")
	}

	for _, c := range []struct {
		org, id string
		ok      bool
	}{
		{org, own.ID, true}, {other, own.ID, false},
		{other, shared.ID, true}, {org, private.ID, false}, {org, local.ID, true},
	} {
		_, err := l.For(ctx, c.org, c.id)
		if c.ok != (err == nil) || (!c.ok && !errors.Is(err, errs.ErrNotFound)) {
			t.Errorf("For(%s) = %v, want ok=%v", c.id, err, c.ok)
		}
	}
	vis, _ := l.Visible(ctx, other)
	var names []string
	for _, d := range vis {
		names = append(names, d.Name)
		if d.SecretKey != "" || d.ArchiveKey != "" {
			t.Error("Visible leaked a key")
		}
	}
	slices.Sort(names)
	if fmt.Sprint(names) != "[local offsite]" {
		t.Errorf("other sees %v", names)
	}

	for ref, want := range map[string]string{
		"": local.ID, "${{ org.backups.hetzner }}": own.ID, "${{stackr.backups.offsite}}": shared.ID,
	} {
		if d, err := l.Resolve(ctx, org, ref); err != nil || d.ID != want {
			t.Errorf("Resolve(%q) = %s, %v", ref, d.Name, err)
		}
	}
	for _, ref := range []string{"${{ stackr.backups.panel }}", "${{ org.backups.offsite }}", "hetzner"} {
		if _, err := l.Resolve(ctx, org, ref); err == nil {
			t.Errorf("Resolve(%q) accepted", ref)
		}
	}
	if _, err := l.Create(ctx, &org, s3("hetzner", false)); err == nil {
		t.Error("duplicate name accepted")
	}
	if err := l.Delete(ctx, local); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("delete local = %v", err)
	}

	upd, err := l.Update(ctx, own, backup.Dest{Name: "hetzner", Bucket: "b2", AccessKey: "ak"})
	if err != nil || upd.SecretKey != "sk" || upd.ArchiveKey != own.ArchiveKey {
		t.Errorf("update = %+v, %v (secret and archive key must stay)", upd, err)
	}
}

func TestSchedules(t *testing.T) {
	st := servicetest.Store(t)
	l := leaf(st)
	org, other := seedOrg(t, st), seedOrg(t, st)
	vol := seedVolume(t, st, org)
	theirs, _ := l.Create(ctx, &other, s3("theirs", false))
	mine, _ := l.Create(ctx, &org, s3("mine", false))
	methods := []string{"tar"}

	s, err := l.AddSchedule(ctx, vol, org, methods, backup.Schedule{Cron: " 0  3 * * * ", Keep: 7})
	if err != nil || s.Method != "tar" || s.Mode != backup.Pause || s.Timezone != "UTC" || s.Cron != "0 3 * * *" {
		t.Fatalf("schedule = %+v, %v", s, err)
	}
	for name, sp := range map[string]backup.Schedule{
		"method":   {Method: "pg_dump", Cron: "@daily"},
		"mode":     {Mode: "snapshot", Cron: "@daily"},
		"schedule": {Cron: "every day"},
		"timezone": {Cron: "@daily", Timezone: "Mars/Olympus"},
		"keep":     {Cron: "@daily", Keep: -1},
	} {
		_, err := l.AddSchedule(ctx, vol, org, methods, sp)
		if iv, ok := errs.IsInvalid(err); !ok || iv.Field != name {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := l.AddSchedule(ctx, vol, org, methods, backup.Schedule{Cron: "@daily", DestID: &theirs.ID}); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("another org's destination = %v", err)
	}
	if _, err := l.AddSchedule(ctx, vol, org, methods, backup.Schedule{Cron: "@daily", DestID: &mine.ID}); err != nil {
		t.Fatal(err)
	}
	if err := l.Delete(ctx, mine); err == nil {
		t.Error("deleted a destination a schedule uses")
	}
}

// objects is an in-memory destination.
type objects struct {
	keys []string
	fail string
}

func (o *objects) List(_ context.Context, prefix string) ([]string, error) {
	var out []string
	for _, k := range o.keys {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	return out, nil
}

func (o *objects) Delete(_ context.Context, key string) error {
	if key == o.fail {
		return errors.New("boom")
	}
	o.keys = slices.DeleteFunc(o.keys, func(k string) bool { return k == key })
	return nil
}

func TestPrune(t *testing.T) {
	st := servicetest.Store(t)
	l := leaf(st)
	org := seedOrg(t, st)
	vol := seedVolume(t, st, org)
	local, _ := l.EnsureLocal(ctx, "/b")
	sA, _ := l.AddSchedule(ctx, vol, org, []string{"tar"}, backup.Schedule{Cron: "@daily", Keep: 2})
	sB, _ := l.AddSchedule(ctx, vol, org, []string{"tar"}, backup.Schedule{Cron: "@hourly", Keep: 2})
	pA, pB := backup.Prefix(org, vol, sA.ID), backup.Prefix(org, vol, sB.ID)
	if pA == pB || strings.HasPrefix(backup.PanelPrefix("x"), "stackr/"+org) {
		t.Fatal("prefixes collide")
	}

	obj := &objects{}
	day := time.Date(2026, 9, 1, 3, 0, 0, 0, time.UTC)
	for i := range 4 { // written out of order: prune sorts
		for _, p := range []string{pA, pB} {
			k := backup.ObjectKey(p, "vol.tar.gz.age", day.AddDate(0, 0, 3-i))
			obj.keys = append(obj.keys, k)
			r, _ := l.Start(ctx, backup.KindVolume, &vol, &sA.ID, local.ID, "cron")
			_, _ = l.Finish(ctx, r, k, 10, nil)
		}
	}
	if gone := l.Prune(ctx, obj, pA, 0); gone != nil {
		t.Errorf("keep 0 pruned %v", gone)
	}
	gone := l.Prune(ctx, obj, pA, 2)
	if len(gone) != 2 || !strings.Contains(gone[0], "20260901") || !strings.Contains(gone[1], "20260902") {
		t.Errorf("pruned %v, want the two oldest", gone)
	}
	if left, _ := obj.List(ctx, pB+"/"); len(left) != 4 {
		t.Errorf("pruning A touched B: %d left", len(left))
	}
	rows, _ := l.Runs(ctx, vol)
	if len(rows) != 6 {
		t.Errorf("%d run rows left, want 6", len(rows))
	}

	obj.fail = backup.ObjectKey(pB, "vol.tar.gz.age", day)
	if gone := l.Prune(ctx, obj, pB, 2); len(gone) != 1 {
		t.Errorf("failed delete: pruned %v", gone)
	}
	if left, _ := obj.List(ctx, pB+"/"); len(left) != 3 {
		t.Errorf("B has %d left, want 3 (the failed one stays)", len(left))
	}
}

func TestRestorable(t *testing.T) {
	st := servicetest.Store(t)
	l := leaf(st)
	org := seedOrg(t, st)
	vol, otherVol := seedVolume(t, st, org), seedVolume(t, st, org)
	local, _ := l.EnsureLocal(ctx, "/b")

	ok, _ := l.Start(ctx, backup.KindVolume, &vol, nil, local.ID, "manual")
	ok, _ = l.Finish(ctx, ok, "stackr/k", 1, nil)
	failed, _ := l.Start(ctx, backup.KindVolume, &vol, nil, local.ID, "manual")
	failed, _ = l.Finish(ctx, failed, "", 0, errors.New("disk full"))
	running, _ := l.Start(ctx, backup.KindVolume, &vol, nil, local.ID, "manual")
	panel, _ := l.Start(ctx, backup.KindPanel, nil, nil, local.ID, "manual")
	panel, _ = l.Finish(ctx, panel, "stackr/_panel/x/k", 1, nil)

	if _, err := l.Restorable(ctx, ok.ID, vol); err != nil {
		t.Errorf("done run: %v", err)
	}
	if _, err := l.Restorable(ctx, ok.ID, otherVol); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("another volume's run: %v", err)
	}
	for _, r := range []store.BackupRun{failed, running, panel} {
		if _, err := l.Restorable(ctx, r.ID, vol); !errors.Is(err, errs.ErrRefused) {
			t.Errorf("%s/%s run: %v", r.Kind, r.Status, err)
		}
	}
	if _, err := l.Start(ctx, backup.KindPanel, &vol, nil, local.ID, "manual"); err == nil {
		t.Error("panel run with a volume accepted")
	}
}
