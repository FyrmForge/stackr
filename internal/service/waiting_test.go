package service_test

import (
	"context"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// Waiting on you: an owner sees the value a parked deploy waits for; access
// parked for approval is an admin's group and the owner's muted line.
func TestWaitingOnYou(t *testing.T) {
	env := servicetest.New(t)
	ctx := context.Background()
	org := env.Org(t, "acme")
	owner := env.User(t, "owner@acme.test", false)
	env.Member(t, org, owner, "owner")
	admin := env.User(t, "admin@acme.test", true)
	tl := env.Tile(t, org)

	pOwner, err := env.Orch.SessionPrincipal(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	pAdmin, _ := env.Orch.SessionPrincipal(ctx, admin)
	if w, err := env.Orch.WaitingOnYou(ctx, pOwner); err != nil || w.Count() != 0 || len(w.Others) != 0 {
		t.Fatalf("nothing waits: %+v, %v", w, err)
	}

	env.WaitingJob(t, "gate.password", `{"tile_id":"`+tl.ID+`"}`)
	env.WaitingJob(t, "elevated access: share media", `{"tile_id":"`+tl.ID+`","host_access":{"stack":"`+tl.Stack+`","what":"share media"}}`)

	w, err := env.Orch.WaitingOnYou(ctx, pOwner)
	if err != nil || w.Count() != 1 || w.Groups[0].Title != "Values to set" || w.Groups[0].Items[0].Act != "Set" {
		t.Fatalf("owner = %+v, %v", w, err)
	}
	if len(w.Others) != 1 || w.Others[0].Sub != "1 job waits for a server admin" {
		t.Errorf("the owner's muted line = %+v", w.Others)
	}
	if wa, _ := env.Orch.WaitingOnYou(ctx, pAdmin); len(wa.Groups) == 0 || wa.Groups[len(wa.Groups)-1].Title != "Access to approve" || len(wa.Others) != 0 {
		t.Errorf("admin = %+v", wa)
	}
}

// A wait on an org value links to the org's variables (the env's tab cannot
// set it), and to an owner only: an env writer gets the muted line.
func TestWaitingOrgParam(t *testing.T) {
	env := servicetest.New(t)
	ctx := context.Background()
	org := env.Org(t, "acme")
	owner := env.User(t, "owner@acme.test", false)
	env.Member(t, org, owner, "owner")
	tl := env.Tile(t, org)
	env.WaitingJob(t, "org.gate.password", `{"tile_id":"`+tl.ID+`"}`)

	p, err := env.Orch.SessionPrincipal(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	w, err := env.Orch.WaitingOnYou(ctx, p)
	if err != nil || w.Count() != 1 {
		t.Fatalf("owner = %+v, %v", w, err)
	}
	it := w.Groups[0].Items[0]
	if !strings.Contains(it.Href, "?drawer=org%3A") || !strings.HasSuffix(it.Href, "&tab=variables") || strings.Contains(it.Href, "/shop/") {
		t.Errorf("link = %q, want the org's variables", it.Href)
	}
}
