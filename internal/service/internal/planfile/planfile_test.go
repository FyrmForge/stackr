package planfile_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/planfile"
)

func plan() planfile.Plan {
	return planfile.Plan{Changes: []planfile.Change{
		{Kind: "create", Tile: "blog"},
		{Kind: "settings", Field: "panel_domain", Old: "a.io", New: "b.io", Impact: "panel moves to b.io; point DNS first"},
		{Kind: "route-delete", Key: "route:old.io", Tile: "old.io", Optional: true},
		{Kind: "dest-delete", Key: "dest:old", Tile: "old", Optional: true},
	}}
}

func TestRiskyRemovalsAutoOK(t *testing.T) {
	p := plan()
	if !p.Risky() {
		t.Error("a plan with an impact line is risky")
	}
	if got := len(p.Removals()); got != 2 {
		t.Errorf("removals = %d, want 2", got)
	}
	if p.AutoOK() {
		t.Error("risky plan is not auto-ok")
	}
	if lines := p.Impacts(); len(lines) != 1 || !strings.Contains(lines[0], "b.io") {
		t.Errorf("impacts = %v", lines)
	}

	quiet := planfile.Plan{Changes: []planfile.Change{{Kind: "create", Tile: "blog"}}}
	if quiet.Risky() || len(quiet.Removals()) != 0 || !quiet.AutoOK() {
		t.Errorf("quiet plan: risky %v, removals %d, auto %v", quiet.Risky(), len(quiet.Removals()), quiet.AutoOK())
	}
	onlyRemoval := planfile.Plan{Changes: []planfile.Change{{Kind: "route-delete", Key: "route:x", Optional: true}}}
	if onlyRemoval.AutoOK() {
		t.Error("a removal row keeps a plan from applying by itself")
	}
	blocked := planfile.Plan{Blockers: []string{"nope"}}
	if blocked.AutoOK() {
		t.Error("a blocked plan is not auto-ok")
	}
	if (planfile.Plan{}).AutoOK() != true {
		t.Error("an empty plan is auto-ok")
	}
}

