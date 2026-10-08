package service

import (
	"context"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/hostgrant"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// A host-access park resumes by itself once the stack's grant covers its ask,
// even when the grant came from another job's approval, not ApproveHostGrant.
func TestCoveredParkResumes(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	now := time.Now()
	must(t, w.st.Users.Create(ctx, store.User{ID: "adm", Email: "a@b.c", Name: "A", Role: "admin", Active: true, CreatedAt: now, UpdatedAt: now}))
	w.fake.RunID = "c1"
	w.fake.Details = map[string]docker.Detail{"c1": {Running: true, Health: "healthy", Networks: map[string]string{"n": "10.0.0.5"}}}
	tl := w.tile(t, "mon", false)
	const line = "host:/var/run/docker.sock:/var/run/docker.sock:ro"
	_, _, err := w.orch.UpdateTile(ctx, tl.ID, func(t *Tile) error {
		t.Volumes = line
		return nil
	})
	must(t, err)
	j, err := w.orch.Deploy(ctx, tl.ID)
	must(t, err)
	if j = w.waiting(t, j.ID); j.State != job.Waiting {
		t.Fatalf("deploy = %s %q, want parked", j.State, j.Error)
	}
	_, err = w.orch.hostgrant.Approve(ctx, w.stack, "adm", hostgrant.Set{Lines: []string{"mon " + line}})
	must(t, err)
	if j = w.wait(t, j.ID); j.State != job.Done {
		t.Fatalf("covered park = %s %q, want it resumed", j.State, j.Error)
	}
}
