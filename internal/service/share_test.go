package service_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
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
		g := store.HostGrant{ID: "g1", StackID: tl.StackID, Lines: tl.Slug + " share:media@10.0.0.5", ApprovedBy: "adm", CreatedAt: time.Now()}
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
	g := store.HostGrant{ID: "g2", StackID: tl.StackID, Lines: tl.Slug + " share:media@10.0.0.5", ApprovedBy: "adm", CreatedAt: time.Now()}
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

// A grant approved for share:foo before foo exists dies when the owner
// creates foo.
func TestShareCreateDropsPreapprovedGrant(t *testing.T) {
	ctx := context.Background()
	r := newOrgRig(t)
	tl, err := r.env.Store.Tiles.Get(ctx, r.env.Tile(t, r.org).ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := r.env.Store.Users.Create(ctx, store.User{ID: "adm", Email: "a@b.c", Name: "A", Role: "admin", Active: true, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	g := store.HostGrant{ID: "g1", StackID: tl.StackID, Lines: tl.Slug + " share:foo@10.0.0.5", ApprovedBy: "adm", CreatedAt: now}
	if err := r.env.Store.HostGrants.Create(ctx, g); err != nil {
		t.Fatal(err)
	}
	if _, err := r.env.Orch.CreateShare(ctx, r.org, service.ShareSpec{Slug: "foo", Kind: "nfs", Source: "evil:/e"}); err != nil {
		t.Fatal(err)
	}
	if g, err := r.env.Store.HostGrants.GetByStack(ctx, tl.StackID); err == nil && strings.Contains(g.Lines, "share:foo") {
		t.Fatalf("pre-approved grant survived: %q", g.Lines)
	}
}

// shareTestRig is an org with a share on a pinned IP and a tile in one env.
func shareTestRig(t *testing.T, grant bool) (orgRig, service.Share, store.Tile) {
	t.Helper()
	ctx := context.Background()
	r := newOrgRig(t)
	tt := r.env.Tile(t, r.org)
	tl, err := r.env.Store.Tiles.Get(ctx, tt.ID)
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.env.Orch.CreateShare(ctx, r.org, service.ShareSpec{Slug: "media", Kind: "nfs", Source: "10.0.0.5:/export"})
	if err != nil {
		t.Fatal(err)
	}
	tl.Volumes = "share:media:/data"
	if err := r.env.Store.Tiles.Update(ctx, tl); err != nil {
		t.Fatal(err)
	}
	if grant {
		now := time.Now()
		if err := r.env.Store.Users.Create(ctx, store.User{ID: "adm", Email: "a@b.c", Name: "A", Role: "admin", Active: true, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		g := store.HostGrant{ID: "g1", StackID: tl.StackID, Lines: tl.Slug + " share:media@10.0.0.5", ApprovedBy: "adm", CreatedAt: now}
		if err := r.env.Store.HostGrants.Create(ctx, g); err != nil {
			t.Fatal(err)
		}
	}
	r.env.Docker.RunID = "c1"
	return r, s, tl
}

func sawRun(r orgRig) bool {
	for _, c := range r.env.Docker.Calls() {
		if c.Method == "Run" {
			return true
		}
	}
	return false
}

func sawRemove(r orgRig) bool {
	for _, c := range r.env.Docker.Calls() {
		if c.Method == "StopRemove" {
			return true
		}
	}
	return false
}

func TestShareTestNeedsGrantOrAdmin(t *testing.T) {
	ctx := context.Background()
	r, _, _ := shareTestRig(t, false)
	owner := &service.Principal{}
	_, err := r.env.Orch.TestShare(ctx, owner, r.org, "media")
	var ref errs.Refused
	if !errors.As(err, &ref) || sawRun(r) {
		t.Fatalf("no grant, not admin: err=%v ran=%v", err, sawRun(r))
	}
	if _, err = r.env.Orch.TestShare(ctx, nil, r.org, "media"); !errors.As(err, &ref) || sawRun(r) {
		t.Fatalf("nil caller: err=%v", err)
	}
	// An admin needs no grant.
	adm := &service.Principal{User: store.User{Role: "admin"}, Access: authz.User{ID: "a", Admin: true, Active: true}}
	// An admin's key capped below admin, or bound to an org or a stack, is not an admin here.
	for name, k := range map[string]authz.User{
		"capped":    {ID: "a", Admin: true, Active: true, Key: true, KeyCapped: true, KeyLevel: authz.LevelOwner},
		"org key":   {ID: "a", Admin: true, Active: true, Key: true, KeyOrg: r.org},
		"stack key": {ID: "a", Admin: true, Active: true, Key: true, KeyStack: "s1"},
		"disabled":  {ID: "a", Admin: true},
	} {
		_, err := r.env.Orch.TestShare(ctx, &service.Principal{User: adm.User, Access: k}, r.org, "media")
		if !errors.As(err, &ref) || sawRun(r) {
			t.Fatalf("%s: err=%v ran=%v", name, err, sawRun(r))
		}
	}
	res, err := r.env.Orch.TestShare(ctx, adm, r.org, "media")
	if err != nil || !res.OK || !sawRun(r) || !sawRemove(r) {
		t.Fatalf("admin: %+v %v", res, err)
	}
}

func TestShareTestReadable(t *testing.T) {
	ctx := context.Background()
	r, _, _ := shareTestRig(t, true)
	res, err := r.env.Orch.TestShare(ctx, &service.Principal{}, r.org, "media")
	if err != nil || !res.OK || res.Node == "" || res.Message != "Readable from "+res.Node+"." {
		t.Fatalf("got %+v, %v", res, err)
	}
	spec := r.env.Docker.Specs[len(r.env.Docker.Specs)-1]
	if len(spec.Volumes) != 1 || !strings.HasSuffix(spec.Volumes[0], ":/data:ro,nocopy") || spec.Restart != "no" {
		t.Fatalf("spec = %+v", spec)
	}
	if !sawRemove(r) {
		t.Fatal("helper not removed")
	}
}

// The test volume goes after the run (behind the helper, in-use ignored), and
// each run names its own helper.
func TestShareTestCleansUpVolumeAndNamesUniquely(t *testing.T) {
	ctx := context.Background()
	r, _, _ := shareTestRig(t, true)
	r.env.Docker.Err = map[string]error{"RemoveVolume": errors.New("volume is in use")}
	for range 2 {
		if res, err := r.env.Orch.TestShare(ctx, &service.Principal{}, r.org, "media"); err != nil || !res.OK {
			t.Fatalf("got %+v, %v", res, err)
		}
	}
	var order []string
	for _, c := range r.env.Docker.Calls() {
		if c.Method == "StopRemove" || c.Method == "RemoveVolume" {
			order = append(order, c.Method)
		}
	}
	if want := []string{"StopRemove", "RemoveVolume", "StopRemove", "RemoveVolume"}; !slices.Equal(order, want) {
		t.Fatalf("order = %v", order)
	}
	var names []string
	for _, sp := range r.env.Docker.Specs {
		if strings.HasPrefix(sp.Name, "stackr-share-test-") {
			names = append(names, sp.Name)
		}
	}
	if len(names) != 2 || names[0] == names[1] {
		t.Fatalf("helper names = %v", names)
	}
}

// The Test uses a volume of its own, labelled so a sweep clears a leaked one;
// the deploy's volume of the same recipe is never created or removed.
func TestShareTestNeverTouchesDeployVolume(t *testing.T) {
	r, s, _ := shareTestRig(t, true)
	if res, err := r.env.Orch.TestShare(context.Background(), &service.Principal{}, r.org, "media"); err != nil || !res.OK {
		t.Fatalf("got %+v, %v", res, err)
	}
	deploy := volume.ShareVolumeName(s, "")
	if len(r.env.Docker.Created) != 1 {
		t.Fatalf("created = %+v", r.env.Docker.Created)
	}
	c := r.env.Docker.Created[0]
	if !strings.HasPrefix(c.Name, "stackr-sharetest-") || strings.HasPrefix(c.Name, deploy) || c.Labels[volume.ShareLabel] != s.ID {
		t.Fatalf("created %+v, deploy name %s", c, deploy)
	}
	for _, call := range r.env.Docker.Calls() {
		if call.Method == "RemoveVolume" && call.Args[0] != c.Name {
			t.Fatalf("removed %v", call)
		}
	}
	if !slices.ContainsFunc(r.env.Docker.Calls(), func(x dockerfake.Call) bool { return x.Method == "RemoveVolume" }) {
		t.Fatal("test volume not removed")
	}
}

func TestShareTestDenied(t *testing.T) {
	ctx := context.Background()
	raw := "mount: permission denied, user=bob,password=hunter2 nfs"
	for name, setup := range map[string]func(r orgRig){
		"mount error": func(r orgRig) {
			r.env.Docker.Err = map[string]error{"Run": docker.MountDeniedError{Volume: "v", Raw: raw}}
		},
		"exit code": func(r orgRig) { r.env.Docker.ExitCode, r.env.Docker.LogsOut = 2, raw },
	} {
		t.Run(name, func(t *testing.T) {
			r, s, _ := shareTestRig(t, true)
			setup(r)
			res, err := r.env.Orch.TestShare(ctx, &service.Principal{}, r.org, "media")
			want := "The NAS refused access to share media (" + s.Source + "). It likely maps root to nobody: give Other read and execute on the export's top folder, or set maproot."
			if err != nil || res.OK || res.Message != want {
				t.Fatalf("got %+v, %v", res, err)
			}
			if !strings.Contains(res.Raw, "permission denied") || strings.Contains(res.Raw, "hunter2") {
				t.Fatalf("raw = %q", res.Raw)
			}
			if !sawRemove(r) {
				t.Fatal("helper not removed")
			}
		})
	}
}

func TestShareTestOtherFailure(t *testing.T) {
	r, _, _ := shareTestRig(t, true)
	r.env.Docker.ExitCode, r.env.Docker.LogsOut = 1, "ls: /data: no such file"
	res, err := r.env.Orch.TestShare(context.Background(), &service.Principal{}, r.org, "media")
	if err != nil || res.OK || !strings.HasPrefix(res.Message, "Could not mount share media from ") || res.Raw == "" {
		t.Fatalf("got %+v, %v", res, err)
	}
}

// A wait that never ends is cut at the run bound and the helper still goes.
func TestShareTestTimeoutCleansUp(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the run bound")
	}
	r, _, _ := shareTestRig(t, true)
	r.env.Docker.WaitBlock = true
	res, err := r.env.Orch.TestShare(context.Background(), &service.Principal{}, r.org, "media")
	if err != nil || res.OK || !sawRemove(r) {
		t.Fatalf("got %+v, %v, removed=%v", res, err, sawRemove(r))
	}
}

// With no tile mounting the share the test runs on the controller; with one
// it runs on that env's node and names it.
func TestShareTestNodePick(t *testing.T) {
	ctx := context.Background()
	r, _, tl := shareTestRig(t, true)
	ctl, err := r.env.Orch.TestShare(ctx, &service.Principal{}, r.org, "media")
	if err != nil || !ctl.OK {
		t.Fatal(ctl, err)
	}
	now := time.Now()
	if err := r.env.Store.Nodes.Create(ctx, store.Node{ID: "w9", Name: "worker9", Slug: "w9", Role: "worker", Tags: "[]", RangeCIDR: "10.211.0.0/16", Status: "online", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := r.env.Store.Environments.SetNode(ctx, tl.EnvironmentID, "w9"); err != nil {
		t.Fatal(err)
	}
	w, err := r.env.Orch.TestShare(ctx, &service.Principal{}, r.org, "media")
	if err != nil || w.Node != "worker9" || w.Node == ctl.Node {
		t.Fatalf("worker pick = %+v, %v (controller %q)", w, err, ctl.Node)
	}
	tl.Volumes = ""
	if err := r.env.Store.Tiles.Update(ctx, tl); err != nil {
		t.Fatal(err)
	}
	adm := &service.Principal{User: store.User{Role: "admin"}, Access: authz.User{ID: "a", Admin: true, Active: true}}
	back, err := r.env.Orch.TestShare(ctx, adm, r.org, "media")
	if err != nil || back.Node != ctl.Node {
		t.Fatalf("unmounted = %+v, %v", back, err)
	}
}

// A non-admin's grant covers only its own stack: a tile mounting the share in
// another stack must not pick the node, and with none in the granted stack
// the test is refused (not run on the controller). An admin keeps the old pick.
func TestShareTestNonAdminNeedsTileInApprovedStack(t *testing.T) {
	ctx := context.Background()
	r, _, _ := shareTestRig(t, false)
	st2, err := r.env.Orch.CreateStack(ctx, r.org, "other", "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := r.env.Store.Users.Create(ctx, store.User{ID: "adm", Email: "a@b.c", Name: "A", Role: "admin", Active: true, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	g := store.HostGrant{ID: "g2", StackID: st2.ID, Lines: "api share:media@10.0.0.5", ApprovedBy: "adm", CreatedAt: now}
	if err := r.env.Store.HostGrants.Create(ctx, g); err != nil {
		t.Fatal(err)
	}
	_, err = r.env.Orch.TestShare(ctx, &service.Principal{}, r.org, "media")
	var c errs.Conflict
	if !errors.As(err, &c) || !strings.Contains(err.Error(), "approved stack") || sawRun(r) {
		t.Fatalf("non-admin: err=%v ran=%v", err, sawRun(r))
	}
	adm := &service.Principal{User: store.User{Role: "admin"}, Access: authz.User{ID: "a", Admin: true, Active: true}}
	if res, err := r.env.Orch.TestShare(ctx, adm, r.org, "media"); err != nil || !res.OK {
		t.Fatalf("admin: %+v %v", res, err)
	}
}
