package service

import (
	"context"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
)

// A host-network tile on a worker is dialled at the worker's mesh address,
// never the panel's own loopback.
func TestHostNetAddr(t *testing.T) {
	o, _ := renewRig(t)
	ctx := context.Background()
	got, err := o.hostNetAddr(docker.WithNode(ctx, "w1"))
	if err != nil || got != "10.207.255.2" {
		t.Fatalf("worker = %q, %v", got, err)
	}
	if got, _ := o.hostNetAddr(ctx); got != "127.0.0.1" {
		t.Fatalf("no node = %q", got)
	}
}
