package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/FyrmForge/hamr/pkg/auth"

	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// A session row holds the hash of its token, never the token; the token
// still finds it.
func TestSessionTokenStoredHashed(t *testing.T) {
	st := servicetest.Store(t)
	ctx := context.Background()
	se := &auth.Session{ID: "s1", Token: "plain-token-value", ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now()}
	if err := st.Sessions.Create(ctx, se); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := st.DB().QueryRowContext(ctx, `SELECT token FROM sessions WHERE id = 's1'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == se.Token || len(stored) != 64 {
		t.Fatalf("stored token = %q", stored)
	}
	got, err := st.Sessions.GetByToken(ctx, se.Token)
	if err != nil || got == nil || got.ID != "s1" || got.Token != se.Token {
		t.Fatalf("lookup = %+v, %v", got, err)
	}
	if got, _ := st.Sessions.GetByToken(ctx, stored); got != nil {
		t.Fatal("the stored hash works as a token")
	}
}
