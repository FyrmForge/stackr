package service

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// addPlacement makes a placement on a node ("" = none yet).
func addPlacement(t *testing.T, n *twoNodes, name, nodeID string, all bool, orgs ...string) store.Placement {
	t.Helper()
	ctx := context.Background()
	p := store.Placement{ID: uuid.NewString(), Name: name, NodeID: nodeID, OpenToAll: all, CreatedAt: time.Now().UTC()}
	must(t, n.st.Placements.Create(ctx, p))
	must(t, n.st.PlacementOrgs.Set(ctx, p.ID, orgs))
	return p
}

// placeW1 gives node w1 the placement "w1" (closed to every org unless all)
// and files the edge env under it, as newTwoNodes would if it knew them.
func (n *twoNodes) placeW1(t *testing.T, all bool) store.Placement {
	t.Helper()
	p := addPlacement(t, n, "w1", "w1", all)
	must(t, n.st.Environments.SetPlacement(context.Background(), n.edge.ID, p.ID))
	return p
}

func acmeID(t *testing.T, n *twoNodes) string {
	t.Helper()
	og, err := n.st.Orgs.GetBySlug(context.Background(), "acme")
	must(t, err)
	return og.ID
}

// An admin makes, opens and deletes placements; nobody else does. The
// default one is listed first and is never deleted.
func TestPlacementAdminVerbs(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	adm, own := adminP(), ownerP(acmeID(t, n))

	if _, err := n.orch.CreatePlacement(ctx, own, PlacementSpec{Name: "eu", OpenToAll: true}); err == nil {
		t.Fatal("a non-admin made a placement")
	}
	for _, bad := range []string{"", "Eu", "a b", "-x"} {
		if _, err := n.orch.CreatePlacement(ctx, adm, PlacementSpec{Name: bad, OpenToAll: true}); !errors.As(err, new(errs.Invalid)) {
			t.Errorf("name %q = %v", bad, err)
		}
	}
	if _, err := n.orch.CreatePlacement(ctx, adm, PlacementSpec{Name: "eu", Node: "nope", OpenToAll: true}); !errors.As(err, new(errs.Invalid)) {
		t.Errorf("unknown node = %v", err)
	}
	if _, err := n.orch.CreatePlacement(ctx, adm, PlacementSpec{Name: "eu", OpenToAll: false, Orgs: []string{"nope"}}); !errors.As(err, new(errs.Invalid)) {
		t.Errorf("unknown org = %v", err)
	}
	wait, err := n.orch.CreatePlacement(ctx, adm, PlacementSpec{Name: "waits", OpenToAll: true})
	must(t, err)
	if wait.NodeID != "" || wait.Online || wait.NodeName != "" || wait.Default {
		t.Errorf("a placement with no node = %+v", wait)
	}
	eu, err := n.orch.CreatePlacement(ctx, adm, PlacementSpec{Name: "eu", Node: "w1", Orgs: []string{"acme"}})
	must(t, err)
	if eu.NodeID != "w1" || eu.NodeName != "w1" || eu.OpenToAll || len(eu.Orgs) != 1 || eu.Orgs[0] != "acme" || !eu.Online {
		t.Errorf("eu = %+v", eu)
	}
	if _, err := n.orch.CreatePlacement(ctx, adm, PlacementSpec{Name: "eu", OpenToAll: true}); !isConflict(err) {
		t.Errorf("duplicate name = %v", err)
	}

	all, err := n.orch.Placements(ctx)
	must(t, err)
	if len(all) != 3 || all[0].Name != DefaultPlacement || !all[0].Default || all[0].NodeName != n.ctl.Name || all[1].Name != "eu" || all[2].Name != "waits" {
		t.Fatalf("placements = %+v", all)
	}
	if all[0].Envs != 1 { // dev
		t.Errorf("default holds %d envs, want 1", all[0].Envs)
	}

	// open to others, then back
	if err := n.orch.SetPlacementOpen(ctx, own, "eu", true, nil); err == nil {
		t.Error("a non-admin opened a placement")
	}
	must(t, n.orch.SetPlacementOpen(ctx, adm, "eu", true, nil))
	if got, _ := n.orch.PlacementByName(ctx, "eu"); !got.OpenToAll || len(got.Orgs) != 0 {
		t.Errorf("after open to all = %+v", got)
	}
	must(t, n.orch.SetPlacementOpen(ctx, adm, "eu", false, []string{"acme"}))
	if got, _ := n.orch.PlacementByName(ctx, "eu"); got.OpenToAll || len(got.Orgs) != 1 {
		t.Errorf("after open to acme = %+v", got)
	}
	if err := n.orch.SetPlacementOpen(ctx, adm, "eu", false, []string{"nope"}); err == nil {
		t.Error("opened to an org that does not exist")
	}
	if err := n.orch.SetPlacementOpen(ctx, adm, "nope", true, nil); err == nil {
		t.Error("opened a placement that does not exist")
	}

	// delete
	if err := n.orch.DeletePlacement(ctx, own, "waits"); err == nil {
		t.Error("a non-admin deleted a placement")
	}
	if err := n.orch.DeletePlacement(ctx, adm, DefaultPlacement); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("delete default = %v", err)
	}
	_, err = n.orch.PinScope(ctx, adm, ScopeOrg, acmeID(t, n), "eu", "", "dev") // dev, on the controller, stays
	must(t, err)
	if err := n.orch.DeletePlacement(ctx, adm, "eu"); !isConflict(err) {
		t.Errorf("delete a pinned placement = %v", err)
	}
	_, err = n.orch.PinScope(ctx, adm, ScopeOrg, acmeID(t, n), "", "", "edge") // edge stays on eu
	must(t, err)
	if err := n.orch.DeletePlacement(ctx, adm, "eu"); !isConflict(err) { // the edge env followed the pin
		t.Errorf("delete a placement holding an env = %v", err)
	}
	must(t, n.st.Environments.SetPlacement(ctx, n.edge.ID, mustPlacement(t, n, DefaultPlacement).ID))
	must(t, n.orch.DeletePlacement(ctx, adm, "eu"))
	must(t, n.orch.DeletePlacement(ctx, adm, "waits"))
	if _, err := n.orch.PlacementByName(ctx, "eu"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("deleted placement = %v", err)
	}
	for a, want := range map[string]int{auditPlacementNew: 2, auditPlacementOpen: 2, auditPlacementDelete: 2} {
		if c := auditCount(t, n, a); c != want {
			t.Errorf("%s rows = %d, want %d", a, c, want)
		}
	}
}

