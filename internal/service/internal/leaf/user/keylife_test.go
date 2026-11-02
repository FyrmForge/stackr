package user_test

import (
	"errors"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/user"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// A key can expire, says when it was last used, and rotates in place.
func TestKeyLifecycle(t *testing.T) {
	st := servicetest.Store(t)
	l := user.New(st.Users, st.Sessions, st.APIKeys)
	u, other := seed(t, st, "u@x.io", false), seed(t, st, "o@x.io", false)

	past := time.Now().Add(-time.Hour)
	if _, _, err := l.MintKeyUntil(ctx, u, "", "", "k", "", "", &past); err == nil {
		t.Error("an expiry in the past was accepted")
	}
	soon := time.Now().Add(time.Hour)
	tok, k, err := l.MintKeyUntil(ctx, u, "", "", "k", "", "", &soon)
	if err != nil || k.ExpiresAt == nil {
		t.Fatalf("mint = %+v, %v", k, err)
	}
	if _, got, err := l.ByKey(ctx, tok); err != nil || got.ID != k.ID {
		t.Fatalf("ByKey = %+v, %v", got, err)
	}
	if got, _ := st.APIKeys.Get(ctx, k.ID); got.LastUsedAt == nil {
		t.Error("a request left no last_used_at")
	}

	newTok, rk, err := l.RotateKey(ctx, u.ID, k.ID)
	if err != nil || newTok == tok || rk.ID != k.ID {
		t.Fatalf("rotate = %q %+v, %v", newTok, rk, err)
	}
	if _, _, err := l.ByKey(ctx, tok); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("the old secret still works: %v", err)
	}
	if _, _, err := l.ByKey(ctx, newTok); err != nil {
		t.Errorf("the new secret: %v", err)
	}
	if _, _, err := l.RotateKey(ctx, other.ID, k.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("rotating someone else's key = %v", err)
	}

	// expiry: the stored time passes, the key is as unknown as a wrong one
	rk.ExpiresAt = &past
	if err := st.APIKeys.Update(ctx, rk); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.ByKey(ctx, newTok); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("an expired key opened a request: %v", err)
	}
}
