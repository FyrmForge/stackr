package service

import (
	"context"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/worker"
)

// A node's report alone raises and clears the wide-prefix problem, no network
// reservation needed; the text carries the masked prefix.
func TestPoolWidePrefixReportClears(t *testing.T) {
	o := newClusterPanel(t)
	ctx := context.Background()
	m := newMeshSvc(o.store, func(string) (MeshNode, bool) { return nil, false })
	m.wide = o.wideProblem
	key := "alert.mesh-wide-prefix.w1"
	m.Report(ctx, "w1", worker.Facts{Interfaces: []worker.Iface{{Name: "x", Prefixes: []string{"10.208.250.1/23"}}}})
	row, _, _ := o.store.Settings.Get(ctx, problemKey(key))
	if !o.hasProblem(ctx, key) || !strings.Contains(row, "10.208.250.0/23") {
		t.Fatalf("problem = %v %s", o.hasProblem(ctx, key), row)
	}
	m.Report(ctx, "w1", worker.Facts{})
	if o.hasProblem(ctx, key) {
		t.Fatal("problem not cleared by the next report")
	}
}