// A closed placement is refused to another org's owner at every verb that
// takes one; an admin goes anywhere. The default is open to all.
func TestPlacementAccess(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	adm := adminP()
	acme := acmeID(t, n)
	other := uuid.NewString()
	must(t, n.st.Orgs.Create(ctx, store.Org{ID: other, Name: "globex", Slug: "globex", EnvColors: "{}", Settings: "{}", CreatedAt: time.Now()}))
	must(t, n.st.PlacementOrgs.Set(ctx, addPlacement(t, n, "w1", "w1", false).ID, []string{acme}))

	if ok, err := n.orch.placementOpenTo(ctx, mustPlacement(t, n, "w1").ID, acme); err != nil || !ok {
		t.Errorf("acme on w1 = %v, %v", ok, err)
	}
	if ok, _ := n.orch.placementOpenTo(ctx, mustPlacement(t, n, "w1").ID, other); ok {
		t.Error("globex may use a placement closed to it")
	}
	if ok, _ := n.orch.placementOpenTo(ctx, mustPlacement(t, n, DefaultPlacement).ID, other); !ok {
		t.Error("the default placement is not open to every org")
	}
	if err := n.orch.SetPin(ctx, ownerP(other), ScopeOrg, other, "w1"); err == nil || !strings.Contains(err.Error(), "not open") {
		t.Errorf("globex pinned to a closed placement = %v", err)
	}
	if err := n.orch.SetIngress(ctx, ownerP(other), ScopeOrg, other, "w1"); err == nil {
		t.Error("globex picked a closed placement as ingress")
	}
	if err := n.orch.SetExits(ctx, ownerP(other), ScopeOrg, other, []string{"w1"}); err == nil {
		t.Error("globex picked a closed placement as an exit")
	}
	addPlacement(t, n, "gx", "w1", false, other)
	if _, err := n.orch.PlacementForNewEnv(ctx, ownerP(acme), n.stack, "gx"); err == nil {
		t.Error("acme placed an env on a placement closed to it")
	}
	if err := n.orch.SetPin(ctx, adm, ScopeOrg, other, "w1"); err != nil {
		t.Errorf("an admin pinned anywhere = %v", err)
	}
	// the dev list: acme sees both, globex only the default
	offers := func(org string) []string {
		os, err := n.orch.OrgPlacements(ctx, org)
		must(t, err)
		var out []string
		for _, o := range os {
			out = append(out, o.Name)
		}
		return out
	}
	if got := offers(acme); strings.Join(got, ",") != "default,w1" {
		t.Errorf("acme offers %v", got)
	}
	if got := offers(other); strings.Join(got, ",") != "default,gx" {
		t.Errorf("globex offers %v", got)
	}
}

