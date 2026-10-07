package hostgrant_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/hostgrant"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

var ctx = context.Background()

func TestSetText(t *testing.T) {
	want := hostgrant.Set{Lines: []string{"host:/b:/b", "host:/a:/a:ro", "device:/dev/x", "host:/a:/a:ro"}, Privileged: true}.Norm()
	if !slices.Equal(want.Lines, []string{"device:/dev/x", "host:/a:/a:ro", "host:/b:/b"}) {
		t.Fatalf("norm = %v", want.Lines)
	}
	have := hostgrant.Set{Lines: []string{"host:/a:/a:ro"}}
	m := want.Missing(have)
	if !slices.Equal(m, []string{"device:/dev/x", "host:/b:/b", "privileged"}) {
		t.Fatalf("missing = %v", m)
	}
	back, ok := hostgrant.Parse(hostgrant.Text(m))
	if !ok || !back.Privileged || !slices.Equal(back.Lines, []string{"device:/dev/x", "host:/b:/b"}) {
		t.Fatalf("parse = %+v, %v", back, ok)
	}
	if _, ok := hostgrant.Parse("param x is not set"); ok {
		t.Fatal("parsed a foreign text")
	}
	// Dropping a line is not a difference that needs approval.
	if m := (hostgrant.Set{}).Missing(have); len(m) != 0 {
		t.Fatalf("subset asks for %v", m)
	}
}

func TestApproveCheckRevoke(t *testing.T) {
	st := servicetest.Store(t)
	now := time.Now().UTC()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(st.Orgs.Create(ctx, store.Org{ID: "o1", Name: "A", Slug: "a", EnvColors: "{}", Settings: "{}", CreatedAt: now}))
	must(st.Stacks.Create(ctx, store.Stack{ID: "s1", OrgID: "o1", Name: "S", Slug: "s", Settings: "{}", CreatedAt: now}))
	must(st.Users.Create(ctx, store.User{ID: "u1", Email: "a@b.c", Name: "A", Role: "admin", Active: true, CreatedAt: now, UpdatedAt: now}))
	l := hostgrant.New(st.HostGrants)

	want := hostgrant.Set{Lines: []string{"host:/var/run/docker.sock:/s"}}
	err := l.Check(ctx, "s1", want)
	if n, ok := errs.IsNeedsApproval(err); !ok || n.Stack != "s1" {
		t.Fatalf("check without a row = %v", err)
	}
	if err := l.Check(ctx, "s1", hostgrant.Set{}); err != nil {
		t.Fatalf("nothing asked: %v", err)
	}
	_, err = l.Approve(ctx, "s1", "u1", want)
	must(err)
	must(l.Check(ctx, "s1", want))
	// A second approval adds, it never drops what another env was granted.
	g, err := l.Approve(ctx, "s1", "u1", hostgrant.Set{Privileged: true})
	must(err)
	if g.Lines != "host:/var/run/docker.sock:/s" || !g.Privileged {
		t.Fatalf("union = %+v", g)
	}
	must(l.Revoke(ctx, "s1"))
	if _, err := l.Row(ctx, "s1"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("row after revoke = %v", err)
	}
	if _, ok := errs.IsNeedsApproval(l.Check(ctx, "s1", want)); !ok {
		t.Fatal("a revoked grant still passes")
	}
}
