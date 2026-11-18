package org

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/ui/uitest"
)

// The org tabs hold their rows flat in the card: people, invites, keys, the
// minted key, domains.
func TestOrgSectionsAreFlat(t *testing.T) {
	var b strings.Builder
	for _, comp := range []interface {
		Render(ctx context.Context, w io.Writer) error
	}{
		Members(MembersView{Base: "/o", Manage: true, Roles: []string{"member"},
			Members: []MemberRow{{UserID: "u1", Name: "A", Email: "a@b.c", Role: "member"}},
			Invites: []InviteRow{{ID: "i1", Email: "x@y.z", Role: "member"}}}),
		Keys(KeysView{Base: "/o", Minted: "tok", Roles: []string{"member"}, Keys: []KeyRow{{Name: "k1"}}}),
		Domains(DomainsView{Add: "/o/d", Example: "x.dev", Rows: []DomainRow{{Host: "h.dev", Level: "org"}}}),
	} {
		if err := comp.Render(t.Context(), &b); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []string{"a@b.c", "x@y.z", "k1", "h.dev"} {
		if !strings.Contains(b.String(), want) {
			t.Fatalf("no row for %s", want)
		}
	}
	uitest.NoPanelInCard(t, b.String())
}