func mustPlacement(t *testing.T, n *twoNodes, name string) store.Placement {
	t.Helper()
	p, err := n.st.Placements.GetByName(context.Background(), name)
	must(t, err)
	return p
}

// The dev list says whether a placement is usable and never names a node.
func TestOrgPlacementsOffers(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	acme := acmeID(t, n)
	addPlacement(t, n, "eu-prod", "w1", true)
	addPlacement(t, n, "later", "", true)
	got, err := n.orch.OrgPlacements(ctx, acme)
	must(t, err)
	byName := map[string]PlacementOffer{}
	for _, o := range got {
		byName[o.Name] = o
	}
	if o := byName["eu-prod"]; !o.HasNode || !o.Online || !o.Usable || o.Why != "" {
		t.Errorf("eu-prod = %+v", o)
	}
	if o := byName["later"]; o.HasNode || o.Usable || o.Why != "no node yet" {
		t.Errorf("later = %+v", o)
	}
	w, _ := n.st.Nodes.Get(ctx, "w1")
	w.Status = "offline"
	must(t, n.st.Nodes.Update(ctx, w))
	got, _ = n.orch.OrgPlacements(ctx, acme)
	for _, o := range got {
		if o.Name == "eu-prod" && (o.Usable || o.Why != "offline" || o.Online) {
			t.Errorf("eu-prod offline = %+v", o)
		}
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), `"w1"`) {
		t.Errorf("the dev list names a node: %s", b)
	}
}

// Volumes and managed tiles need a node tagged storage (planfile.Fits).
func TestPlacementFit(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	adm := adminP()
	addPlacement(t, n, "plain", "w1", true) // w1 has no tags
	if got, err := n.orch.envNeeds(ctx, n.dev.ID); err != nil || got.Storage {
		t.Fatalf("a bare env needs %+v, %v", got, err)
	}

	// a volume on dev
	must(t, n.st.Volumes.Create(ctx, store.Volume{ID: "v1", ScopeKind: "env", ScopeID: n.dev.ID, Slug: "data", Name: "dev-data", CreatedAt: time.Now()}))
	if got, _ := n.orch.envNeeds(ctx, n.dev.ID); !got.Storage {
		t.Error("an env with a volume does not need storage")
	}
	err := n.orch.SetPin(ctx, adm, ScopeEnv, n.dev.ID, "plain")
	if !errors.As(err, new(errs.Invalid)) || !strings.Contains(err.Error(), "needs storage") {
		t.Fatalf("pinned a volume env to a node without storage = %v", err)
	}
	if err := n.orch.SetPin(ctx, adm, ScopeStack, n.stack, "plain"); err == nil {
		t.Error("pinned a stack holding a volume env to a node without storage")
	}
	mustTag(t, n, adm, "w1", "storage")
	if err := n.orch.SetPin(ctx, adm, ScopeStack, n.stack, "plain"); !errors.As(err, new(*MoveNeeded)) {
		t.Errorf("pin to a storage node = %v, want the move of dev to confirm", err)
	}
	// and the tag cannot go while that data sits there
	must(t, n.st.Environments.SetNode(ctx, n.dev.ID, "w1"))
	if err := n.orch.SetNodeTags(ctx, adm, "w1", nil); !isConflict(err) {
		t.Errorf("dropped storage under a volume env = %v", err)
	}

	// a managed tile counts too
	_, err = n.orch.CreateManagedTile(ctx, Tile{EnvironmentID: n.edge.ID, Name: "db", MemLimitMB: 512}, "postgres")
	must(t, err)
	if got, _ := n.orch.envNeeds(ctx, n.edge.ID); !got.Storage {
		t.Error("an env with a managed tile does not need storage")
	}
}

