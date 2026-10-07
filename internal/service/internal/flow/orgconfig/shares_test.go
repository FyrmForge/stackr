package orgconfig_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/flow/orgconfig"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

const mediaShare = `shares:
  media:
    kind: nfs
    source: nas:/export
`

func withShares(l *orgconfig.Live) {
	l.Shares = []store.Share{{ID: "sh1", OrgID: "o1", Slug: "media", Kind: "nfs", Source: "nas:/export"}}
}

// shares: adds, updates field by field and, only when the block is present,
// deletes; a mounted share cannot go.
func TestDiffShares(t *testing.T) {
	for _, c := range []struct {
		name    string
		file    string
		live    func(*orgconfig.Live)
		changes []orgconfig.Change
		blocker string
	}{
		{name: "no block says nothing", file: v1, live: withShares},
		{
			name: "add",
			file: v1 + mediaShare,
			changes: []orgconfig.Change{
				{Kind: "share", Tile: "media", New: "nas:/export", Note: "nfs"},
			},
		},
		{name: "same", file: v1 + mediaShare, live: withShares},
		{
			name: "update",
			file: v1 + mediaShare + "    options: nfsvers=4\n",
			live: withShares,
			changes: []orgconfig.Change{
				{Kind: "share-update", Tile: "media", Field: "options", New: "nfsvers=4"},
			},
		},
		{
			name: "delete",
			file: v1 + "shares: {}\n",
			live: withShares,
			changes: []orgconfig.Change{
				{Kind: "share-delete", Tile: "media", Old: "nas:/export"},
			},
		},
		{
			name: "delete while mounted",
			file: v1 + "shares: {}\n",
			live: func(l *orgconfig.Live) {
				withShares(l)
				l.ShareUsers = map[string][]string{"media": {"web"}}
			},
			blocker: "web still mount it",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, err := orgconfig.Parse([]byte(c.file))
			if err != nil {
				t.Fatal(err)
			}
			l := live()
			if c.live != nil {
				c.live(&l)
			}
			p := orgconfig.Diff(f, l)
			if !slices.Equal(p.Changes, c.changes) {
				t.Errorf("changes = %+v\nwant %+v", p.Changes, c.changes)
			}
			one(t, "blockers", p.Blockers, c.blocker)
		})
	}
}

// A bad share is a parse error naming it; export round-trips clean.
func TestSharesParseAndExport(t *testing.T) {
	_, err := orgconfig.Parse([]byte(v1 + "shares:\n  Bad:\n    kind: nfs\n    source: nas:/e\n"))
	if err == nil || !strings.Contains(err.Error(), "shares.Bad") {
		t.Errorf("err = %v", err)
	}
	_, err = orgconfig.Parse([]byte(v1 + "shares:\n  a:\n    kind: smb\n    source: //nas/e\n    user: bob\n    password: hunter2\n"))
	if err == nil || !strings.Contains(err.Error(), "never the value") {
		t.Errorf("literal password err = %v", err)
	}

	l := live()
	withShares(&l)
	l.Shares = append(l.Shares, store.Share{
		Slug: "docs", Kind: "smb", Source: "//nas/docs", User: "bob", PasswordRef: "${{ org.params.nas.pw }}",
	})
	out, err := orgconfig.Export(l)
	if err != nil {
		t.Fatal(err)
	}
	f, err := orgconfig.Parse(out)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if p := orgconfig.Diff(f, l); len(p.Changes) != 0 || p.Blocked() {
		t.Errorf("export diffs against itself: %+v", p)
	}
}
