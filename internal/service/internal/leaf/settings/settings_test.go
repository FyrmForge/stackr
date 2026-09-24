package settings

import (
	"reflect"
	"testing"
)

func ptr[T any](v T) *T { return &v }

func TestResolveCascade(t *testing.T) {
	server := Settings{MemLimitMB: ptr(1024), CPULimit: ptr(2.0)}
	org := Settings{MemLimitMB: ptr(512)}
	stack := Settings{}
	env := Settings{CPULimit: ptr(0.0), ProtectUser: ptr("u"), ProtectPassword: ptr("p")}
	tests := []struct {
		name   string
		levels []Settings
		want   Resolved
	}{
		{"nothing set is the default", nil, Resolved{}},
		{"server only", []Settings{server}, Resolved{CPULimit: 2, MemLimitMB: 1024}},
		{"org overrides server", []Settings{server, org}, Resolved{CPULimit: 2, MemLimitMB: 512}},
		{"empty stack inherits", []Settings{server, org, stack}, Resolved{CPULimit: 2, MemLimitMB: 512}},
		{"explicit zero at env is unlimited", []Settings{server, org, stack, env},
			Resolved{CPULimit: 0, MemLimitMB: 512, ProtectUser: "u", ProtectPassword: "p"}},
		{"protect pair is one unit", []Settings{env, {ProtectUser: ptr("v")}},
			Resolved{ProtectUser: "v"}},
	}
	for _, tt := range tests {
		if got := Resolve(tt.levels...); got != tt.want {
			t.Errorf("%s: %+v, want %+v", tt.name, got, tt.want)
		}
	}
	cpu, mem := Resolve(server).EffectiveLimits(0.5, 0)
	if cpu != 0.5 || mem != 1024 {
		t.Errorf("tile rung: %v %v, want 0.5 1024", cpu, mem)
	}
}

func TestExplicitZeroSurvivesJSON(t *testing.T) {
	s, err := Parse(Settings{CPULimit: ptr(0.0)}.JSON())
	if err != nil || s.CPULimit == nil || *s.CPULimit != 0 {
		t.Fatalf("round trip lost the zero: %+v %v", s, err)
	}
	if _, err := Parse("{not json"); err == nil {
		t.Error("corrupt blob read as no overrides")
	}
}

// B24: a typo is refused and the old value stays.
func TestMergeRefuses(t *testing.T) {
	cur := Settings{MemLimitMB: ptr(512), CPULimit: ptr(1.0)}
	tests := []struct {
		name string
		vals map[string]string
		at   Scope
	}{
		{"typo in a number", map[string]string{"mem_limit_mb": "51 2"}, Org},
		{"negative", map[string]string{"mem_limit_mb": "-1"}, Org},
		{"not a bool", map[string]string{"protect": "yes please"}, Org},
		{"not a float", map[string]string{"cpu_limit": "1,5"}, Org},
		{"half a pair", map[string]string{"protect_user": "u"}, Org},
		{"unknown key", map[string]string{"mem_limt_mb": "5"}, Org},
		{"install knob on an env", map[string]string{"workers": "4"}, Env},
		{"one bad value refuses all", map[string]string{"cpu_limit": "2", "mem_limit_mb": "x"}, Org},
	}
	for _, tt := range tests {
		got, err := Merge(cur, tt.vals, tt.at)
		if err == nil {
			t.Errorf("%s: accepted", tt.name)
		}
		if !reflect.DeepEqual(got, cur) {
			t.Errorf("%s: level changed to %+v", tt.name, got)
		}
	}
}

func TestMergeApplies(t *testing.T) {
	cur := Settings{MemLimitMB: ptr(512), CPULimit: ptr(1.0)}
	got, err := Merge(cur, map[string]string{"cpu_limit": " 0 ", "mem_limit_mb": ""}, Stack)
	if err != nil {
		t.Fatal(err)
	}
	if got.CPULimit == nil || *got.CPULimit != 0 || got.MemLimitMB != nil {
		t.Errorf("merge = %+v, want cpu 0 and mem cleared", got)
	}
}

// Every cascade knob has a Settings field and a Set case.
func TestCatalogueMatchesSettings(t *testing.T) {
	n := 0
	for _, k := range Catalogue {
		if k.Scopes&Flat != 0 {
			continue
		}
		n++
		var s Settings
		raw := map[Type]string{TInt: "1", TFloat: "1.5", TBool: "true", TStr: "x"}[k.Type]
		if err := Set(&s, k.Key, raw); err != nil {
			t.Errorf("Set(%s): %v", k.Key, err)
		}
		if reflect.DeepEqual(s, Settings{}) {
			t.Errorf("Set(%s) wrote no field", k.Key)
		}
	}
	if f := reflect.TypeFor[Settings]().NumField(); f != n {
		t.Errorf("Settings has %d fields, catalogue %d cascade knobs", f, n)
	}
}