func mustTag(t *testing.T, n *twoNodes, by *Principal, node string, add ...string) []string {
	t.Helper()
	out, err := n.orch.TagNode(context.Background(), by, node, TagSpec{Add: add})
	must(t, err)
	return out.Tags
}

// A new env lands on the placement its pins name, or waits with no node when
// the placement has none; its deploy is refused with the placement's name.
func TestEnvCreateOnPlacement(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	adm := adminP()
	acme := acmeID(t, n)
	w1 := addPlacement(t, n, "eu-prod", "w1", true)
	later := addPlacement(t, n, "later", "", true)

	e, err := n.orch.CreateEnv(ctx, n.stack, "qa", EnvSpec{Type: "static", FromKind: "branch", FromBranch: "main"})
	must(t, err)
	if def := mustPlacement(t, n, DefaultPlacement); e.NodeID != n.ctl.ID || e.PlacementID != def.ID {
		t.Fatalf("unpinned env on %q in %q", e.NodeID, e.PlacementID)
	}

	_, err = n.orch.PinScope(ctx, adm, ScopeOrg, acme, "eu-prod", "", "dev", "qa") // the existing envs stay: new ones are under test
	must(t, err)
	e, err = n.orch.CreateEnv(ctx, n.stack, "stage", EnvSpec{Type: "static", FromKind: "branch", FromBranch: "main"})
	must(t, err)
	if e.NodeID != "w1" || e.PlacementID != w1.ID {
		t.Fatalf("org pin: env on %q in %q", e.NodeID, e.PlacementID)
	}
	// the stack's pin beats the org's, and a placement with no node leaves the env waiting
	must(t, n.orch.SetPin(ctx, adm, ScopeStack, n.stack, "later"))
	e, err = n.orch.CreateEnv(ctx, n.stack, "prod", EnvSpec{Type: "static", FromKind: "branch", FromBranch: "main"})
	must(t, err)
	if e.NodeID != "" || e.PlacementID != later.ID {
		t.Fatalf("nodeless pin: env on %q in %q", e.NodeID, e.PlacementID)
	}
	tl := n.tile(t, e)
	var w *PlacementNoNodeError
	if _, err := n.orch.Deploy(ctx, tl.ID); !errors.As(err, &w) || w.Placement != "later" || !errors.Is(err, errs.ErrBusy) {
		t.Fatalf("deploy of an env with no node = %v", err)
	}
	if err := n.orch.gateEnv(ctx, e.ID, false); !errors.As(err, &w) {
		t.Errorf("gate of an env with no node = %v", err)
	}
	if !strings.Contains(w.Error(), "placement later has no node yet") {
		t.Errorf("message = %q", w.Error())
	}
	// the boot's adopt step leaves it waiting
	if c, err := n.st.Environments.AdoptUnplaced(ctx, n.ctl.ID); err != nil || c != 0 {
		t.Errorf("adopted %d waiting envs, %v", c, err)
	}
	if got, _ := n.st.Environments.Get(ctx, e.ID); got.NodeID != "" {
		t.Errorf("the waiting env moved to %q", got.NodeID)
	}
	// a waiting env and its tiles can still be deleted: nothing ran anywhere
	dj, err := n.orch.DeleteTile(ctx, tl.ID)
	must(t, err)
	if j := n.wait(t, dj); j.State != "done" {
		t.Fatalf("delete a waiting env's tile = %s %s", j.State, j.Error)
	}
	if err := n.orch.DeleteEnv(ctx, e.ID); err != nil {
		t.Fatalf("delete a waiting env = %v", err)
	}

	// an offline node is refused for an explicit placement, not for a pin
	w1n, _ := n.st.Nodes.Get(ctx, "w1")
	w1n.Status = "offline"
	must(t, n.st.Nodes.Update(ctx, w1n))
	if _, err := n.orch.PlacementForNewEnv(ctx, adm, n.stack, "eu-prod"); !isConflict(err) {
		t.Errorf("placement on an offline node = %v", err)
	}
}

