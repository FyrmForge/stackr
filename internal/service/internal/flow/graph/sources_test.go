package graph

import (
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// A source line follows what a clone resolves to: a named connector, else
// the org's. A second connector on the same host draws none.
func TestBuildsVia(t *testing.T) {
	org := "o1"
	own := store.Connector{ID: "own", Host: "github.com", OrgID: &org}
	s1 := store.Connector{ID: "s1", Host: "github.com"}
	s2 := store.Connector{ID: "s2", Host: "github.com"}
	live := []store.Connector{own, s1, s2}
	built := []store.Tile{{GitURL: "https://github.com/a/b"}}
	images := []store.Tile{{}}
	repo := "https://github.com/a/stack"
	via := func(ts []store.Tile, og store.Org, st store.Stack) (got []string) {
		for _, c := range live {
			if buildsVia(ts, live, og, st, c.ID) {
				got = append(got, c.ID)
			}
		}
		return got
	}
	for _, c := range []struct {
		name string
		ts   []store.Tile
		og   store.Org
		st   store.Stack
		want string
	}{
		{"inherited org connector", nil, store.Org{ConfigConnectorID: "s1"}, store.Stack{ConfigRepo: repo}, "s1"},
		{"no line from a second connector on the host", built, store.Org{ConfigConnectorID: "s1"}, store.Stack{ConfigRepo: repo}, "s1"},
		{"tile follows the stack's connector", built, store.Org{ConfigConnectorID: "own"}, store.Stack{ConfigConnectorID: "s2"}, "s2"},
		{"named wins over the org", nil, store.Org{ConfigConnectorID: "own"}, store.Stack{ConfigRepo: repo, ConfigConnectorID: "s2"}, "s2"},
		{"nothing to inherit draws none", built, store.Org{}, store.Stack{ConfigRepo: repo}, ""},
		{"images only, no repo draw none", images, store.Org{ConfigConnectorID: "own"}, store.Stack{}, ""},
	} {
		got := via(c.ts, c.og, c.st)
		if (c.want == "") != (len(got) == 0) || (c.want != "" && (len(got) != 1 || got[0] != c.want)) {
			t.Errorf("%s: %v, want %q", c.name, got, c.want)
		}
	}
}
