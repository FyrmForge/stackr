package service

import "testing"

func TestRefuseServerRefsEntrypoint(t *testing.T) {
	for in, bad := range map[string]bool{
		"sh -c":       false,
		"":            false,
		"sh ${{ x }}": true,
		`sh "open`:    true,
	} {
		err := refuseServerRefs(Tile{Entrypoint: in})
		if (err != nil) != bad {
			t.Errorf("entrypoint %q: err = %v, want refusal %v", in, err, bad)
		}
	}
}
