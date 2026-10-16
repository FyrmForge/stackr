package serverconfig_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/flow/serverconfig"
	"github.com/FyrmForge/stackr/internal/service/internal/planfile"
)

// withNodes is live() on a cluster of ctl (controller), h3 and h4, a
// default placement on ctl, eu-prod on h4 with two envs and eu-db on h3.
func withNodes(l *serverconfig.Live) {
	l.Nodes = []serverconfig.NodeLive{
		{ID: "n0", Name: "ctl", Online: true, Tags: []string{"ingress", "storage"}, Locked: map[string]string{"ingress": "it is the default ingress"}},
		{ID: "n3", Name: "h3", Online: true, Tags: []string{"egress", "storage"}},
		{ID: "n4", Name: "h4", Online: true, Tags: []string{"egress", "ingress"}},
		{ID: "n5", Name: "h5", Pending: true},
		{ID: "n6", Name: "h6", Forgotten: true},
	}
	l.Placements = []serverconfig.PlacementLive{
		{ID: "p0", Name: "default", Node: "ctl", OpenToAll: true},
		{ID: "p1", Name: "eu-prod", Node: "h4", OpenToAll: true, Used: 3},
		{ID: "p2", Name: "eu-db", Node: "h3", OpenToAll: false, Orgs: []string{"acme"}},
		{ID: "p3", Name: "idle", OpenToAll: true},
	}
	l.Moves = map[string][]planfile.Check{}
	l.MoveEnvs = map[string][]string{}
}

const sameNodes = "nodes:\n  ctl: {tags: [ingress, storage]}\n  h3: {tags: [egress, storage]}\n  h4: {tags: [egress, ingress]}\n"
const samePlacements = "placements:\n  default: {node: ctl}\n  eu-prod: {node: h4}\n  eu-db: {node: h3, open: [acme]}\n  idle: {}\n"

func gap(p serverconfig.Plan, kind, key string) (planfile.Gap, bool) {
	i := slices.IndexFunc(p.Gaps, func(g planfile.Gap) bool { return g.Kind == kind && g.Key == key })
	if i < 0 {
		return planfile.Gap{}, false
	}
	return p.Gaps[i], true
}

func TestParsePlacements(t *testing.T) {
	f := parse(t, "nodes:\n  h4: {tags: [storage, ingress, ingress]}\nplacements:\n  a: {node: h4}\n  b: {open: all}\n  c: {open: [globex, acme]}\n  d: {open: []}\n")
	if got := f.Nodes["h4"].Tags; !slices.Equal(got, []string{"ingress", "storage"}) {
		t.Errorf("tags = %q, want sorted and unique", got)
	}
	if f.Placements["a"].Open != nil || f.Placements["a"].Node != "h4" {
		t.Errorf("a = %+v", f.Placements["a"])
	}
	if o := f.Placements["b"].Open; o == nil || !o.All {
		t.Errorf("b open = %+v, want all", o)
	}
	if o := f.Placements["c"].Open; o == nil || !slices.Equal(o.Orgs, []string{"acme", "globex"}) {
		t.Errorf("c open = %+v, want sorted slugs", o)
	}
	if o := f.Placements["d"].Open; o == nil || o.All || len(o.Orgs) != 0 {
		t.Errorf("d open = %+v, want none", o)
	}
	for _, c := range []struct{ name, file, want string }{
		{"bad placement name", "placements:\n  Eu_Prod: {}\n", "not a placement name"},
		{"bad node name", "nodes:\n  H4: {}\n", "not a node name"},
		{"bad node in placement", "placements:\n  a: {node: H4}\n", "not a node name"},
		{"unknown tag", "nodes:\n  h4: {tags: [fast]}\n", "not a tag"},
		{"bad open", "placements:\n  a: {open: nobody}\n", "all, or a list"},
		{"bad org slug", "placements:\n  a: {open: [Bad Org]}\n", "not an org slug"},
		{"unknown key", "placements:\n  a: {pin: x}\n", "unknown key"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := serverconfig.Parse([]byte(v1 + c.file))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want %q", err, c.want)
			}
		})
	}
}

