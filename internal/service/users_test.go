package service

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/user"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// A restore mints the admin a new key that authenticates as that admin; an
// install with no admin gets none.
func TestMintAdminKey(t *testing.T) {
	ctx := context.Background()
	cfg := Config{DataDir: t.TempDir(), SecretsKey: testKey}
	if _, _, _, err := MintAdminKey(ctx, cfg); err == nil {
		t.Fatal("minted a key with no admin")
	}
	admin, _, err := CreateAdmin(ctx, cfg, "admin@example.com", "correct horse battery", "Admin")
	must(t, err)
	u, key, _, err := MintAdminKey(ctx, cfg)
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

	// a second restore revokes the installer and first restore keys, keeps
	// other keys, and logs each revoke
	var other string
	must(t, withUsers(ctx, cfg, func(l *user.Leaf) error {
		_, k, err := l.MintKey(ctx, admin, "", "", "laptop", "", "")
		other = k.ID
		return err
	}))
	_, key2, _, err := MintAdminKey(ctx, cfg)
	must(t, err)
	must(t, withStore(ctx, cfg, func(st *store.Store) error {
		l := user.New(st.Users, st.Sessions, st.APIKeys)
		if _, _, err := l.ByKey(ctx, key); err == nil {
			t.Error("the first restore key still works")
		}
		if _, _, err := l.ByKey(ctx, key2); err != nil {
			t.Errorf("the new key: %v", err)
		}
		ks, err := l.Keys(ctx, admin.ID)
		if err != nil {
			return err
		}
		var names []string
		for _, k := range ks {
			names = append(names, k.Name)
			if k.Name == "laptop" && k.ID != other {
				t.Errorf("laptop key changed")
			}
		}
		slices.Sort(names)
		if !slices.Equal(names, []string{"laptop", "restore"}) {
			t.Errorf("keys left %v, want laptop and the new restore", names)
		}
		evs, err := securitylog.New(st.SecurityEvents).List(ctx, store.SecurityFilter{Action: securitylog.KeyRevoke})
		if err != nil {
			return err
		}
		if len(evs) != 2 {
			t.Errorf("%d key.revoke rows, want 2 (installer, restore)", len(evs))
		}
		return nil
	}))
}

// The restore's CLI starts on the admin's oldest org membership, else the
// oldest org, else none.
func TestMintAdminKeyOrg(t *testing.T) {
	ctx := context.Background()
	cfg := Config{DataDir: t.TempDir(), SecretsKey: testKey}
	admin, _, err := CreateAdmin(ctx, cfg, "admin@example.com", "correct horse battery", "Admin")
	must(t, err)
	if _, _, org, err := MintAdminKey(ctx, cfg); err != nil || org != "" {
		t.Fatalf("no org: %q %v", org, err)
	}
	now := time.Now()
	must(t, withStore(ctx, cfg, func(st *store.Store) error {
		for i, slug := range []string{"old", "dnsqa"} {
			if err := st.Orgs.Create(ctx, store.Org{ID: slug, Name: slug, Slug: slug, EnvColors: "{}", Settings: "{}", CreatedAt: now.Add(time.Duration(i) * time.Hour)}); err != nil {
				return err
			}
		}
		return nil
	}))
	if _, _, org, _ := MintAdminKey(ctx, cfg); org != "old" {
		t.Errorf("no membership: %q, want the oldest org", org)
	}
	must(t, withStore(ctx, cfg, func(st *store.Store) error {
		return st.OrgMembers.Create(ctx, store.OrgMember{ID: "m1", OrgID: "dnsqa", UserID: admin.ID, Role: "owner", CreatedAt: now})
	}))
	if _, _, org, _ := MintAdminKey(ctx, cfg); org != "dnsqa" {
		t.Errorf("member of dnsqa: %q", org)
	}
}
