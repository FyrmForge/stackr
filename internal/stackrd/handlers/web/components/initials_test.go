package components

import "testing"

func TestInitials(t *testing.T) {
	for in, want := range map[string]string{
		"FyrmForge": "FF", "Fyrm Forge": "FF", "stackr": "S", "": "?",
		"Dumitru Vulpe": "DV", "acme": "A", "ACME": "AC",
	} {
		if got := Initials(in); got != want {
			t.Errorf("Initials(%q) = %q, want %q", in, got, want)
		}
	}
}
