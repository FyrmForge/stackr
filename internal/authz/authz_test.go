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
		{"unbound key, non-admin", unbound, "org.read", errs.ErrRefused},
		{"unbound key, admin", adminKey, "user.admin", nil},
		{"anonymous", anonymous, "org.read", errs.ErrRefused},
	}
	for _, tt := range tests {
		if got := Can(tt.u, tt.v, o1); !errors.Is(got, tt.want) || (tt.want == nil && got != nil) {
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