func TestApprovable(t *testing.T) {
	p := plan()
	for _, c := range []struct {
		name    string
		ticked  []string
		confirm bool
		want    string // "" = ok; else a substring of the refusal
	}{
		{"confirmed, nothing ticked", nil, true, ""},
		{"confirmed, one ticked", []string{"route:old.io"}, true, ""},
		{"confirmed, both ticked", []string{"route:old.io", "dest:old", "dest:old"}, true, ""},
		{"risky unconfirmed", nil, false, "This plan has impact lines; confirm to approve."},
		{"unknown key", []string{"route:nope"}, true, `"route:nope"`},
		{"an applied change is not a removal", []string{"create"}, true, `"create"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := p.Approvable(c.ticked, c.confirm)
			switch {
			case c.want == "" && err != nil:
				t.Errorf("err = %v, want ok", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Errorf("err = %v, want %q", err, c.want)
			}
		})
	}

	quiet := planfile.Plan{}
	if err := quiet.Approvable(nil, false); err != nil {
		t.Errorf("a plan with no impact needs no confirm: %v", err)
	}
	if err := quiet.Approvable([]string{"route:x"}, false); err == nil {
		t.Error("a tick on a plan with no removals is refused")
	}
	if _, ok := errs.IsConflict(p.Approvable(nil, false)); !ok {
		t.Error("the confirm refusal is a Conflict")
	}
	if _, ok := errs.IsInvalid(p.Approvable([]string{"x"}, true)); !ok {
		t.Error("an unknown tick is Invalid")
	}
}

func TestTickedRemovals(t *testing.T) {
	p := plan()
	got := p.TickedRemovals([]string{"dest:old", "route:gone"})
	if len(got) != 1 || got[0].Key != "dest:old" {
		t.Errorf("ticked removals = %+v, want only dest:old", got)
	}
}

func TestOnlyTicked(t *testing.T) {
	p := plan()
	p.Blockers = []string{"b"}
	got := p.OnlyTicked([]string{"dest:old", "route:gone"})
	if len(got.Changes) != 3 || got.Changes[2].Key != "dest:old" || len(got.Removals()) != 1 {
		t.Errorf("changes = %+v, want the two plain ones and dest:old", got.Changes)
	}
	if len(got.Blockers) != 1 || len(p.Changes) != 4 {
		t.Error("OnlyTicked keeps the blockers and leaves the original alone")
	}
	if n := len(p.OnlyTicked(nil).Removals()); n != 0 {
		t.Errorf("nothing ticked leaves %d removals", n)
	}
}

func TestSummary(t *testing.T) {
	add := func(k string) bool { return k == "create" }
	p := plan()
	if s := p.Summary(add); s != "1 to add, 1 to change, 2 removals to review, needs confirm" {
		t.Errorf("summary = %q", s)
	}
	p.Blockers = []string{"x"}
	if s := p.Summary(add); !strings.HasSuffix(s, ", 1 blocker") {
		t.Errorf("summary = %q", s)
	}
	if s := (&planfile.Plan{}).Summary(add); s != "no changes" {
		t.Errorf("empty summary = %q", s)
	}
	one := planfile.Plan{Changes: []planfile.Change{{Kind: "route-delete", Key: "route:x", Optional: true}}}
	if s := one.Summary(add); s != "1 removal to review" {
		t.Errorf("summary = %q", s)
	}
}

// A plan stored before Impact, Optional and Key existed reads back whole.
func TestPlanJSONKeepsOldRows(t *testing.T) {
	old := `{"changes":[{"kind":"create","tile":"blog","new":"u","note":"n"}],"blockers":["b"],"notes":["x"]}`
	var p planfile.Plan
	if err := json.Unmarshal([]byte(old), &p); err != nil {
		t.Fatal(err)
	}
	if p.Changes[0].Impact != "" || p.Changes[0].Optional || p.Changes[0].Key != "" || !p.Blocked() {
		t.Errorf("old row = %+v", p)
	}
	b, err := json.Marshal(p)
	if err != nil || string(b) != old {
		t.Errorf("round trip = %s, %v; want the old bytes (new fields omit when empty)", b, err)
	}
}

type doc struct {
	A int `yaml:"a"`
}

func TestStrictYAML(t *testing.T) {
	var v doc
	hints := map[string]string{"vars": "declare them under params:"}
	if err := planfile.StrictYAML([]byte("a: 1\n"), &v, hints); err != nil || v.A != 1 {
		t.Fatalf("good file: %v %+v", err, v)
	}
	if err := planfile.StrictYAML(nil, &v, hints); err != nil {
		t.Errorf("empty file: %v", err)
	}
	err := planfile.StrictYAML([]byte("vars: 1\nzzz: 2\n"), &v, hints)
	if err == nil || !strings.Contains(err.Error(), "unknown key vars (no longer supported: declare them under params:)") ||
		!strings.Contains(err.Error(), "unknown key zzz") || strings.Contains(err.Error(), "type") {
		t.Errorf("err = %v", err)
	}
}

func TestCheckParams(t *testing.T) {
	val := "x"
	for name, c := range map[string]struct {
		in   map[string]map[string]planfile.Param
		want string
	}{
		"ok":            {map[string]map[string]planfile.Param{"app": {"a": {Type: "param", Value: &val}, "s": {Type: "secret"}}}, ""},
		"collection":    {map[string]map[string]planfile.Param{"App": {}}, `collection "App"`},
		"name":          {map[string]map[string]planfile.Param{"app": {"A-b": {Type: "param"}}}, "lower-case"},
		"secret value":  {map[string]map[string]planfile.Param{"app": {"s": {Type: "secret", Value: &val}}}, "never goes in the file"},
		"type":          {map[string]map[string]planfile.Param{"app": {"s": {Type: "other"}}}, "type must be param or secret"},
		"empty is fine": {nil, ""},
	} {
		err := planfile.CheckParams(c.in)
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
}
