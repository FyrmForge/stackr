package service

import (
	"context"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
)

// The map's controller card carries the daemon's docker version and the arch.
func TestClusterMapControllerFacts(t *testing.T) {
	n := newTwoNodes(t)
	n.ctlFake.Host = docker.HostInfo{CPUs: 4, MemBytes: 8 << 30, Version: "29.9.0"}
	m, err := n.orch.ClusterMap(context.Background())
	must(t, err)
	if c := m.Nodes[0]; c.Role != node.Controller || c.Docker != "29.9.0" || c.Arch == "" {
		t.Fatalf("controller = %+v", c)
	}
}
