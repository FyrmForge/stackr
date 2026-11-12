package main

import (
	"strings"
	"testing"
)

// An empty value for a tile list flag clears the list, spaced or with "=".
func TestTileSetEmptyListFlagClears(t *testing.T) {
	for _, args := range [][]string{{"--published-ports", ""}, {"--published-ports="}} {
		r := fake(t, map[string]string{"/tiles/api": `{"tile":{"slug":"api"}}`})
		a := append([]string{"tile", "set", "api", "--stack", "shop", "--env", "dev"}, args...)
		code, _, errw := cli(t, a...)
		w := writes(r)
		if code != 0 || len(w) != 1 || !strings.Contains(w[0], `"published_ports":""`) {
			t.Errorf("%q = %d %s %v, want a clear", args, code, errw, w)
		}
	}
}
