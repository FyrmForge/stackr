package service

import (
	"context"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/user"
)

// A restore mints the admin a new key that authenticates as that admin; an
// install with no admin gets none.
func TestMintAdminKey(t *testing.T) {
	ctx := context.Background()
	cfg := Config{DataDir: t.TempDir(), SecretsKey: testKey}
	if _, _, err := MintAdminKey(ctx, cfg); err == nil {
		t.Fatal("minted a key with no admin")
	}
	admin, _, err := CreateAdmin(ctx, cfg, "admin@example.com", "correct horse battery", "Admin")
	must(t, err)
	u, key, err := MintAdminKey(ctx, cfg)
	must(t, err)
	if u.ID != admin.ID || key == "" {
		t.Fatalf("minted for %s (key %t), want %s", u.Email, key != "", admin.Email)
	}
	must(t, withUsers(ctx, cfg, func(l *user.Leaf) error {
		got, k, err := l.ByKey(ctx, key)
		if err == nil && (got.ID != admin.ID || k.OrgID != nil && *k.OrgID != "") {
			t.Errorf("key is %s's, org %v; want the admin's, no org", got.Email, k.OrgID)
		}
		return err
	}))
}
