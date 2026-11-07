package registry_test

import (
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/registry"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// An org's repositories hold its slug until purged; an emptied repository no
// longer holds it, and a purge stays inside the slug.
func TestHoldsAndPurgeOrg(t *testing.T) {
	r := startRegistry(t)
	st := servicetest.Store(t)
	l := registry.New(st.RegistryImages, "").WithBase(r.URL())
	r.push("acme/docker.io/library/app", "t1")
	r.push("other/app", "t1")

	if ok, err := l.Holds(ctx, "acme"); err != nil || !ok {
		t.Fatalf("Holds(acme) = %v, %v", ok, err)
	}
	if ok, _ := l.Holds(ctx, "acm"); ok {
		t.Error("a prefix of the slug holds")
	}
	if err := l.PurgeOrg(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if ok, err := l.Holds(ctx, "acme"); err != nil || ok {
		t.Errorf("Holds(acme) after purge = %v, %v", ok, err)
	}
	if ok, _ := l.Holds(ctx, "other"); !ok {
		t.Error("purge reached another org")
	}
}
