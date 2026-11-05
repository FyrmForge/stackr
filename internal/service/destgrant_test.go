package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

const lanEndpoint = "http://192.168.1.106:9100"

func (w *world) users(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	for id, role := range map[string]string{"adm": "admin", "mem": "user"} {
		must(t, w.st.Users.Create(ctx, store.User{ID: id, Email: id + "@b.c", Name: id, Role: role, Active: true, CreatedAt: now, UpdatedAt: now}))
	}
	// an admin's key bound to the org
	must(t, w.st.APIKeys.Create(ctx, store.APIKey{ID: "orgkey", UserID: "adm", OrgID: &w.org, Name: "k", TokenHash: "h1", CreatedAt: now}))
}

func lanSpec(endpoint string) BackupDestSpec {
	return BackupDestSpec{Name: "rig-s3", Endpoint: endpoint, Bucket: "b", AccessKey: "a", SecretKey: "s"}
}

func asUser(user, key string) context.Context {
	return WithActor(context.Background(), user, key)
}

// A member's destination on a private address is saved but pending, listed
// for the admin, approved on request, and pending again when the endpoint
// moves.
func TestDestGrantPendingApproveRepend(t *testing.T) {
	w := newWorld(t)
	w.users(t)
	ctx := asUser("mem", "")
	d, err := w.orch.CreateBackupDest(ctx, &w.org, lanSpec(lanEndpoint))
	must(t, err)
	if d.ApprovedEndpoint != "" {
		t.Fatalf("a member's save approved itself: %+v", d)
	}
	gs, err := w.orch.DestGrants(ctx)
	must(t, err)
	if len(gs) != 1 || !gs[0].Pending || gs[0].Addr != "192.168.1.106:9100" || gs[0].Name != "rig-s3" {
		t.Fatalf("grants = %+v", gs)
	}
	if n, _ := w.orch.HostGrantsWaiting(ctx); n != 1 {
		t.Fatalf("badge = %d", n)
	}
	// a stale look is refused, the right one approves
	if _, err := w.orch.ApproveDestGrant(ctx, d.ID, "adm", "192.168.1.7:9100"); err == nil {
		t.Fatal("approved an address that was not shown")
	}
	if _, err := w.orch.ApproveDestGrant(ctx, d.ID, "adm", gs[0].Addr); err != nil {
		t.Fatal(err)
	}
	gs, _ = w.orch.DestGrants(ctx)
	if len(gs) != 1 || gs[0].Pending || gs[0].ApprovedBy != "adm" {
		t.Fatalf("after approve = %+v", gs)
	}
	// the same host:port under another path or name stays approved; a new
	// endpoint is pending again
	_, err = w.orch.UpdateBackupDest(ctx, w.org, d.ID, lanSpec("http://192.168.1.106:9101"))
	must(t, err)
	gs, _ = w.orch.DestGrants(ctx)
	if len(gs) != 1 || !gs[0].Pending {
		t.Fatalf("after endpoint change = %+v", gs)
	}
	// a public endpoint needs no approval at all
	_, err = w.orch.UpdateBackupDest(ctx, w.org, d.ID, lanSpec("https://8.8.8.8:9000"))
	must(t, err)
	if gs, _ = w.orch.DestGrants(ctx); len(gs) != 0 {
		t.Fatalf("public endpoint listed: %+v", gs)
	}
}

// A server admin's own save approves in their name; an admin's key bound to
// an org does not.
func TestDestGrantAdminSaveAutoApproves(t *testing.T) {
	w := newWorld(t)
	w.users(t)
	d, err := w.orch.CreateBackupDest(asUser("adm", ""), &w.org, lanSpec(lanEndpoint))
	must(t, err)
	if d.ApprovedEndpoint != "192.168.1.106:9100" || d.ApprovedBy != "adm" {
		t.Fatalf("admin save = %+v", d)
	}
	// the admin's change keeps it approved; a member's endpoint change does not
	if _, err = w.orch.UpdateBackupDest(asUser("mem", ""), w.org, d.ID, lanSpec("http://192.168.1.106:9200")); err != nil {
		t.Fatal(err)
	}
	gs, _ := w.orch.DestGrants(context.Background())
	if len(gs) != 1 || !gs[0].Pending {
		t.Fatalf("member's change = %+v", gs)
	}
	s := lanSpec(lanEndpoint)
	s.Name = "viakey"
	k, err := w.orch.CreateBackupDest(asUser("adm", "orgkey"), &w.org, s)
	must(t, err)
	if k.ApprovedEndpoint != "" {
		t.Fatalf("org-bound key approved: %+v", k)
	}
}

