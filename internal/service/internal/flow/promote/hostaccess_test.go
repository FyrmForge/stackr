package promote

import (
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/hostgrant"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

const sockFile = `
version: 1
stack: shop
ladder: [dev, prd]
head: main
base:
  tiles:
    mon:
      image: nginx:1
      port: 80
      volumes: ["host:/var/run/docker.sock:/var/run/docker.sock:ro"]
environments:
  dev: {}
  prd: {}
`

// A host line parks the promote on an admin; approved, the same release
// deploys with the bind; a changed line or a privileged flag parks again; a
// re-apply of the approved release does not.
func TestHostAccessParksUntilApproved(t *testing.T) {
	w := setup(t)
	w.f.D.HostGrants = hostgrant.New(w.s.HostGrants)
	w.files["c1"] = sockFile
	r := w.release(t, "c1")

	p, err := w.f.Plan(ctx, w.dev.ID, r.ID, io.Discard)
	must(t, err)
	want := "elevated access: mon host:/var/run/docker.sock:/var/run/docker.sock:ro"
	if len(p.Blockers) != 1 || p.Blockers[0] != want {
		t.Fatalf("blockers = %q; want %q", p.Blockers, want)
	}
	_, err = w.f.Apply(ctx, w.dev.ID, r.ID, io.Discard, nil)
	n, ok := errs.IsNeedsApproval(err)
	if !ok || n.Stack != w.st.ID || n.What != want {
		t.Fatalf("apply = %v; want NeedsApproval %q", err, want)
	}

	ask, ok := hostgrant.Parse(n.What)
	if !ok {
		t.Fatal("the approval text does not parse back")
	}
	now := time.Now()
	must(t, w.s.Users.Create(ctx, store.User{ID: "adm", Email: "a@b.c", Name: "A", Role: "admin", Active: true, CreatedAt: now, UpdatedAt: now}))
	_, err = w.f.D.HostGrants.Approve(ctx, w.st.ID, "adm", ask)
	must(t, err)
	if _, err = w.f.Apply(ctx, w.dev.ID, r.ID, io.Discard, nil); err != nil {
		t.Fatalf("apply after approval: %v", err)
	}
	bound := false
	for _, s := range w.fake.Specs {
		bound = bound || slices.Contains(s.Volumes, "/var/run/docker.sock:/var/run/docker.sock:ro")
	}
	if !bound {
		t.Errorf("no container got the bind: %+v", w.fake.Specs)
	}
	w.running(w.tileIn(t, w.dev, "mon"))
	if p, err = w.f.Plan(ctx, w.dev.ID, r.ID, io.Discard); err != nil || p.Blocked() {
		t.Fatalf("same release again: %v %v", p.Blockers, err)
	}

	// A changed line is new access.
	w.files["c2"] = strings.Replace(sockFile, "/var/run/docker.sock:/var/run/docker.sock:ro", "/etc:/host-etc", 1)
	if p, err = w.f.Plan(ctx, w.dev.ID, w.release(t, "c2").ID, io.Discard); err != nil || len(p.Blockers) != 1 ||
		!strings.Contains(p.Blockers[0], "mon host:/etc:/host-etc") {
		t.Fatalf("changed line: %v %v", p.Blockers, err)
	}

	// Privileged with no grant parks too.
	w.files["c3"] = strings.Replace(sockFile, "      port: 80\n", "      port: 80\n      privileged: true\n", 1)
	if p, err = w.f.Plan(ctx, w.dev.ID, w.release(t, "c3").ID, io.Discard); err != nil || len(p.Blockers) != 1 ||
		p.Blockers[0] != "elevated access: mon privileged" {
		t.Fatalf("privileged: %v %v", p.Blockers, err)
	}

	// Another blocker beside it fails plainly: approving would not help.
	w.files["c4"] = strings.Replace(w.files["c3"], "image: nginx:1", "image: nginx:1\n      engine: postgres", 1)
	r4 := w.release(t, "c4")
	if _, err = w.f.Apply(ctx, w.dev.ID, r4.ID, io.Discard, nil); err == nil {
		t.Fatal("applied a blocked plan")
	} else if _, ok := errs.IsNeedsApproval(err); ok {
		t.Fatalf("parked with another blocker: %v", err)
	}
}

// An env sync that copies a host line plans the same blocker and, applied,
// parks on an admin instead of writing anything.
func TestSyncHostAccessParks(t *testing.T) {
	w := syncWorld(t)
	w.f.D.HostGrants = hostgrant.New(w.s.HostGrants)
	row := imgTile("nginx:1", 80)
	row.Volumes = "host:/srv/x:/data"
	w.mk(t, w.dev, "mon", row)

	s := w.syncPlan(t)
	if len(s.Blockers) != 1 || !s.OnlyHostAccess() {
		t.Fatalf("blockers = %q", s.Blockers)
	}
	_, err := w.syncApply(t, s, "mon")
	if _, ok := errs.IsNeedsApproval(err); !ok {
		t.Fatalf("apply = %v, want NeedsApproval", err)
	}
	if _, err := w.f.D.Tiles.GetBySlug(ctx, w.prd.ID, "mon"); err == nil {
		t.Error("the sync wrote a tile before approval")
	}
}

// A promote that parks at deploy time after SetRelease moved the env pointer
// must, once approved, redeploy the tile still running the old image: the
// resume sees an empty release diff.
func TestParkedPromoteResumesDeploy(t *testing.T) {
	w := syncWorld(t)
	w.f.D.HostGrants = hostgrant.New(w.s.HostGrants)
	row := imgTile("nginx:1", 80)
	row.Volumes = "host:/srv/x:/data"
	api := w.mk(t, w.prd, "api", row)
	w.fake.Containers = append(w.fake.Containers, docker.Container{
		ID:     "run-" + api.ID,
		State:  "running",
		Image:  "nginx@sha256:one",
		Labels: map[string]string{tile.LabelTile: api.ID, tile.LabelRole: "replica"},
	})
	r1 := w.release(t, "", release.Pin{Slug: "api", Repo: "nginx", Digest: "sha256:one"})
	r2 := w.release(t, "", release.Pin{Slug: "api", Repo: "nginx", Digest: "sha256:two"})
	_, err := w.f.D.Envs.SetRelease(ctx, w.prd, r1.ID)
	must(t, err)
	_, err = w.f.D.Envs.SetRelease(ctx, w.dev, r2.ID)
	must(t, err)

	_, err = w.f.Apply(ctx, w.prd.ID, r2.ID, io.Discard, nil)
	n, ok := errs.IsNeedsApproval(err)
	if !ok {
		t.Fatalf("apply = %v, want NeedsApproval", err)
	}
	ask, _ := hostgrant.Parse(n.What)
	now := time.Now()
	must(t, w.s.Users.Create(ctx, store.User{ID: "adm", Email: "a@b.c", Name: "A", Role: "admin", Active: true, CreatedAt: now, UpdatedAt: now}))
	_, err = w.f.D.HostGrants.Approve(ctx, w.st.ID, "adm", ask)
	must(t, err)

	p, err := w.f.Apply(ctx, w.prd.ID, r2.ID, io.Discard, nil)
	must(t, err)
	if !slices.Equal(p.Deployed, []string{api.ID}) {
		t.Errorf("deployed = %v; want the parked tile on the new release", p.Deployed)
	}
}

// A tile that refs a host-network tile gets a plan warning: no slug or VIP
// reaches it.
func TestPlanWarnsOnRefToHostNetworkTile(t *testing.T) {
	w := setup(t)
	w.f.D.HostGrants = hostgrant.New(w.s.HostGrants)
	w.files["c1"] = `
version: 1
stack: shop
ladder: [dev, prd]
head: main
base:
  tiles:
    mon:
      image: nginx:1
      port: 80
      network: host
    web:
      image: nginx:1
      port: 80
      env:
        MON: "${{ tile.mon.url }}"
environments:
  dev: {}
  prd: {}
`
	p, err := w.f.Plan(ctx, w.dev.ID, w.release(t, "c1").ID, io.Discard)
	must(t, err)
	want := "tile web refs mon, which runs on the host network; reach it by the server address and a granted port"
	if !slices.Contains(p.Warnings, want) {
		t.Errorf("warnings = %q; want %q", p.Warnings, want)
	}
}
