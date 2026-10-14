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

// The registry checks nothing, so no container may reach it: it runs on the
// host's network and listens on the host's loopback only. A published port on
// a bridge would leave it open to every container on that bridge, a build
// step included.
func TestRegistryLocalOnly(t *testing.T) {
	c := Registry()
	if !c.HostNetwork || len(c.Ports) != 0 {
		t.Fatalf("host network %v, ports %v: want the host's network and no published port", c.HostNetwork, c.Ports)
	}
	if !slices.Contains(c.Env, "REGISTRY_HTTP_ADDR=127.0.0.1:"+RegistryPort) {
		t.Fatalf("env %v does not bind the loopback", c.Env)
	}
	if c.Labels[LabelSpec] != c.Hash() || c.Labels[LabelRole] != "registry" {
		t.Fatalf("labels %v", c.Labels)
	}
	args := strings.Join(c.RunArgs(), " ")
	if !strings.Contains(args, "--network host") || strings.Contains(args, " -p ") {
		t.Fatalf("run args %v", c.RunArgs())
	}
}
