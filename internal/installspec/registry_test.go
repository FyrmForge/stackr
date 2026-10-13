package installspec

import (
	"slices"
	"strings"
	"testing"
)

// The registry must take an index whose other platforms are absent, and the
// spec hash must move so an upgrade recreates the container.
func TestRegistryAcceptsPartialIndexes(t *testing.T) {
	c := Registry()
	for _, e := range []string{
		"REGISTRY_VALIDATION_MANIFESTS_INDEXES_PLATFORMS=none",
		"REGISTRY_STORAGE_DELETE_ENABLED=true",
	} {
		if !slices.Contains(c.Env, e) {
			t.Errorf("env lacks %s: %v", e, c.Env)
		}
	}
	old := c
	old.Env = []string{"REGISTRY_STORAGE_DELETE_ENABLED=true"}
	if old.Hash() == c.Hash() {
		t.Error("spec hash ignores the validation setting")
	}
}

func TestRegistryLocalOnly(t *testing.T) {
	c := Registry()
	for _, p := range c.Ports {
		if !strings.HasPrefix(p, "127.0.0.1:") {
			t.Fatalf("port %q not bound to loopback", p)
		}
	}
	if c.Labels[LabelSpec] != c.Hash() || c.Labels[LabelRole] != "registry" {
		t.Fatalf("labels %v", c.Labels)
	}
	if !slices.Contains(c.RunArgs(), "127.0.0.1:5000:5000") {
		t.Fatalf("run args %v", c.RunArgs())
	}
}