// An env with no tiles and no volumes moves without a typed confirm: nothing
// stops and no data moves.
func TestPinEmptyEnvNeedsNoConfirm(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	adm := adminP()
	n.placeW1(t, true)
	addPlacement(t, n, "w1-twin", "w1", true)
	empty, err := n.orch.CreateEnv(ctx, n.stack, "empty", EnvSpec{Type: environment.Static, FromKind: environment.FromBranch, FromBranch: "main"})
	must(t, err)
	js, err := n.orch.PinScope(ctx, adm, ScopeEnv, empty.ID, "w1-twin", "")
	if err != nil || len(js) != 1 {
		t.Fatalf("empty env pin = %v, %v; want its move queued without a confirm", js, err)
	}
	// dev has a tile: its pin still asks
	if _, err := n.orch.PinScope(ctx, adm, ScopeEnv, n.dev.ID, "w1-twin", ""); !errors.As(err, new(*MoveNeeded)) {
		t.Errorf("env with a tile = %v, want the move to confirm", err)
	}
}

// Org and stack pins carry an env that is on the same node and move the
// ones on another node only once confirmed with the scope's slug (D4); an env
// pin that would move it is refused with the plan to confirm. Pinning an env
// to the placement it already holds, or to another on its own node, only
// relabels it.
func TestSetPinFollowsAndRefusesMoves(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	adm := adminP()
	w1 := n.placeW1(t, true)
	twin := addPlacement(t, n, "w1-twin", "w1", true)

	// dev is on the controller: a stack pin to w1-twin is refused with its
	// move, one plan per env, and nothing is written
	err := n.orch.SetPin(ctx, adm, ScopeStack, n.stack, "w1-twin")
	var need *MoveNeeded
	if !errors.As(err, &need) || len(need.Plans) != 1 || need.Plans[0].Placement != "acme/shop/dev" || !strings.Contains(err.Error(), "acme/shop/dev") {
		t.Fatalf("stack pin over an env on another node = %v", err)
	}
	if st, _ := n.st.Stacks.Get(ctx, n.stack); st.PlacementID != nil {
		t.Fatal("a refused stack pin was written")
	}
	if ps, err := n.orch.PlanPin(ctx, adm, ScopeStack, n.stack, "w1-twin"); err != nil || len(ps) != 1 {
		t.Errorf("PlanPin = %+v, %v", ps, err)
	}
	if _, err := n.orch.PinScope(ctx, adm, ScopeStack, n.stack, "w1-twin", "wrong"); !errors.As(err, &need) {
		t.Errorf("wrong confirm = %v", err)
	}
	// the stack's slug confirms: dev's move is queued, edge (on w1) just follows
	js, err := n.orch.PinScope(ctx, adm, ScopeStack, n.stack, "w1-twin", "shop")
	must(t, err)
	if len(js) != 1 {
		t.Fatalf("confirmed stack pin queued %d moves, want 1", len(js))
	}
	edge, _ := n.st.Environments.Get(ctx, n.edge.ID)
	dev, _ := n.st.Environments.Get(ctx, n.dev.ID)
	if edge.PlacementID != twin.ID || edge.NodeID != "w1" {
		t.Errorf("edge in %q on %q", edge.PlacementID, edge.NodeID)
	}
	if dev.PlacementID != twin.ID {
		t.Errorf("dev in %q, want w1-twin once its move started", dev.PlacementID)
	}
	if st, _ := n.st.Stacks.Get(ctx, n.stack); st.PlacementID == nil || *st.PlacementID != twin.ID {
		t.Error("the confirmed stack pin was not written")
	}
	n.wait(t, js[0])

	// clearing the pin hands edge (on w1) to default on the controller: that
	// moves it too, so it waits for the confirm and nothing is written
	err = n.orch.SetPin(ctx, adm, ScopeStack, n.stack, "")
	if !errors.As(err, &need) || !slices.ContainsFunc(need.Plans, func(p PlacementMovePlan) bool { return p.Placement == "acme/shop/edge" }) {
		t.Fatalf("clearing a pin over an env on another node = %v", err)
	}
	if ps, err := n.orch.PlanPin(ctx, adm, ScopeStack, n.stack, ""); err != nil || len(ps) != len(need.Plans) {
		t.Errorf("PlanPin of a clear = %+v, %v", ps, err)
	}
	if st, _ := n.st.Stacks.Get(ctx, n.stack); st.PlacementID == nil {
		t.Fatal("a refused clear was written")
	}

	// env pin onto the other node: refused, nothing written
	err = n.orch.SetPin(ctx, adm, ScopeEnv, n.edge.ID, DefaultPlacement)
	if !errors.As(err, &need) || len(need.Plans) != 1 {
		t.Fatalf("env pin that moves = %v", err)
	}
	if got, _ := n.st.Environments.Get(ctx, n.edge.ID); got.PlacementID != twin.ID || got.NodeID != "w1" {
		t.Errorf("refused pin still wrote: %+v", got)
	}
	// same node: just a relabel
	must(t, n.orch.SetPin(ctx, adm, ScopeEnv, n.edge.ID, "w1"))
	if got, _ := n.st.Environments.Get(ctx, n.edge.ID); got.PlacementID != w1.ID {
		t.Errorf("edge in %q after a same-node pin", got.PlacementID)
	}
	// "" inherits what the stack gives
	must(t, n.orch.SetPin(ctx, adm, ScopeEnv, n.edge.ID, ""))
	if got, _ := n.st.Environments.Get(ctx, n.edge.ID); got.PlacementID != twin.ID {
		t.Errorf("inherited pin = %q, want the stack's", got.PlacementID)
	}
	// a placement with no node cannot take a running env
	addPlacement(t, n, "later", "", true)
	if err := n.orch.SetPin(ctx, adm, ScopeEnv, n.edge.ID, "later"); !isConflict(err) {
		t.Errorf("pin to a nodeless placement = %v", err)
	}
	if err := n.orch.SetPin(ctx, adm, "tile", "x", "w1"); !errors.As(err, new(errs.Invalid)) {
		t.Errorf("pin on a tile = %v", err)
	}
}

