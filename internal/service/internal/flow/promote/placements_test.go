package promote

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/planfile"
)

const placeHead = "version: 1\nstack: shop\nladder: [dev, prd]\nhead: main\nbase:\n  tiles:\n    api: {image: nginx:1, port: 80}\n"

func facts() planfile.PlacementFacts {
	return planfile.PlacementFacts{Known: map[string]planfile.PlacementFact{
		"eu-prod": {Open: true, HasNode: true, Online: true, Tags: []string{"ingress", "egress", "storage"}},
		"eu-db":   {Open: true, HasNode: true, Online: true, Tags: []string{"storage"}},
		"closed":  {Open: false, HasNode: true, Online: true},
		"waiting": {Open: true},
		"edge":    {Open: true, HasNode: true, Online: true, Tags: []string{"ingress"}},
	}}
}

// wire gives the flow a gatherer; sets collects what apply would write.
func wire(w *world, live PlacementLive, sets *[]PlacementWrite) {
	live.Placements = facts()
	live.MayPlace = true // an owner approves; the service test covers a member
	live.Set = func(_ context.Context, pw PlacementWrite) error {
		*sets = append(*sets, pw)
		return nil
	}
	w.f.Placements = func(context.Context, string) (PlacementLive, error) { return live, nil }
}

func TestStackFilePlacing(t *testing.T) {
	r, err := Load([]byte(placeHead+"pin: eu-prod\ningress: eu-prod\nexits: [eu-prod, own]\nenvironments:\n  dev: {pin: eu-db}\n  prd: {from: promote}\n"), nil, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if r.Placing.Pin != "eu-prod" || !slices.Equal(r.Placing.Exits, []string{"eu-prod", "own"}) || r.Envs["dev"].Placing.Pin != "eu-db" || !r.Envs["prd"].Placing.IsZero() {
		t.Errorf("placing = %+v dev %+v", r.Placing, r.Envs["dev"].Placing)
	}
	for name, c := range map[string]struct{ src, want string }{
		"bad pin":     {"pin: Eu_Prod\n", "not a placement name"},
		"dup exit":    {"exits: [a, a]\n", "listed twice"},
		"bad env pin": {"environments:\n  dev: {pin: 'x y'}\n", "not a placement name"},
	} {
		if _, err := Load([]byte(placeHead+c.src), nil, "acme"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// the main file's alone, at the stack level
	_, err = Load([]byte(placeHead+"include: [x.yml]\n"), func(string) ([]byte, error) { return []byte("pin: eu-prod\n"), nil }, "acme")
	if err == nil || !strings.Contains(err.Error(), "main stack file") {
		t.Errorf("include pin: err = %v", err)
	}
	// an include's env section fills a gap the main file leaves; the main file wins
	inc := "environments:\n  dev: {pin: eu-db}\n  prd: {pin: eu-db}\n"
	r, err = Load([]byte(placeHead+"include: [x.yml]\nenvironments:\n  dev: {pin: eu-prod}\n  prd: {from: promote}\n"),
		func(string) ([]byte, error) { return []byte(inc), nil }, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if r.Envs["dev"].Placing.Pin != "eu-prod" || r.Envs["prd"].Placing.Pin != "eu-db" {
		t.Errorf("merged pins dev %q prd %q", r.Envs["dev"].Placing.Pin, r.Envs["prd"].Placing.Pin)
	}
}

func TestPlanPlacing(t *testing.T) {
	move := planfile.MoveFact{From: "default", To: "eu-prod", Downtime: "about 20 s", Checks: []planfile.Check{{Name: "target online", OK: true}}}
	bad := planfile.MoveFact{Checks: []planfile.Check{{Name: "target online", Detail: "h4 offline"}}}
	for _, c := range []struct {
		name    string
		file    string
		env     string // dev or prd
		live    PlacementLive
		changes string // kinds joined
		blocker string
		warn    string
		impact  string
	}{
		{name: "absent says nothing", file: placeHead, env: "dev"},
		{
			name: "stack keys land with the bottom rung", file: placeHead + "pin: eu-prod\ningress: eu-prod\nexits: [eu-prod]\n", env: "dev",
			changes: "pin:shop ingress:shop exits:shop",
		},
		{name: "stack keys wait for the bottom rung", file: placeHead + "pin: eu-prod\n", env: "prd"},
		{name: "env keys land with the env", file: placeHead + "environments:\n  dev: {}\n  prd: {from: promote, pin: eu-db, ingress: eu-prod}\n", env: "prd", changes: "pin:prd ingress:prd"},
		{name: "unchanged is clean", file: placeHead + "pin: eu-prod\n", env: "dev", live: PlacementLive{Stack: planfile.Placing{Pin: "eu-prod"}}},
		{name: "undeclared", file: placeHead + "pin: ghost\n", env: "dev", changes: "pin:shop", blocker: "not declared"},
		{name: "closed", file: placeHead + "environments:\n  dev: {pin: closed}\n  prd: {from: promote}\n", env: "dev", changes: "pin:dev", blocker: "not open"},
		{
			name: "storage env on a placement without it", file: placeHead + "pin: eu-prod\nenvironments:\n  dev: {pin: waiting}\n  prd: {from: promote}\n", env: "dev",
			live: PlacementLive{Needs: func(context.Context) (map[string]planfile.Needs, error) {
				return map[string]planfile.Needs{"dev": {Storage: true}}, nil
			}}, changes: "pin:shop pin:dev", warn: "no node yet",
		},
		{
			// nothing is deployed yet, so only the file says the env holds data
			name: "file volumes need storage", file: placeHead + "pin: edge\nvolumes:\n  data: {}\n", env: "dev",
			changes: "pin:shop", blocker: "placement edge does not fit: needs storage",
		},
		{
			name: "ingress needs ingress", file: placeHead + "ingress: eu-db\n", env: "dev", changes: "ingress:shop", blocker: "needs ingress",
		},
		{
			name: "exit needs egress", file: placeHead + "exits: [eu-db]\n", env: "dev", changes: "exits:shop", blocker: "needs egress",
		},
		{
			name: "pin that moves envs", file: placeHead + "pin: eu-prod\n", env: "dev", changes: "pin:shop",
			live: PlacementLive{Moves: func(_ context.Context, scope, pl string, _ []string) (map[string]planfile.MoveFact, error) {
				return map[string]planfile.MoveFact{"shop/dev": move, "shop/prd": move}, nil
			}},
			impact:  "moves 2 envs (shop/dev, shop/prd) to eu-prod; each stops for about 20 s",
			blocker: "pin eu-prod moves 2 envs (shop/dev, shop/prd) to eu-prod; each stops for about 20 s; confirm that move on the Placement tab or with stackr pin eu-prod --stack shop, then promote again",
		},
		{
			name: "pin whose checks fail", file: placeHead + "environments:\n  dev: {pin: eu-prod}\n  prd: {from: promote}\n", env: "dev", changes: "pin:dev", blocker: "shop/dev cannot move: target online: h4 offline",
			live: PlacementLive{Moves: func(_ context.Context, scope, pl string, _ []string) (map[string]planfile.MoveFact, error) {
				return map[string]planfile.MoveFact{"shop/dev": bad}, nil
			}},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := setup(t)
			w.fake.Digests = map[string]string{"nginx:1": "sha256:one"}
			w.files["p"] = c.file
			var sets []PlacementWrite
			wire(w, c.live, &sets)
			env := w.dev
			if c.env == "prd" {
				env = w.prd
			}
			p, err := w.f.Plan(ctx, env.ID, w.release(t, "p").ID, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			imp := ""
			for _, ch := range p.Changes {
				if ch.Kind == "pin" || ch.Kind == "ingress" || ch.Kind == "exits" {
					got = append(got, ch.Kind+":"+ch.Tile)
					imp += ch.Impact
				}
			}
			if strings.Join(got, " ") != c.changes {
				t.Errorf("changes = %v, want %q", got, c.changes)
			}
			if imp != c.impact {
				t.Errorf("impact = %q, want %q", imp, c.impact)
			}
			has := func(ss []string, sub string) bool {
				return sub == "" && len(ss) == 0 || sub != "" && slices.ContainsFunc(ss, func(s string) bool { return strings.Contains(s, sub) })
			}
			blockers := slices.DeleteFunc(slices.Clone(p.Blockers), func(s string) bool {
				return !strings.Contains(s, "placement") && !strings.Contains(s, "cannot move") && !strings.Contains(s, "Placement tab")
			})
			if !has(blockers, c.blocker) {
				t.Errorf("blockers = %v, want %q", p.Blockers, c.blocker)
			}
			warns := slices.DeleteFunc(slices.Clone(p.Warnings), func(s string) bool { return !strings.Contains(s, "placement") })
			if !has(warns, c.warn) {
				t.Errorf("warnings = %v, want %q", p.Warnings, c.warn)
			}
		})
	}
}

// The stack pin does not move an env the file pins itself.
func TestPlanPlacingExcept(t *testing.T) {
	w := setup(t)
	w.files["p"] = placeHead + "pin: eu-prod\nenvironments:\n  dev: {}\n  prd: {from: promote, pin: eu-db}\n"
	var sets []PlacementWrite
	var gotExcept []string
	wire(w, PlacementLive{Moves: func(_ context.Context, scope, pl string, except []string) (map[string]planfile.MoveFact, error) {
		if scope == "stack" {
			gotExcept = except
		}
		return nil, nil
	}}, &sets)
	if _, err := w.f.Plan(ctx, w.dev.ID, w.release(t, "p").ID, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(gotExcept, []string{"prd"}) {
		t.Errorf("except = %v", gotExcept)
	}
}

// Without a gatherer the keys are named and not read.
func TestPlanPlacingUnwired(t *testing.T) {
	w := setup(t)
	w.files["p"] = placeHead + "pin: eu-prod\n"
	p, err := w.f.Plan(ctx, w.dev.ID, w.release(t, "p").ID, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(p.Warnings, func(s string) bool { return strings.Contains(s, "not read here") }) {
		t.Errorf("warnings = %v", p.Warnings)
	}
}

// applyPlacing hands every planned change to Set, once.
func TestApplyPlacing(t *testing.T) {
	w := setup(t)
	w.files["p"] = placeHead + "pin: eu-prod\nexits: [eu-prod, own]\nenvironments:\n  dev: {ingress: eu-prod}\n  prd: {from: promote}\n"
	var sets []PlacementWrite
	moves := func(_ context.Context, scope, pl string, _ []string) (map[string]planfile.MoveFact, error) {
		return map[string]planfile.MoveFact{"shop/dev": {Checks: []planfile.Check{{Name: "x", OK: true}}}}, nil
	}
	wire(w, PlacementLive{Moves: moves}, &sets)
	_, wk, err := w.f.plan(ctx, w.dev.ID, w.release(t, "p").ID, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.f.applyPlacing(ctx, wk, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(sets) != 3 {
		t.Fatalf("sets = %+v", sets)
	}
	pin := sets[0]
	if pin.Field != "pin" || pin.Scope != "stack" || pin.ScopeID != w.st.ID || !slices.Equal(pin.Refs, []string{"eu-prod"}) || len(pin.Except) != 0 {
		t.Errorf("pin write = %+v", pin)
	}
	if sets[1].Field != "exits" || !slices.Equal(sets[1].Refs, []string{"eu-prod", "own"}) {
		t.Errorf("exits write = %+v", sets[1])
	}
	if sets[2].Field != "ingress" || sets[2].Scope != "env" || sets[2].ScopeID != w.dev.ID {
		t.Errorf("env ingress write = %+v", sets[2])
	}
}

// Export writes the stack's keys, and an env's pin only when it differs from
// what it inherits; the file reads back clean.
func TestExportPlacing(t *testing.T) {
	w := setup(t)
	var sets []PlacementWrite
	wire(w, PlacementLive{
		Stack:   planfile.Placing{Pin: "eu-prod", Ingress: "eu-prod", Exits: []string{"eu-prod", "own"}},
		Env:     planfile.Placing{Pin: "eu-db", Exits: []string{"own"}},
		Inherit: "eu-prod",
	}, &sets)
	b, _, err := w.f.Export(ctx, w.st.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	for _, want := range []string{"pin: eu-prod", "ingress: eu-prod", "- eu-prod", "pin: eu-db", "- own"} {
		if !strings.Contains(out, want) {
			t.Errorf("export lacks %q:\n%s", want, out)
		}
	}
	if _, err := Load(b, nil, "acme"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	wire(w, PlacementLive{Stack: planfile.Placing{Pin: "eu-prod"}, Env: planfile.Placing{Pin: "eu-prod"}, Inherit: "eu-prod"}, &sets)
	b, _, err = w.f.Export(ctx, w.st.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(b), "pin:") != 1 {
		t.Errorf("an env on its inherited pin names none:\n%s", b)
	}
}
