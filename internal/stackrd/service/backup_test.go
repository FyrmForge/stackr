package service

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

type backupStore struct {
	repo.Store
	dests   []repo.BackupDestination
	stack   *repo.Stack
	created *repo.Backup
}

func (b *backupStore) ListBackupDestinations(context.Context) ([]repo.BackupDestination, error) {
	return b.dests, nil
}
func (b *backupStore) GetStack(context.Context, string) (*repo.Stack, error) { return b.stack, nil }
func (b *backupStore) GetBackupDestination(_ context.Context, id string) (*repo.BackupDestination, error) {
	for i := range b.dests {
		if b.dests[i].ID == id {
			return &b.dests[i], nil
		}
	}
	return nil, nil
}
func (b *backupStore) CreateBackup(_ context.Context, row *repo.Backup) error {
	b.created = row
	return nil
}

// One visibility rule, where the admin page, the org page and the API each
// had their own. An unshared server-wide destination carries credentials the
// admin has not handed out.
func TestVisibleDestinations(t *testing.T) {
	st := &backupStore{dests: []repo.BackupDestination{
		{ID: "global-private"},
		{ID: "global-shared", Shared: true},
		{ID: "mine", OrgID: sql.NullString{String: "org1", Valid: true}},
		{ID: "theirs", OrgID: sql.NullString{String: "org2", Valid: true}},
	}}
	svc := NewBackupDestinationService(st, nil)
	ctx := context.Background()

	got, err := svc.Visible(ctx, Viewer{Orgs: map[string]bool{"org1": true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "global-shared" || got[1].ID != "mine" {
		t.Fatalf("member saw %v", ids(got))
	}
	if got, _ = svc.Visible(ctx, Viewer{Admin: true}); len(got) != 4 {
		t.Fatalf("an admin should see every destination, saw %v", ids(got))
	}
	// AtScope is the other question: what one page administers.
	if got, _ = svc.AtScope(ctx, ""); len(got) != 2 {
		t.Fatalf("the admin page administers the server-wide ones, got %v", ids(got))
	}
	if got, _ = svc.AtScope(ctx, "org1"); len(got) != 1 || got[0].ID != "mine" {
		t.Fatalf("an org page administers its own, got %v", ids(got))
	}
}

func ids(ds []repo.BackupDestination) []string {
	out := make([]string, len(ds))
	for i := range ds {
		out[i] = ds[i].ID
	}
	return out
}

// The kind derivation was written three times and the API's accepted a dump
// for a tile that is not a database, which produced a schedule that failed on
// every run. Keep defaulted to 0 everywhere but the panel, and 0 means keep
// nothing.
func TestScheduleDerivesKindAndDefaultsKeep(t *testing.T) {
	ctx := context.Background()
	st := &backupStore{
		stack: &repo.Stack{ID: "s1", OrgID: "org1"},
		dests: []repo.BackupDestination{{ID: "d1", OrgID: sql.NullString{String: "org1", Valid: true}, Name: "offsite"}},
	}
	svc := NewBackupScheduleService(st, NewBackupDestinationService(st, nil), nil, NewGateService(st))
	cron := "0 3 * * *"

	db := &repo.Tile{ID: "t1", StackID: "s1", Kind: "managed", Engine: "postgres"}
	b, err := svc.Adopt(ctx, db, ScheduleSpec{Dest: "d1", Cron: &cron})
	if err != nil {
		t.Fatal(err)
	}
	if b.Kind != repo.BackupDump {
		t.Fatalf("a managed database should be dumped, got %q", b.Kind)
	}
	if b.KeepLatest != DefaultKeep {
		t.Fatalf("keep = %d, want %d; 0 means keep nothing", b.KeepLatest, DefaultKeep)
	}

	// A dump of something that is not a database: refused, where the API
	// accepted it.
	svc2 := &repo.Tile{ID: "t2", StackID: "s1", Kind: "service"}
	if _, err := svc.Adopt(ctx, svc2, ScheduleSpec{Dest: "d1", Kind: repo.BackupDump, Cron: &cron}); err == nil {
		t.Fatal("a dump of a plain service was accepted")
	}
	// And a volume tar of a tile with no volume of its own.
	if _, err := svc.Adopt(ctx, svc2, ScheduleSpec{Dest: "d1", Cron: &cron}); err == nil {
		t.Fatal("a volume tar of a tile with no volume was accepted")
	}
}

// A destination that does not resolve is not-found, never invalid: it is the
// tenant boundary, and "exists but not yours" must read the same as "gone".
func TestResolveHidesAnotherOrgsDestination(t *testing.T) {
	st := &backupStore{dests: []repo.BackupDestination{
		{ID: "theirs", OrgID: sql.NullString{String: "org2", Valid: true}, Name: "offsite"},
	}}
	svc := NewBackupDestinationService(st, nil)
	_, err := svc.Resolve(context.Background(), "org1", "theirs")
	if !errors.Is(err, svcerr.ErrNotFound) {
		t.Fatalf("want not-found, got %v", err)
	}
}
