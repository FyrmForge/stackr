package authz

import (
	"errors"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/errs"
)

func TestCan(t *testing.T) {
	var (
		admin = User{
			ID:     "a",
			Admin:  true,
			Active: true,
		}
		owner = User{
			ID:     "o",
			Active: true,
			Roles:  map[string]string{"o1": "owner"},
		}
		outsider = User{
			ID:     "n",
			Active: true,
			Roles:  map[string]string{"o2": "owner"},
		}
		disabled = User{
			ID:     "d",
			Active: false,
			Roles:  map[string]string{"o1": "owner"},
		}
		demoted = User{
			ID:     "o",
			Active: true,
			Roles:  map[string]string{},
			Key:    true,
			KeyOrg: "o1",
		}
		ownerKey = User{
			ID:     "o",
			Active: true,
			Roles:  map[string]string{"o1": "owner"},
			Key:    true,
			KeyOrg: "o1",
		}
		otherKey = User{
			ID:     "o",
			Active: true,
			Roles:  map[string]string{"o1": "owner", "o2": "owner"},
			Key:    true,
			KeyOrg: "o2",
		}
		unbound = User{
			ID:     "o",
			Active: true,
			Roles:  map[string]string{"o1": "owner"},
			Key:    true,
		}
		adminKey = User{
			ID:     "a",
			Admin:  true,
			Active: true,
			Key:    true,
		}
		anonymous = User{}
	)
	o1 := Resource{OrgID: "o1"}
	tests := []struct {
		name string
		u    User
		v    Verb
		want error
	}{
		{"admin reads", admin, "org.read", nil},
		{"admin writes", admin, "tile.write", nil},
		{"admin admins", admin, "user.admin", nil},
		{"admin unknown verb", admin, "made.up", nil},

		{"owner reads", owner, "org.read", nil},
		{"owner writes", owner, "tile.write", nil},
		{"owner owns", owner, "member.manage", nil},
		{"owner is not admin", owner, "user.admin", errs.ErrRefused},
		{"unknown verb fails closed", owner, "made.up", errs.ErrRefused},

		{"non-member reads", outsider, "org.read", errs.ErrNotFound},
		{"non-member writes", outsider, "tile.write", errs.ErrNotFound},
		{"non-member admins", outsider, "user.admin", errs.ErrRefused},

		{"disabled reads", disabled, "org.read", errs.ErrRefused},
		{"disabled writes", disabled, "tile.write", errs.ErrRefused},

		{"demoted key reads", demoted, "org.read", errs.ErrNotFound},
		{"demoted key writes", demoted, "tile.write", errs.ErrNotFound},
		{"demoted key owns", demoted, "member.manage", errs.ErrNotFound},

		{"key in its org", ownerKey, "tile.write", nil},
		{"key does not travel", otherKey, "org.read", errs.ErrNotFound},
		{"unbound key, non-admin, own org reads", unbound, "org.read", nil},
		{"unbound key, non-admin, own org owns", unbound, "member.manage", nil},
		{"unbound key, non-admin, admin verb", unbound, "user.admin", errs.ErrRefused},
		{"unbound key, admin", adminKey, "user.admin", nil},
		{"org-bound key, admin, admin verb", func() User { u := adminKey; u.KeyOrg = "o1"; return u }(), "user.admin", errs.ErrRefused},
		{"anonymous", anonymous, "org.read", errs.ErrRefused},
	}
	for _, tt := range tests {
		if got := Can(tt.u, tt.v, o1); !errors.Is(got, tt.want) || (tt.want == nil && got != nil) {
			t.Errorf("%s: Can = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// A stackr login key is the account's: it reaches every org its user is in,
// no other, and /me for anyone active.
func TestUnboundKey(t *testing.T) {
	u := User{ID: "u", Active: true, Roles: map[string]string{"o1": "viewer", "o2": "member"}, Key: true}
	if err := Self(u); err != nil {
		t.Errorf("Self = %v", err)
	}
	if err := Can(u, "tile.write", Resource{OrgID: "o2"}); err != nil {
		t.Errorf("member org write = %v", err)
	}
	if err := Can(u, "tile.write", Resource{OrgID: "o1"}); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("viewer org write = %v, want refused", err)
	}
	if err := Can(u, "org.read", Resource{OrgID: "o3"}); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("other org = %v, want not found", err)
	}
}

func TestCanKeyScope(t *testing.T) {
	key := func(role string, capped bool, ceiling Level, stack string) User {
		return User{ID: "u", Active: true, Roles: map[string]string{"o1": role}, Key: true, KeyOrg: "o1",
			KeyCapped: capped, KeyLevel: ceiling, KeyStack: stack}
	}
	org, shop, web := Resource{OrgID: "o1"}, Resource{OrgID: "o1", StackID: "s1"}, Resource{OrgID: "o1", StackID: "s2"}
	adminKey := key("", true, LevelOwner, "")
	adminKey.Admin = true
	uncapped := adminKey
	uncapped.KeyCapped = false
	tests := []struct {
		name string
		u    User
		v    Verb
		r    Resource
		want error
	}{
		{"viewer ceiling reads", key("owner", true, LevelRead, ""), "org.read", org, nil},
		{"viewer ceiling cannot deploy", key("owner", true, LevelRead, ""), "tile.write", shop, errs.ErrRefused},
		{"member ceiling deploys", key("owner", true, LevelWrite, ""), "tile.write", shop, nil},
		{"member ceiling cannot own", key("owner", true, LevelWrite, ""), "member.manage", org, errs.ErrRefused},
		{"owner ceiling, role demoted to viewer", key("viewer", true, LevelOwner, ""), "tile.write", shop, errs.ErrRefused},
		{"owner ceiling, role demoted to member", key("member", true, LevelOwner, ""), "member.manage", org, errs.ErrRefused},
		{"no ceiling is the role", key("member", false, 0, ""), "tile.write", shop, nil},
		{"stack key on its stack", key("owner", true, LevelWrite, "s1"), "tile.write", shop, nil},
		{"stack key on another stack", key("owner", true, LevelWrite, "s1"), "tile.write", web, errs.ErrNotFound},
		{"stack key on an org verb", key("owner", true, LevelOwner, "s1"), "member.manage", org, errs.ErrRefused},
		{"stack key on an org read", key("owner", true, LevelRead, "s1"), "org.read", org, errs.ErrRefused},
		{"admin user, owner ceiling, admin verb", adminKey, "user.admin", org, errs.ErrRefused},
		{"admin user, owner ceiling, owner verb", adminKey, "member.manage", org, nil},
		{"admin user, owner ceiling, above it", func() User { u := adminKey; u.KeyLevel = LevelRead; return u }(), "tile.write", shop, errs.ErrRefused},
		{"admin user, org-bound key, no ceiling, admin verb", uncapped, "user.admin", org, errs.ErrRefused},
		{"admin user, unbound key, admin verb", func() User { u := uncapped; u.KeyOrg = ""; return u }(), "user.admin", org, nil},
	}
	for _, tt := range tests {
		if got := Can(tt.u, tt.v, tt.r); !errors.Is(got, tt.want) || (tt.want == nil && got != nil) {
			t.Errorf("%s: Can = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestStandingChanged(t *testing.T) {
	tests := []struct {
		name, org, from, to string
		close               bool
		scope               string
	}{
		{"owner removed", "o1", "owner", "", true, "o1"},
		{"owner to viewer", "o1", "owner", "viewer", true, "o1"},
		{"owner to member keeps write", "o1", "owner", "member", false, ""},
		{"viewer removed had nothing", "o1", "viewer", "", false, ""},
		{"disabled closes everything", "", "", "", true, ""},
	}
	for _, tt := range tests {
		c, s := StandingChanged(tt.org, tt.from, tt.to)
		if c != tt.close || s != tt.scope {
			t.Errorf("%s: = %v %q, want %v %q", tt.name, c, s, tt.close, tt.scope)
		}
	}
}

// Deleting an env's logs is for owners and admins; a member is refused.
func TestCanPurgeLogs(t *testing.T) {
	r := Resource{OrgID: "o1"}
	for role, want := range map[string]bool{"viewer": false, "member": false, "owner": true} {
		u := User{ID: "u", Active: true, Roles: map[string]string{"o1": role}}
		if got := Can(u, "env.logs.purge", r) == nil; got != want {
			t.Errorf("%s: allowed=%v, want %v", role, got, want)
		}
	}
	if err := Can(User{ID: "a", Active: true, Admin: true}, "env.logs.purge", r); err != nil {
		t.Errorf("admin: %v", err)
	}
}