// An ingress pick that sends a tile's hosts to another proxy asks first;
// one that keeps them where they are, or a confirmed one, saves.
func TestPickIngressAsksWhenHostsMove(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	adm := adminP()
	n.placeW1(t, true)
	mustTag(t, n, adm, "w1", "ingress")
	must(t, n.st.Domains.Create(ctx, store.Domain{ID: "d1", TileID: n.devT.ID, Host: "shop.example.com", Path: "/", CreatedAt: time.Now()}))

	var hm *IngressHostsMove
	if err := n.orch.PickIngress(ctx, adm, ScopeEnv, n.dev.ID, "w1", false); !errors.As(err, &hm) || !slices.Equal(hm.Hosts, []string{"shop.example.com"}) || hm.To != "w1" {
		t.Fatalf("pick that moves a host = %v", err)
	}
	if pick, _ := n.orch.ingressPick(ctx, ScopeEnv, n.dev.ID); pick != "" {
		t.Fatal("a refused pick was written")
	}
	must(t, n.orch.PickIngress(ctx, adm, ScopeEnv, n.dev.ID, "w1", true))
	// edge has no hosts: its pick moves nothing and saves
	must(t, n.orch.PickIngress(ctx, adm, ScopeEnv, n.edge.ID, "w1", false))
	// the API path saves and names the moved hosts
	out, err := n.orch.IngressSet(ctx, adm, ScopeEnv, n.dev.ID, "")
	must(t, err)
	if !slices.Equal(out.Moved, []string{"shop.example.com"}) {
		t.Errorf("IngressSet moved = %v", out.Moved)
	}
}

