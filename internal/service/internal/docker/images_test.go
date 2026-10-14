package docker

import (
	"errors"
	"testing"
)

// buildx prints a header before the ids; neither the header nor a stderr
// warning is a removed entry.
func TestParsePruneSkipsNoise(t *testing.T) {
	n, total, _ := parsePrune("Deleted build cache objects:\nabc\ndef\nTotal:\t1.2GB\n")
	if n != 2 || total != "1.2GB" {
		t.Errorf("n=%d total=%q", n, total)
	}
	if n, _, _ = parsePrune("WARNING: something odd happened\nabc\n"); n != 1 {
		t.Errorf("a warning line counted: n=%d", n)
	}
}

// A box that never built anything has no builder: nothing to prune, not a
// failure of the nightly job.
func TestBuildCacheResultNoBuilder(t *testing.T) {
	exit := errors.New("exit status 1")
	n, total, err := buildCacheResult("", "ERROR: no builder \"stackr-builder\" found\n", exit)
	if err != nil || n != 0 || total != "" {
		t.Errorf("n=%d total=%q err=%v", n, total, err)
	}
	if _, _, err = buildCacheResult("", "ERROR: daemon down\n", exit); err == nil {
		t.Error("a real failure was swallowed")
	}
}

// Only a cluster registry ref gets a minted pass, for its repository; any
// other registry keeps the caller's auth.
func TestClusterRepo(t *testing.T) {
	for ref, want := range map[string]string{
		"localhost:5000/acme/shop/api:v1":                      "acme/shop/api",
		"localhost:5000/stackr.system/stackr@sha256:abc":       "stackr.system/stackr",
		"localhost:5000/acme/docker.io/library/postgres":       "acme/docker.io/library/postgres",
		"ghcr.io/acme/api:v1":                                  "",
		"localhost:5001/acme/shop/api:v1":                      "",
		"registry.example.com/localhost:5000/acme/shop/api:v1": "",
	} {
		got, ok := clusterRepo(ref)
		if got != want || ok != (want != "") {
			t.Errorf("%s: %q %v, want %q", ref, got, ok, want)
		}
	}
}
