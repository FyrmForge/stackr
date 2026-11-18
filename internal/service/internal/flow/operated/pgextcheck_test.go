package operated

import (
	"slices"
	"strings"
	"testing"
)

var hostileNames = []string{
	"", "Vector", "9vector", "_vector", "vec tor", "vec\"tor", "vec'tor", "vec\ntor", "vec\nRUN x",
	"\nFROM x", "vector\n", "-vector", "--rm", "vec;drop", "vec$(id)", "vec`id`", "vec-tor", "vec.tor",
	"a123456789012345678901234567890123456789012345678901234567890123456", // 65 chars
}

func TestCheckExtNames(t *testing.T) {
	t.Parallel()
	if err := CheckExtNames(nil); err != nil {
		t.Fatalf("nil: %v", err)
	}
	for _, n := range []string{"vector", "vchord", "earth_distance", "a", "postgis_tiger_geocoder", "x9_", "uuid-ossp"} {
		if err := CheckExtNames([]string{n}); err != nil {
			t.Errorf("%q refused: %v", n, err)
		}
	}
	for _, n := range hostileNames {
		if n == "vec-tor" {
			continue // a hyphen is fine in an extension name (uuid-ossp), never in a startup module
		}
		if err := CheckExtNames([]string{"vector", n}); err == nil {
			t.Errorf("%q accepted", n)
		}
	}
}

func TestCheckPreload(t *testing.T) {
	t.Parallel()
	if err := CheckPreload(nil); err != nil {
		t.Fatalf("nil: %v", err)
	}
	if err := CheckPreload([]string{"vchord", "pg_cron"}); err != nil {
		t.Fatalf("good: %v", err)
	}
	for _, n := range hostileNames {
		if err := CheckPreload([]string{n}); err == nil {
			t.Errorf("%q accepted", n)
		}
	}
	err := CheckPreload([]string{"vchord", "pg_stat_statements"})
	if err == nil || !strings.Contains(err.Error(), "pg_stat_statements is always loaded; leave it out") {
		t.Errorf("pg_stat_statements: %v", err)
	}
}

func TestPGOfficial(t *testing.T) {
	t.Parallel()
	for image, want := range map[string]bool{
		"":                                    true,
		"postgres":                            true,
		"postgres:17.11":                      true,
		"postgres:17":                         true,
		"library/postgres:17.11":              true,
		"docker.io/library/postgres:17.11":    true,
		"postgres@sha256:abc":                 true,
		"postgres:17@sha256:abc":              true,
		"ghcr.io/immich-app/postgres:18-vc":   false,
		"pgvector/pgvector:pg17":              false,
		"postgresql:17":                       false,
		"localhost:5000/postgres:17":          false,
		"registry.example.com/postgres:17.11": false,
		"postgres-custom:17":                  false,
	} {
		if got := PGOfficial(image); got != want {
			t.Errorf("PGOfficial(%q) = %v, want %v", image, got, want)
		}
	}
}

func TestExtMissing(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ inst, slice, want []string }{
		{nil, nil, nil},
		{nil, []string{"vector"}, []string{"vector"}},
		{[]string{"vector", "vchord"}, []string{"vchord"}, nil},
		{[]string{"vector"}, []string{"vchord", "vector", "earthdistance", "vchord"}, []string{"vchord", "earthdistance"}},
		{[]string{"vector"}, nil, nil},
	} {
		if got := ExtMissing(c.inst, c.slice); !slices.Equal(got, c.want) {
			t.Errorf("ExtMissing(%v, %v) = %v, want %v", c.inst, c.slice, got, c.want)
		}
	}
}