// IngressFor: the nearest picked placement's node, then the env's own node
// when it is tagged ingress, then "" (the cluster default).
func TestIngressForOrder(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	adm := adminP()
	n.placeW1(t, true)
	// the controller leaves the ingress tag out of this scenario's way: w1 and
	// the controller both carry it, so only the order is under test
	ing := func(tl Tile) string {
		t.Helper()
		got, err := n.orch.IngressFor(ctx, tl.ID)
		must(t, err)
		return got
	}
	edgeT := n.edgeT

	// w1 is not tagged ingress: an env there uses the cluster default
	if got := ing(edgeT); got != "" {
		t.Errorf("edge on an untagged node = %q", got)
	}
	// the controller is tagged ingress: dev enters on its own node
	if got := ing(n.devT); got != n.ctl.ID {
		t.Errorf("dev on the controller = %q, want %q", got, n.ctl.ID)
	}
	mustTag(t, n, adm, "w1", "ingress", "egress")
	if got := ing(edgeT); got != "w1" {
		t.Errorf("edge on an ingress node = %q", got)
	}
	// an override beats the env's own node, and the nearest one wins
	must(t, n.orch.SetIngress(ctx, adm, ScopeStack, n.stack, "w1"))
	if got := ing(n.devT); got != "w1" {
		t.Errorf("stack pick = %q", got)
	}
	must(t, n.orch.SetIngress(ctx, adm, ScopeEnv, n.dev.ID, DefaultPlacement))
	if got := ing(n.devT); got != n.ctl.ID {
		t.Errorf("env pick = %q", got)
	}
	must(t, n.orch.SetIngress(ctx, adm, ScopeTile, n.devT.ID, "w1"))
	if got := ing(n.devT); got != "w1" {
		t.Errorf("tile pick = %q", got)
	}
	// a pick on a placement with no node is skipped
	addPlacement(t, n, "later", "", true)
	must(t, n.orch.SetIngress(ctx, adm, ScopeTile, n.devT.ID, "later"))
	if got := ing(n.devT); got != n.ctl.ID && got != "w1" {
		t.Errorf("nodeless pick = %q", got)
	}
	must(t, n.orch.SetIngress(ctx, adm, ScopeTile, n.devT.ID, ""))
	must(t, n.orch.SetIngress(ctx, adm, ScopeEnv, n.dev.ID, ""))
	must(t, n.orch.SetIngress(ctx, adm, ScopeStack, n.stack, ""))
	if got := ing(n.devT); got != n.ctl.ID {
		t.Errorf("all picks cleared = %q", got)
	}
}

