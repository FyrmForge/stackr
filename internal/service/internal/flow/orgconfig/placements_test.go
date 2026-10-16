package orgconfig_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/flow/orgconfig"
	"github.com/FyrmForge/stackr/internal/service/internal/planfile"
)

func placed() planfile.PlacementFacts {
	return planfile.PlacementFacts{Known: map[string]planfile.PlacementFact{
		"eu-prod": {Open: true, HasNode: true, Online: true, Tags: []string{"ingress", "egress"}},
		"eu-db":   {Open: true, HasNode: true, Online: true, Tags: []string{"storage"}},
		"closed":  {Open: false, HasNode: true, Online: true},
		"waiting": {Open: true},
		"down":    {Open: true, HasNode: true, Tags: []string{"storage"}},
	}}
}

func TestDiffPlacing(t *testing.T) {
	move := planfile.MoveFact{From: "default", To: "eu-prod", Downtime: "about 20 s", Checks: []planfile.Check{{Name: "target online", OK: true}}}
	bad := planfile.MoveFact{Checks: []planfile.Check{{Name: "disk", Detail: "h4 free 1 GB"}}}
	for _, c := range []struct {
		name    string
		file    string
		live    func(*orgconfig.Live)
		changes []planfile.Change
		blocker string
		note    string
	}{
		{name: "absent says nothing", file: v1},
		{
			name:    "new pin",
			file:    v1 + "pin: eu-prod\n",
			changes: []planfile.Change{{Kind: "pin", Field: "org", New: "eu-prod"}},
		},
		{name: "same pin is clean", file: v1 + "pin: eu-prod\n", live: func(l *orgconfig.Live) { l.Current.Pin = "eu-prod" }},
		{
			name:    "undeclared",
			file:    v1 + "pin: ghost\n",
			changes: []planfile.Change{{Kind: "pin", Field: "org", New: "ghost"}},
			blocker: "placement ghost is not declared",
		},
		{
			name:    "not open to the org",
			file:    v1 + "pin: closed\n",
			changes: []planfile.Change{{Kind: "pin", Field: "org", New: "closed"}},
			blocker: "not open to this organization",
		},
		{
			name:    "storage needed by an env under the pin",
			file:    v1 + "pin: eu-prod\n",
			live:    func(l *orgconfig.Live) { l.EnvNeeds = map[string]planfile.Needs{"shop/prod": {Storage: true}} },
			changes: []planfile.Change{{Kind: "pin", Field: "org", New: "eu-prod"}},
			blocker: "needs storage",
		},
		{
			name:    "no node yet warns",
			file:    v1 + "pin: waiting\n",
			changes: []planfile.Change{{Kind: "pin", Field: "org", New: "waiting"}},
			note:    "has no node yet",
		},
		{
			name:    "offline warns",
			file:    v1 + "pin: down\n",
			changes: []planfile.Change{{Kind: "pin", Field: "org", New: "down"}},
			note:    "offline",
		},
		{
			name:    "ingress override needs ingress",
			file:    v1 + "ingress: eu-db\n",
			changes: []planfile.Change{{Kind: "ingress", Field: "org", New: "eu-db"}},
			blocker: "needs ingress",
		},
		{
			name:    "exit rung needs egress",
			file:    v1 + "exits: [eu-prod, eu-db, own]\n",
			changes: []planfile.Change{{Kind: "exits", Field: "org", New: "eu-prod, eu-db, own"}},
			blocker: "placement eu-db does not fit: needs egress",
		},
		{
			name:    "empty exits clears",
			file:    v1 + "exits: []\n",
			live:    func(l *orgconfig.Live) { l.Current.Exits = []string{"own"} },
			changes: []planfile.Change{{Kind: "exits", Field: "org", Old: "own"}},
		},
		{
			name: "pin that moves envs is an impact line",
			file: v1 + "pin: eu-prod\n",
			live: func(l *orgconfig.Live) {
				l.Moves = map[string]planfile.MoveFact{"shop/prod": move}
			},
			changes: []planfile.Change{{
				Kind: "pin", Field: "org", New: "eu-prod",
				Impact: "moves 1 env (shop/prod) to eu-prod; each stops for about 20 s",
			}},
		},
		{
			name: "pin whose move checks fail is a blocker",
			file: v1 + "pin: eu-prod\n",
			live: func(l *orgconfig.Live) {
				l.Moves = map[string]planfile.MoveFact{"shop/prod": bad}
			},
			changes: []planfile.Change{{Kind: "pin", Field: "org", New: "eu-prod"}},
			blocker: "shop/prod cannot move: disk: h4 free 1 GB",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, err := orgconfig.Parse([]byte(c.file))
			if err != nil {
				t.Fatal(err)
			}
			l := live()
			l.Placements = placed()
			if c.live != nil {
				c.live(&l)
			}
			p := orgconfig.Diff(f, l)
			if !slices.Equal(p.Changes, c.changes) {
				t.Errorf("changes = %+v\nwant %+v", p.Changes, c.changes)
			}
			one(t, "blockers", p.Blockers, c.blocker)
			one(t, "notes", p.Notes, c.note)
		})
	}
}