func TestDiffPlacements(t *testing.T) {
	for _, c := range []struct {
		name    string
		file    string
		mut     func(*serverconfig.Live)
		changes []string
		blocker string
		note    string
		gap     string // a node gap
	}{
		{name: "in sync", file: sameNodes + samePlacements},
		{name: "no blocks says nothing", file: ""},
		{
			name: "new placement on a joined node", file: samePlacements + "  fresh: {node: h4}\n",
			changes: []string{"placement:fresh"},
		},
		{
			name: "new placement waits for its node", file: samePlacements + "  fresh: {node: h9}\nnodes:\n  h9: {tags: [ingress]}\n",
			changes: []string{"placement:fresh"}, note: "fresh waits for node h9", gap: "h9",
		},
		{
			name: "pending node is not joined", file: samePlacements + "  fresh: {node: h5}\n",
			changes: []string{"placement:fresh"}, note: "fresh waits for node h5", gap: "h5",
		},
		{
			name: "forgotten node is not joined", file: samePlacements + "  fresh: {node: h6}\n",
			changes: []string{"placement:fresh"}, note: "fresh waits for node h6", gap: "h6",
		},
		{
			name: "declared node not joined", file: sameNodes + "  h9: {tags: [egress]}\n",
			note: "node h9 is not joined yet", gap: "h9",
		},
		{
			name: "unmanaged node", file: "nodes:\n  ctl: {tags: [ingress, storage]}\n",
			note: "h4 is not in the file (unmanaged)",
		},
		{
			name: "tags change", file: "nodes:\n  h3: {tags: [egress]}\n",
			changes: []string{"node-tags:tags"}, note: "h4 is not in the file",
		},
		{
			name: "dropping a locked tag blocks", file: "nodes:\n  ctl: {tags: [storage]}\n",
			changes: []string{"node-tags:tags"}, blocker: "cannot drop ingress: it is the default ingress",
		},
		{
			name: "open change", file: sameNodes + "placements:\n  default: {node: ctl}\n  eu-prod: {node: h4, open: [acme]}\n  eu-db: {node: h3, open: all}\n  idle: {}\n",
			changes: []string{"placement-open:open", "placement-open:open"},
		},
		{
			name: "open to an unknown org", file: samePlacements + "  fresh: {open: [nope]}\n",
			changes: []string{"placement:fresh"}, blocker: `no org "nope"`,
		},
		{
			name: "open to an org the file creates", file: samePlacements + "  fresh: {open: [newco]}\norgs:\n  - {slug: newco}\n  - {slug: acme, repo: https://github.com/acme/infra, branch: main, connector: gh-main, auto: true}\n",
			changes: []string{"org-create:newco", "placement:fresh"},
		},
		{
			name: "first node", file: sameNodes + "placements:\n  default: {node: ctl}\n  eu-prod: {node: h4}\n  eu-db: {node: h3, open: [acme]}\n  idle: {node: h3}\n",
			changes: []string{"placement-node:node"},
		},
		{
			name: "node of an existing placement waits", file: strings.Replace(samePlacements, "idle: {}", "idle: {node: h9}", 1),
			note: "idle waits for node h9", gap: "h9",
		},
		{
			name: "absent node leaves the placement alone", file: "placements:\n  default: {node: ctl}\n  eu-prod: {}\n  eu-db: {node: h3, open: [acme]}\n  idle: {}\n",
			note: "eu-prod names no node in the file; it stays on h4",
		},
		{
			name: "repoint, checks pass", file: strings.Replace(samePlacements, "eu-prod: {node: h4}", "eu-prod: {node: h3}", 1),
			mut: func(l *serverconfig.Live) {
				l.Moves[serverconfig.MoveKey("eu-prod", "h3")] = []planfile.Check{{Name: "target online", OK: true}}
				l.MoveEnvs[serverconfig.MoveKey("eu-prod", "h3")] = []string{"a/b/c", "a/b/d", "a/e/f", "a/e/g"}
			},
			changes: []string{"placement-move:node"},
		},
		{
			name: "repoint, a check fails", file: strings.Replace(samePlacements, "eu-prod: {node: h4}", "eu-prod: {node: h3}", 1),
			mut: func(l *serverconfig.Live) {
				l.Moves[serverconfig.MoveKey("eu-prod", "h3")] = []planfile.Check{
					{Name: "target online", OK: true},
					{Name: "disk", Detail: "h3 free 2 GB, needs 9 GB + 10%"},
				}
			},
			blocker: "disk: h3 free 2 GB",
		},
		{
			name: "removed placement is a removal row", file: "placements:\n  default: {node: ctl}\n  eu-prod: {node: h4}\n  eu-db: {node: h3, open: [acme]}\n",
			changes: []string{"placement-delete:idle"},
		},
		{
			name: "removed placement in use carries a note", file: "placements:\n  default: {node: ctl}\n  eu-db: {node: h3, open: [acme]}\n  idle: {}\n",
			changes: []string{"placement-delete:eu-prod"},
		},
		{
			name: "the default is never a removal row", file: "placements:\n  idle: {}\n",
			changes: []string{"placement-delete:eu-prod", "placement-delete:eu-db"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := diff(t, c.file, withNodes, func(l *serverconfig.Live) {
				if c.mut != nil {
					c.mut(l)
				}
			})
			wantKinds(t, p, c.changes...)
			has(t, "blockers", p.Blockers, c.blocker)
			if c.note != "" {
				has(t, "notes", p.Notes, c.note)
			}
			if c.gap != "" {
				if _, ok := gap(p, "node", c.gap); !ok {
					t.Errorf("gaps = %+v, want a node gap for %s", p.Gaps, c.gap)
				}
			}
			if c.gap == "" && len(p.Gaps) != 0 {
				t.Errorf("gaps = %+v, want none", p.Gaps)
			}
		})
	}
}

