package installspec

import (
	"slices"
	"strings"
	"testing"
)

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