// What a member is shown of a placement never holds the node's name. The
// placement here is named differently from its node so a scan can tell.
func TestMemberViewHoldsNoNodeName(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	adm := adminP()
	acme := acmeID(t, n)
	p := addPlacement(t, n, "eu-prod", "w1", true)
	must(t, n.st.Environments.SetPlacement(ctx, n.edge.ID, p.ID))
	mustTag(t, n, adm, "w1", "ingress", "egress")
	_, err := n.orch.PinScope(ctx, adm, ScopeOrg, acme, "eu-prod", "", "dev") // dev stays on the controller
	must(t, err)
	must(t, n.orch.SetIngress(ctx, adm, ScopeStack, n.stack, "eu-prod"))
	must(t, n.orch.SetExits(ctx, adm, ScopeOrg, acme, []string{"eu-prod", DefaultPlacement}))
	edgeT := n.edgeT

	var all []any
	for _, sc := range []struct{ kind, id string }{
		{ScopeOrg, acme}, {ScopeStack, n.stack}, {ScopeEnv, n.edge.ID}, {ScopeEnv, n.dev.ID}, {ScopeTile, edgeT.ID},
	} {
		v, err := n.orch.Placement(ctx, sc.kind, sc.id)
		must(t, err)
		all = append(all, v)
		pin, err := n.orch.PinOf(ctx, sc.kind, sc.id)
		must(t, err)
		ing, err := n.orch.IngressGet(ctx, sc.kind, sc.id)
		must(t, err)
		ex, err := n.orch.ExitGet(ctx, sc.kind, sc.id)
		must(t, err)
		all = append(all, pin, ing, ex)
	}
	offers, err := n.orch.OrgPlacements(ctx, acme)
	must(t, err)
	all = append(all, offers)
	// the banner for an env on a node that went down
	w, _ := n.st.Nodes.Get(ctx, "w1")
	w.Status = "offline"
	must(t, n.st.Nodes.Update(ctx, w))
	down, ok := n.orch.EnvNodeDown(ctx, n.edge.ID)
	if !ok || down.Placement != "eu-prod" {
		t.Fatalf("node down banner = %+v, %v", down, ok)
	}
	all = append(all, down.Placement)

	for _, v := range all {
		b, err := json.Marshal(v)
		must(t, err)
		if strings.Contains(string(b), "w1") {
			t.Errorf("%T holds a node name: %s", v, b)
		}
	}
	v, _ := n.orch.Placement(ctx, ScopeEnv, n.edge.ID)
	if v.Pin != "eu-prod" || v.PinFrom != "From org: eu-prod." {
		t.Errorf("env view pin %q from %q", v.Pin, v.PinFrom)
	}
	if v.Ingress != "" || v.IngressName != "eu-prod" || v.IngressFrom != "From stack: eu-prod." {
		t.Errorf("env ingress %q / %q / %q", v.Ingress, v.IngressName, v.IngressFrom)
	}
	if len(v.Rungs) != 2 || v.Rungs[0].Name != "eu-prod" || !v.Inherited {
		t.Errorf("rungs = %+v inherited=%v", v.Rungs, v.Inherited)
	}
	var names []string
	for _, o := range v.Nodes {
		names = append(names, o.Name)
	}
	if strings.Join(names, ",") != "default,eu-prod" {
		t.Errorf("offered placements = %v", names)
	}
}

// An org pin covers only the stacks without a pin of their own, so a volume
// env in a pinned stack does not stop the org pinning to a node without
// storage (the org file plan skips those stacks too).
func TestOrgPinSkipsPinnedStacks(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	adm := adminP()
	addPlacement(t, n, "plain", "w1", true) // w1 has no tags
	must(t, n.st.Volumes.Create(ctx, store.Volume{ID: "v1", ScopeKind: "env", ScopeID: n.dev.ID, Slug: "data", Name: "dev-data", CreatedAt: time.Now()}))
	if err := n.orch.SetPin(ctx, adm, ScopeOrg, acmeID(t, n), "plain"); err == nil {
		t.Fatal("pinned the org over an unpinned volume env to a node without storage")
	}
	_, err := n.orch.PinScope(ctx, adm, ScopeStack, n.stack, DefaultPlacement, "", "edge") // edge, on w1, stays
	must(t, err)
	if err := n.orch.SetPin(ctx, adm, ScopeOrg, acmeID(t, n), "plain"); err != nil {
		t.Errorf("org pin with every stack pinned = %v", err)
	}
}

// An ingress pick moves routes between nodes' proxies, so it pushes the
// routing table at once (the rig's override waited for the next sync).
func TestSetIngressPushesRoutes(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	before := n.pushes.Load()
	must(t, n.orch.SetIngress(ctx, adminP(), ScopeStack, n.stack, DefaultPlacement))
	if n.pushes.Load() == before {
		t.Error("an ingress pick pushed no routes")
	}
}
