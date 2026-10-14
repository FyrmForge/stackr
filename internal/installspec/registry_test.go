package installspec

import (
	"slices"
	"strings"
	"testing"
)

// The registry must take an index whose other platforms are absent, and the
// spec hash must move so an upgrade recreates the container.
func TestRegistryAcceptsPartialIndexes(t *testing.T) {
	c := Registry("/var/lib/stackr")
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

// The registry checks nothing, so nothing but stackrd may reach it: no
// network and no TCP listener, only a socket under the data dir. A host
// network tile shares the host's loopback, a bridge tile its bridge.
func TestRegistryLocalOnly(t *testing.T) {
	c := Registry("/var/lib/stackr")
	if c.HostNetwork || c.Network != "none" || len(c.Ports) != 0 {
		t.Fatalf("host network %v, network %q, ports %v: want no network", c.HostNetwork, c.Network, c.Ports)
	}
	if !slices.Contains(c.Env, "REGISTRY_HTTP_NET=unix") || !slices.Contains(c.Volumes, "/var/lib/stackr/registry:/run/stackr-registry") {
		t.Fatalf("env %v, volumes %v: want the socket under the data dir", c.Env, c.Volumes)
	}
	if c.Labels[LabelSpec] != c.Hash() || c.Labels[LabelRole] != "registry" {
		t.Fatalf("labels %v", c.Labels)
	}
	args := strings.Join(c.RunArgs(), " ")
	if !strings.Contains(args, "--network none") || strings.Contains(args, " -p ") {
		t.Fatalf("run args %v", c.RunArgs())
	}
}