func TestParsePlacing(t *testing.T) {
	for _, c := range []struct{ name, file, err string }{
		{"bad pin", v1 + "pin: Eu_Prod\n", "not a placement name"},
		{"bad exit", v1 + "exits: [a, -b]\n", "not a placement name"},
		{"dup exit", v1 + "exits: [a, a]\n", "listed twice"},
		{"ok", v1 + "pin: eu-prod\ningress: default\nexits: [eu-prod, own]\n", ""},
	} {
		_, err := orgconfig.Parse([]byte(c.file))
		if (c.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

func TestSecretGap(t *testing.T) {
	l := live()
	withParams(&l)
	f, err := orgconfig.Parse([]byte(v1 + "params:\n  email:\n    all:\n      token: {type: secret}\n      sender: old@acme.test\n      api_key: {type: secret}\n"))
	if err != nil {
		t.Fatal(err)
	}
	p := orgconfig.Diff(f, l)
	want := planfile.Gap{Kind: "secret", Scope: "org", Env: "all", Key: "email.token"}
	if len(p.Gaps) != 1 || p.Gaps[0].Kind != want.Kind || p.Gaps[0].Key != want.Key || p.Gaps[0].Env != "all" || p.Gaps[0].Scope != "org" {
		t.Errorf("gaps = %+v", p.Gaps)
	}
	if len(p.Notes) != 0 || p.Blocked() || p.AutoOK() {
		t.Errorf("gaps alone: notes %v, blocked %v, auto %v; want a plan with nothing to apply by itself", p.Notes, p.Blocked(), p.AutoOK())
	}
	p.Changes = append(p.Changes, orgconfig.Change{Kind: "param", Tile: "all", Field: "email.sender"})
	if !p.AutoOK() {
		t.Error("a gap must not gate a plan that has changes")
	}
}

// Export writes the pins back and diffing it is clean.
func TestExportPlacing(t *testing.T) {
	l := live()
	l.Placements = placed()
	l.Current = planfile.Placing{Pin: "eu-prod", Ingress: "eu-prod", Exits: []string{"eu-prod", "own"}}
	out, err := orgconfig.Export(l)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"pin: eu-prod", "ingress: eu-prod", "- eu-prod", "- own"} {
		if !strings.Contains(string(out), w) {
			t.Errorf("export lacks %q:\n%s", w, out)
		}
	}
	f, err := orgconfig.Parse(out)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if p := orgconfig.Diff(f, l); len(p.Changes) != 0 || p.Blocked() {
		t.Errorf("export diffs: %v %v", p.Changes, p.Blockers)
	}
}
