package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/graph"
	ltraffic "github.com/FyrmForge/stackr/internal/service/internal/leaf/traffic"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Egress comes from the last traffic sample: a tile that talked to the
// internet gets an edge to the internet system card; Traffic off drops both.
func TestCanvasEgress(t *testing.T) {
	ctx := context.Background()
	orch, err := New(
		Config{DataDir: t.TempDir(), SecretsKey: testKey, Conntrack: "/nonexistent"},
		WithDocker(dockerfake.New()),
		WithVIP(vipStub{}),
		WithProxy(func(context.Context, json.RawMessage) error { return nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = orch.Close() })
	og := store.Org{
		ID:        "o1",
		Name:      "acme",
		Slug:      "acme",
		EnvColors: "{}",
		Settings:  "{}",
		CreatedAt: time.Now(),
	}
	if err := orch.store.Orgs.Create(ctx, og); err != nil {
		t.Fatal(err)
	}
	st, err := orch.CreateStack(ctx, og.ID, "shop", "")
	if err != nil {
		t.Fatal(err)
	}
	en, err := orch.CreateEnv(ctx, st.ID, "dev", EnvSpec{Type: "static", FromKind: "branch", FromBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	tl, err := orch.CreateTile(ctx, Tile{
		StackID:       st.ID,
		EnvironmentID: en.ID,
		Name:          "api",
		Kind:          "image",
		ImageRef:      "nginx:1",
	})
	if err != nil {
		t.Fatal(err)
	}
	s := CanvasScope{Kind: CanvasEnv, ID: en.ID}
	lanes := []ltraffic.Edge{
		{From: tl.ID, To: ltraffic.Internet, BPS: 10},
		{From: tl.ID, To: ltraffic.Internet, BPS: 5},
	}
	v, err := orch.graph.Build(ctx, s, graph.In{Show: graph.All, Traffic: lanes})
	if err != nil {
		t.Fatal(err)
	}
	var sys bool
	for _, n := range v.Nodes {
		sys = sys || (n.ID == "internet" && n.System)
	}
	if !sys || len(v.Edges) != 1 || v.Edges[0] != (graph.Edge{Kind: "egress", From: tl.ID, To: "internet"}) ||
		v.Divider == 0 {
		t.Errorf("egress: nodes %+v edges %+v divider %d", v.Nodes, v.Edges, v.Divider)
	}
	off := graph.All
	off.Traffic = false
	v, _ = orch.graph.Build(ctx, s, graph.In{Show: off, Traffic: lanes})
	if len(v.Edges) != 0 || len(v.Nodes) != 2 || v.Divider != 0 {
		t.Errorf("traffic off: nodes %+v edges %+v divider %d, want api and vars only", v.Nodes, v.Edges, v.Divider)
	}
}