func TestNodeGapCarriesTags(t *testing.T) {
	p := diff(t, sameNodes+"  h9: {tags: [storage, egress]}\n"+samePlacements+"  eu: {node: h9}\n  us: {node: h9}\n", withNodes)
	if len(p.Gaps) != 1 {
		t.Fatalf("gaps = %+v, want one for h9", p.Gaps)
	}
	if g := p.Gaps[0]; g.Key != "h9" || !slices.Equal(g.Tags, []string{"egress", "storage"}) {
		t.Errorf("gap = %+v", g)
	}
	if !p.AutoOK() {
		t.Errorf("a gap never gates the auto-apply: %+v", p)
	}
}

func TestPlacementMoveImpact(t *testing.T) {
	file := strings.Replace(samePlacements, "eu-prod: {node: h4}", "eu-prod: {node: h3}", 1)
	envs := []string{"a/b/c", "a/b/d", "a/e/f", "a/e/g"}
	mut := func(l *serverconfig.Live) {
		l.MoveEnvs[serverconfig.MoveKey("eu-prod", "h3")] = envs
	}
	p := diff(t, file, withNodes, mut)
	imp := p.Impacts()
	if len(imp) != 1 || !strings.Contains(imp[0], "moves 4 envs (a/b/c, a/b/d, a/e/f, +1) to h3") {
		t.Errorf("impacts = %q", imp)
	}
	if p.AutoOK() {
		t.Error("a move is risky: it needs the approver's confirm")
	}
	// the key an approved move is remembered by does not hold the text
	c := p.Changes[0]
	if c.Kind != "placement-move" || c.Tile != "eu-prod" || c.Field != "node" {
		t.Errorf("change = %+v", c)
	}

	// a forgotten source is a restore
	p = diff(t, file, withNodes, mut, func(l *serverconfig.Live) {
		l.Placements[1].Node = "h6"
	})
	if imp := p.Impacts(); len(imp) != 1 || !strings.Contains(imp[0], "restores from backups 4 envs") {
		t.Errorf("impacts = %q", imp)
	}

	// no envs: it repoints at once, with nothing to confirm
	p = diff(t, file, withNodes)
	if len(p.Impacts()) != 0 || len(p.Changes) != 1 {
		t.Errorf("plan = %+v, want one change and no impact", p)
	}
}

func TestPlacementRemovalRowKeys(t *testing.T) {
	p := diff(t, "placements:\n  default: {node: ctl}\n", withNodes)
	got := map[string]string{}
	for _, c := range p.Removals() {
		got[c.Key] = c.Note
	}
	if !strings.Contains(got["placement:eu-prod"], "in use by 3 pin or envs") {
		t.Errorf("removals = %+v", got)
	}
	if _, ok := got["placement:idle"]; !ok {
		t.Errorf("removals = %+v", got)
	}
	if _, ok := got["placement:default"]; ok {
		t.Error("the default placement is never a removal row")
	}
}

func TestExportPlacementsRoundTrip(t *testing.T) {
	l := full()
	withNodes(&l)
	out, err := serverconfig.Export(l)
	if err != nil {
		t.Fatal(err)
	}
	f, err := serverconfig.Parse(out)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	p := serverconfig.Diff(f, l)
	if len(p.Changes) != 0 || p.Blocked() || len(p.Gaps) != 0 {
		t.Errorf("export diffs against itself: %+v\n%s", p, out)
	}
	for _, want := range []string{"nodes:", "placements:", "eu-prod:", "node: h4", "- acme"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("export lacks %q:\n%s", want, out)
		}
	}
	for _, never := range []string{"h5", "h6", "open: all"} {
		if strings.Contains(string(out), never) {
			t.Errorf("export holds %q:\n%s", never, out)
		}
	}
}
