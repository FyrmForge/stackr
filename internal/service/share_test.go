package service_test

import (
	"context"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
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
	swept := false
	for _, c := range r.env.Docker.Calls() {
		swept = swept || c.String() == "RemoveVolume(stackr-share-"+media.ID+"-old)"
	}
	if !swept {
		t.Error("the org file edit did not sweep the old recipe's volume")
	}
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
