package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

func TestNodeCerts(t *testing.T) {
	ctx := context.Background()
	s := servicetest.Store(t)
	nc := store.NewNodeCerts(s.DB())

	a := store.NodeCert{Serial: "aa", NodeID: "n1", Role: "worker", NotAfter: t1, Issuer: "ff", CreatedAt: t0}
	b := store.NodeCert{Serial: "bb", NodeID: "n1", Role: "worker", NotAfter: t1, CreatedAt: t1}
	for _, c := range []store.NodeCert{a, b} {
		if err := nc.Create(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := errs.IsConflict(nc.Create(ctx, a)); !ok {
		t.Fatalf("duplicate serial not a conflict")
	}
	got, err := nc.Get(ctx, "aa")
	if err != nil || got.RevokedAt != nil || !got.NotAfter.Equal(a.NotAfter) || got.NodeID != "n1" || got.Role != "worker" || got.Issuer != "ff" || !got.CreatedAt.Equal(t0) {
		t.Fatalf("get: %+v %v", got, err)
	}
	if _, err := nc.Get(ctx, "zz"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if rev, _ := nc.IsRevoked(ctx, "aa"); rev {
		t.Fatal("fresh cert revoked")
	}
	if err := nc.Revoke(ctx, "aa", t1); err != nil {
		t.Fatal(err)
	}
	if rev, _ := nc.IsRevoked(ctx, "aa"); !rev {
		t.Fatal("not revoked")
	}
	if rev, _ := nc.IsRevoked(ctx, "unknown"); rev {
		t.Fatal("unknown serial revoked")
	}
	if err := nc.Revoke(ctx, "zz", t1); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("revoke missing: %v", err)
	}
	// RevokeNode skips the one already revoked.
	if n, err := nc.RevokeNode(ctx, "n1", t1.Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("revoke node: %d %v", n, err)
	}
	if live, err := nc.ListLive(ctx); err != nil || len(live) != 0 {
		t.Fatalf("live after revoking all: %+v %v", live, err)
	}
	list, err := nc.ListByNode(ctx, "n1")
	if err != nil || len(list) != 2 || list[0].Serial != "aa" || list[0].RevokedAt == nil || !list[0].RevokedAt.Equal(t1) {
		t.Fatalf("list: %+v %v", list, err)
	}
}
