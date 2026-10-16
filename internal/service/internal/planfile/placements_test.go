package planfile

import (
	"strings"
	"testing"
)

func TestPlacementRef(t *testing.T) {
	facts := PlacementFacts{Known: map[string]PlacementFact{
		"ok":      {Open: true, HasNode: true, Online: true, Tags: []string{"storage", "ingress", "egress"}},
		"closed":  {Open: false, HasNode: true, Online: true},
		"nonode":  {Open: true},
		"off":     {Open: true, HasNode: true, Tags: []string{"storage"}},
		"plain":   {Open: true, HasNode: true, Online: true},
		"plainTo": {Open: true, HasNode: true, Online: true, Tags: []string{"egress"}},
	}}
	tests := []struct {
		name, ref string
		need      Needs
		block     string // substring, "" = none
		warn      string
	}{
		{"empty", "", Needs{}, "", ""},
		{"undeclared", "ghost", Needs{}, "not declared", ""},
		{"closed", "closed", Needs{}, "not open", ""},
		{"no node", "nonode", Needs{Storage: true}, "", "no node yet"},
		{"offline", "off", Needs{Storage: true}, "", "offline"},
		{"storage missing", "plain", Needs{Storage: true}, "needs storage", ""},
		{"ingress missing", "plain", Needs{Ingress: true}, "needs ingress", ""},
		{"egress ok", "plainTo", Needs{Egress: true}, "", ""},
		{"all ok", "ok", Needs{Storage: true, Ingress: true, Egress: true}, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, w := PlacementRef(tc.ref, tc.need, facts)
			if (tc.block == "") != (b == "") || !strings.Contains(b, tc.block) {
				t.Errorf("blocker = %q, want %q", b, tc.block)
			}
			if (tc.warn == "") != (w == "") || !strings.Contains(w, tc.warn) {
				t.Errorf("warning = %q, want %q", w, tc.warn)
			}
		})
	}
}

func TestPlacingPlan(t *testing.T) {
	facts := PlacementFacts{Known: map[string]PlacementFact{
		"eu-prod": {Open: true, HasNode: true, Online: true, Tags: []string{"ingress", "egress"}},
		"eu-db":   {Open: true, HasNode: true, Online: true, Tags: []string{"storage"}},
	}}
	pass := MoveFact{From: "a", To: "eu-prod", Downtime: "about 20 s", Checks: []Check{{Name: "target online", OK: true}}}
	fail := MoveFact{From: "a", To: "eu-prod", Checks: []Check{{Name: "disk", OK: false, Detail: "h4 free 1 GB"}}}
	t.Run("unchanged is quiet", func(t *testing.T) {
		cs, b, w := PlacingPlan(PlacingIn{Scope: "org", Want: Placing{Pin: "eu-prod", Ingress: "eu-prod", Exits: []string{"eu-prod", "own"}},
			Have: Placing{Pin: "eu-prod", Ingress: "eu-prod", Exits: []string{"eu-prod", "own"}}, Facts: facts})
		if len(cs)+len(b)+len(w) != 0 {
			t.Errorf("got %v %v %v", cs, b, w)
		}
	})
	t.Run("absent says nothing", func(t *testing.T) {
		cs, b, _ := PlacingPlan(PlacingIn{Scope: "org", Have: Placing{Pin: "eu-prod", Exits: []string{"own"}}, Facts: facts})
		if len(cs)+len(b) != 0 {
			t.Errorf("got %v %v", cs, b)
		}
	})
	t.Run("changes", func(t *testing.T) {
		cs, b, _ := PlacingPlan(PlacingIn{Scope: "stack", Tile: "api", Want: Placing{Pin: "eu-db", Ingress: "eu-prod", Exits: []string{"eu-prod"}}, Facts: facts, Need: Needs{Storage: true}})
		if len(b) != 0 || len(cs) != 3 || cs[0].Kind != "pin" || cs[1].Kind != "ingress" || cs[2].New != "eu-prod" || cs[0].Tile != "api" || cs[0].Field != "stack" {
			t.Errorf("got %+v %v", cs, b)
		}
	})
	t.Run("clear exits", func(t *testing.T) {
		cs, _, _ := PlacingPlan(PlacingIn{Scope: "org", Want: Placing{Exits: []string{}}, Have: Placing{Exits: []string{"own"}}, Facts: facts})
		if len(cs) != 1 || cs[0].Old != "own" || cs[0].New != "" {
			t.Errorf("got %+v", cs)
		}
	})
	t.Run("wrong tags", func(t *testing.T) {
		_, b, _ := PlacingPlan(PlacingIn{Scope: "org", Want: Placing{Pin: "eu-prod", Ingress: "eu-db", Exits: []string{"eu-db"}}, Facts: facts, Need: Needs{Storage: true}})
		if len(b) != 3 {
			t.Errorf("blockers %v", b)
		}
	})
	t.Run("move passes is an impact", func(t *testing.T) {
		cs, b, _ := PlacingPlan(PlacingIn{Scope: "stack", Tile: "api", Want: Placing{Pin: "eu-prod"}, Have: Placing{Pin: "eu-db"}, Facts: facts,
			Moves: map[string]MoveFact{"api/prod": pass, "api/dev": pass}})
		if len(b) != 0 || len(cs) != 1 || cs[0].Impact != "moves 2 envs (api/dev, api/prod) to eu-prod; each stops for about 20 s" {
			t.Errorf("got %+v %v", cs, b)
		}
	})
	t.Run("move fails is a blocker", func(t *testing.T) {
		cs, b, _ := PlacingPlan(PlacingIn{Scope: "env", Tile: "prod", Want: Placing{Pin: "eu-prod"}, Have: Placing{Pin: "eu-db"}, Facts: facts,
			Moves: map[string]MoveFact{"api/prod": fail}})
		if len(b) != 1 || !strings.Contains(b[0], "disk: h4 free 1 GB") || cs[0].Impact != "" {
			t.Errorf("got %+v %v", cs, b)
		}
	})
	t.Run("moves ignored when pin same", func(t *testing.T) {
		cs, b, _ := PlacingPlan(PlacingIn{Scope: "stack", Want: Placing{Pin: "eu-prod"}, Have: Placing{Pin: "eu-prod"}, Facts: facts,
			Moves: map[string]MoveFact{"api/prod": fail}})
		if len(cs)+len(b) != 0 {
			t.Errorf("got %+v %v", cs, b)
		}
	})
}

func TestCheckPlacing(t *testing.T) {
	for _, tc := range []struct {
		p  Placing
		ok bool
	}{
		{Placing{Pin: "eu-prod", Exits: []string{"own", "h4"}}, true},
		{Placing{Pin: "Bad_Name"}, false},
		{Placing{Ingress: "own"}, true}, // own is a valid name; the facts refuse it
		{Placing{Exits: []string{"a", "a"}}, false},
		{Placing{Exits: []string{"-x"}}, false},
	} {
		if err := CheckPlacing("k", tc.p); (err == nil) != tc.ok {
			t.Errorf("%+v: %v", tc.p, err)
		}
	}
}
