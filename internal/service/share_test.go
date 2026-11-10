package service_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// The org file creates, updates and (when the approver ticks the removal row)
// deletes shares through the verbs, and a mounted share cannot be deleted by
// the verb.
func TestOrgFileShares(t *testing.T) {
	ctx := context.Background()
	r := newOrgRig(t)
	r.orgFile(t, `version: 1
org: acme
shares:
  media:
    kind: nfs
    source: nas:/export
  docs:
    kind: smb
    source: //nas/docs
    user: bob
    password: ${{ org.params.nas.pw }}
`)
	r.bind(t, true) // config_auto: each plan applies itself
	eventually(t, "both shares", func() bool {
		ss, _ := r.env.Orch.Shares(ctx, r.org)
		return len(ss) == 2
	})
	ss, err := r.env.Orch.Shares(ctx, r.org)
	if err != nil || len(ss) != 2 || ss[0].Slug != "docs" || ss[1].Source != "nas:/export" {
		t.Fatalf("shares = %+v, %v", ss, err)
	}

	// A volume of media's current recipe, held by no container, stands for
	// a deploy's leftover; the edit must sweep it (no tile row names it).
	media := ss[1]
	r.env.Docker.Volumes = append(r.env.Docker.Volumes, docker.VolumeInfo{
		Name: "stackr-share-" + media.ID + "-old", Labels: map[string]string{"stackr.share": media.ID},
	})

	// An edit updates in place; a share the block drops is deleted.
	sha := r.orgFile(t, `version: 1
org: acme
shares:
  media:
    kind: nfs
    source: nas:/export
    options: nfsvers=4
`)
	r.push(t, "acme/org", sha)
	// the drop is a removal row, so the plan waits for an approve even with
	// config_auto on; the approver ticks it
	var pl service.OrgPlan
	eventually(t, "the plan with the removal row", func() bool {
		pl = r.plans(t)[0]
		return pl.Commit == sha && pl.Status == "pending"
	})
	if now, _ := r.env.Orch.Shares(ctx, r.org); len(now) != 2 {
		t.Fatalf("an unapproved plan changed the shares: %+v", now)
	}
	if _, err := r.env.Orch.ApproveOrgPlan(ctx, pl.ID, service.ApproveOpts{Ticked: []string{"share:docs"}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "media updated, docs gone", func() bool {
		ss, _ := r.env.Orch.Shares(ctx, r.org)
		return len(ss) == 1 && ss[0].Options == "nfsvers=4"
	})
	// the sweep is a reconcile job, so it lands a moment after the edit
	eventually(t, "the old recipe's volume swept", func() bool {
		for _, c := range r.env.Docker.Calls() {
			if c.String() == "RemoveVolume(stackr-share-"+media.ID+"-old)" {
				return true
			}
		}
		return false
	})
}

func TestDeleteShareMounted(t *testing.T) {
	ctx := context.Background()
	r := newOrgRig(t)
	tl := r.env.Tile(t, r.org)
	if _, err := r.env.Orch.CreateShare(ctx, r.org, service.ShareSpec{Slug: "media", Kind: "nfs", Source: "nas:/e"}); err != nil {
		t.Fatal(err)
	}
	row, err := r.env.Store.Tiles.Get(ctx, tl.ID)
	if err != nil {
		t.Fatal(err)
	}
	row.Volumes = "share:media/a:/data"
	if err := r.env.Store.Tiles.Update(ctx, row); err != nil {
		t.Fatal(err)
	}
	if err := r.env.Orch.DeleteShare(ctx, r.org, "media"); !isConflict(err) {
		t.Errorf("delete while mounted = %v, want Conflict", err)
	}
	row.Volumes = ""
	if err := r.env.Store.Tiles.Update(ctx, row); err != nil {
		t.Fatal(err)
	}
	if err := r.env.Orch.DeleteShare(ctx, r.org, "media"); err != nil {
		t.Error(err)
	}
}

// Deleting a share, or changing where it points, takes back the admin's
// approval to mount it in the same transaction: a recreated name has none.
func TestShareGrantDroppedWithShare(t *testing.T) {
	ctx := context.Background()
	r := newOrgRig(t)
	tt := r.env.Tile(t, r.org)
	tl, err := r.env.Store.Tiles.Get(ctx, tt.ID)
	if err != nil {
		t.Fatal(err)
	}
	grant := func() bool {
		g, err := r.env.Store.HostGrants.GetByStack(ctx, tl.StackID)
		return err == nil && strings.Contains(g.Lines, "share:media")
	}
	now := time.Now()
	if err := r.env.Store.Users.Create(ctx, store.User{ID: "adm", Email: "a@b.c", Name: "A", Role: "admin", Active: true, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	seed := func() {
		if _, err := r.env.Orch.CreateShare(ctx, r.org, service.ShareSpec{Slug: "media", Kind: "nfs", Source: "nas:/e"}); err != nil {
			t.Fatal(err)
		}
		g := store.HostGrant{ID: "g1", StackID: tl.StackID, Lines: tl.Slug + " share:media", ApprovedBy: "adm", CreatedAt: time.Now()}
		if err := r.env.Store.HostGrants.Create(ctx, g); err != nil {
			t.Fatal(err)
		}
	}
	seed()
	// an edit that changes nothing about the server keeps it
	if _, err := r.env.Orch.UpdateShare(ctx, r.org, "media", service.ShareSpec{Slug: "media", Kind: "nfs", Source: "nas:/e"}); err != nil || !grant() {
		t.Fatalf("same recipe: %v grant=%v", err, grant())
	}
	if _, err := r.env.Orch.UpdateShare(ctx, r.org, "media", service.ShareSpec{Slug: "media", Kind: "nfs", Source: "evil:/e"}); err != nil || grant() {
		t.Fatalf("repointed: %v grant=%v", err, grant())
	}
	g := store.HostGrant{ID: "g2", StackID: tl.StackID, Lines: tl.Slug + " share:media", ApprovedBy: "adm", CreatedAt: time.Now()}
	if err := r.env.Store.HostGrants.Create(ctx, g); err != nil {
		t.Fatal(err)
	}
	if err := r.env.Orch.DeleteShare(ctx, r.org, "media"); err != nil || grant() {
		t.Fatalf("deleted: %v grant=%v", err, grant())
	}
	if _, err := r.env.Orch.CreateShare(ctx, r.org, service.ShareSpec{Slug: "media", Kind: "nfs", Source: "other:/e"}); err != nil || grant() {
		t.Fatalf("recreated: %v grant=%v", err, grant())
	}
}