// An approval is for dest endpoints only: a global destination or a
// non-private one is refused.
func TestApproveDestGrantRefusals(t *testing.T) {
	w := newWorld(t)
	w.users(t)
	pub, err := w.orch.CreateBackupDest(asUser("mem", ""), &w.org, lanSpec("https://8.8.8.8:9000"))
	must(t, err)
	if _, err := w.orch.ApproveDestGrant(context.Background(), pub.ID, "adm", "8.8.8.8:9000"); err == nil {
		t.Fatal("approved a public destination")
	}
}

func (w *world) hostTile(t *testing.T) Tile {
	t.Helper()
	w.fake.RunID = "c1"
	w.fake.Details = map[string]docker.Detail{"c1": {Running: true, Health: "healthy", Networks: map[string]string{"n": "10.0.0.5"}}}
	tl := w.tile(t, "mon", false)
	_, _, err := w.orch.UpdateTile(context.Background(), tl.ID, func(t *Tile) error {
		t.Volumes = "host:/var/run/docker.sock:/var/run/docker.sock:ro"
		return nil
	})
	must(t, err)
	return tl
}

// A tile a server admin deploys is granted its host access in their name and
// runs; a member's, or an admin's org-bound key's, parks for approval.
func TestAdminDeployAutoApprovesHostAccess(t *testing.T) {
	w := newWorld(t)
	w.users(t)
	tl := w.hostTile(t)

	j, err := w.orch.Deploy(asUser("adm", ""), tl.ID)
	must(t, err)
	if j = w.wait(t, j.ID); j.State != job.Done {
		t.Fatalf("admin deploy = %s %q", j.State, j.Error)
	}
	g, err := w.orch.HostGrant(context.Background(), w.stack)
	must(t, err)
	if !g.Granted || g.ApprovedBy != "adm" {
		t.Fatalf("grant = %+v", g)
	}

	must(t, w.orch.RevokeHostGrant(context.Background(), w.stack, "", "adm"))
	for _, ctx := range []context.Context{asUser("mem", ""), asUser("adm", "orgkey")} {
		j, err = w.orch.Deploy(ctx, tl.ID)
		must(t, err)
		if j = w.waiting(t, j.ID); j.State != job.Waiting || !strings.Contains(*j.WaitingParam, "elevated access") {
			t.Fatalf("deploy = %s %q, want parked", j.State, j.Error)
		}
		if g, _ = w.orch.HostGrant(context.Background(), w.stack); g.Granted {
			t.Fatalf("granted without an admin: %+v", g)
		}
	}
}

// A revoke makes an approved destination pending again and is logged.
func TestDestGrantRevoke(t *testing.T) {
	w := newWorld(t)
	w.users(t)
	ctx := asUser("mem", "")
	d, err := w.orch.CreateBackupDest(ctx, &w.org, lanSpec(lanEndpoint))
	must(t, err)
	_, err = w.orch.ApproveDestGrant(ctx, d.ID, "adm", "192.168.1.106:9100")
	must(t, err)
	if _, err := w.orch.RevokeDestGrant(asUser("adm", ""), d.ID); err != nil {
		t.Fatal(err)
	}
	gs, _ := w.orch.DestGrants(ctx)
	if len(gs) != 1 || !gs[0].Pending || gs[0].ApprovedBy != "" {
		t.Fatalf("after revoke = %+v", gs)
	}
	if e := one(t, w, "grant.revoke"); e.TargetKind != "backup_dest" || e.TargetID != d.ID {
		t.Fatalf("log = %+v", e)
	}
}
