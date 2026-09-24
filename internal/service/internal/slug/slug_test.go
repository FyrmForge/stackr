package slug

import "testing"

func TestGrammar(t *testing.T) {
	for in, want := range map[string]string{
		"Orders DB":    "orders-db",
		"  --a__b--  ": "a-b",
		"!!!":          "",
		"Café 2":       "caf-2",
		"x":            "x",
	} {
		if got := Make(in); got != want {
			t.Errorf("Make(%q) = %q, want %q", in, got, want)
		}
	}
	for s, want := range map[string]bool{
		"orders-db": true,
		"a":         true,
		"a_b":       false,
		"-a":        false,
		"a--b":      false,
		"A":         false,
		"":          false,
	} {
		if Valid(s) != want {
			t.Errorf("Valid(%q) = %v", s, !want)
		}
	}
	for s, want := range map[string]bool{
		"api_key": true,
		"a-b":     false,
		"A":       false,
		"":        false,
	} {
		if ValidName(s) != want {
			t.Errorf("ValidName(%q) = %v", s, !want)
		}
	}
	for s, want := range map[string]bool{
		"PATH": true,
		"_x1":  true,
		"1X":   false,
		"A-B":  false,
		"":     false,
	} {
		if ValidEnvKey(s) != want {
			t.Errorf("ValidEnvKey(%q) = %v", s, !want)
		}
	}
}
