package service_test

import (
	"context"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// Enable flips the flag back and nothing else: what disable closed stays closed.
func TestEnableUserRoundTrip(t *testing.T) {
	e := servicetest.New(t)
	ctx := context.Background()
	u := e.User(t, "u@x.test", false)
	e.APIKey(t, u, "")
	if err := e.Orch.DisableUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	active := func() bool {
		us, err := e.Orch.Users(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range us {
			if x.ID == u {
				return x.Active
			}
		}
		t.Fatal("user gone")
		return false
	}
	if active() {
		t.Fatal("still active after disable")
	}
	if err := e.Orch.EnableUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if !active() {
		t.Fatal("not active after enable")
	}
	keys, err := e.Store.APIKeys.ListByUser(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Errorf("keys = %d after disable+enable, want 0 (stay revoked)", len(keys))
	}
}
