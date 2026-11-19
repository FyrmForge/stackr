package graph

import (
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

func TestBuildsVia(t *testing.T) {
	org := "o1"
	own := store.Connector{ID: "own", Host: "github.com", OrgID: &org}
	s1 := store.Connector{ID: "s1", Host: "github.com"}
	s2 := store.Connector{ID: "s2", Host: "github.com"}
	built := []store.Tile{{GitURL: "https://github.com/a/b"}}
	images := []store.Tile{{}}
	via := func(ts []store.Tile, live []store.Connector, cfg string) (got []string) {
		for _, c := range live {
			if buildsVia(ts, live, cfg, c.ID) {
				got = append(got, c.ID)
			}
		}
		return got
	}
	for _, c := range []struct {
		name string
		ts   []store.Tile
		live []store.Connector
		cfg  string
		want string
	}{
		{"own wins over two shared", built, []store.Connector{own, s1, s2}, "", "own"},
		{"config breaks a shared tie", built, []store.Connector{s1, s2}, "s2", "s2"},
		{"ambiguous draws none", built, []store.Connector{s1, s2}, "", ""},
		{"config on another host does not break it", built, []store.Connector{s1, s2, {ID: "gl", Host: "gitlab.com"}}, "gl", ""},
		{"images only draw none", images, []store.Connector{own, s1, s2}, "", ""},
	} {
		got := via(c.ts, c.live, c.cfg)
		if (c.want == "") != (len(got) == 0) || (c.want != "" && (len(got) != 1 || got[0] != c.want)) {
			t.Errorf("%s: %v, want %q", c.name, got, c.want)
		}
	}
}
