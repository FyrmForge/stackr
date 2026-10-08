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
	line := hostgrant.Line
	want := hostgrant.Set{Lines: []string{line("w", "host:/b:/b"), line("w", "host:/a:/a:ro"), line("w", "device:/dev/x"), line("w", "host:/a:/a:ro"), line("w", "privileged")}}.Norm()
	if !slices.Equal(want.Lines, []string{"w device:/dev/x", "w host:/a:/a:ro", "w host:/b:/b", "w privileged"}) {
		t.Fatalf("norm = %v", want.Lines)
	}
	have := hostgrant.Set{Lines: []string{line("w", "host:/a:/a:ro")}}
	m := want.Missing(have)
	if !slices.Equal(m, []string{"w device:/dev/x", "w host:/b:/b", "w privileged"}) {
		t.Fatalf("missing = %v", m)
	}
	back, ok := hostgrant.Parse(hostgrant.Text(m))
	if !ok || !slices.Equal(back.Lines, m) {
		t.Fatalf("parse = %+v, %v", back, ok)
	}
	if _, ok := hostgrant.Parse("param x is not set"); ok {
		t.Fatal("parsed a foreign text")
	}
	// Dropping a line is not a difference that needs approval.
	if m := (hostgrant.Set{}).Missing(have); len(m) != 0 {
		t.Fatalf("subset asks for %v", m)
	}
	// Another tile's identical perm is not covered.
	if m := (hostgrant.Set{Lines: []string{line("b", "host:/a:/a:ro")}}).Missing(have); len(m) != 1 {
		t.Fatalf("tile b covered by tile a's line: %v", m)
	}
	sl, perm := hostgrant.Split(line("w", "lan:10.0.0.0/24:80"))
	if sl != "w" || perm != "lan:10.0.0.0/24:80" {
		t.Fatalf("split = %q %q", sl, perm)
	}
	if got := want.ForTile("w"); len(got.Lines) != 4 || len(want.ForTile("x").Lines) != 0 {
		t.Fatalf("for tile = %v", got.Lines)
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

	want := hostgrant.Set{Lines: []string{"w host:/var/run/docker.sock:/s"}}
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
	g, err := l.Approve(ctx, "s1", "u1", hostgrant.Set{Lines: []string{"x privileged"}})
	must(err)
	if g.Lines != "w host:/var/run/docker.sock:/s\nx privileged" {
		t.Fatalf("union = %+v", g)
	}
	// Revoking one tile leaves the other's lines.
	must(l.Revoke(ctx, "s1", "w"))
	if row, err := l.Row(ctx, "s1"); err != nil || row.Lines != "x privileged" {
		t.Fatalf("row after tile revoke = %+v, %v", row, err)
	}
	must(l.Revoke(ctx, "s1", "x"))
	if _, err := l.Row(ctx, "s1"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("row after last tile revoke = %v", err)
	}
	_, err = l.Approve(ctx, "s1", "u1", want)
	must(err)
	must(l.Revoke(ctx, "s1", ""))
	if _, err := l.Row(ctx, "s1"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("row after revoke = %v", err)
	}
	if _, ok := errs.IsNeedsApproval(l.Check(ctx, "s1", want)); !ok {
		t.Fatal("a revoked grant still passes")
	}
}
