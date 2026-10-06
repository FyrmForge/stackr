package service

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/graph"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// The sync overlay tags the matching cards and draws the rows a sync would
// create as static ghosts; with no overlay the canvas is what it was.
func TestCanvasSyncOverlay(t *testing.T) {
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
	plain, err := orch.graph.Build(ctx, s, graph.In{Show: graph.All})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range plain.Nodes {
		if n.Sync != "" {
			t.Errorf("no overlay: node %s tagged %q", n.ID, n.Sync)
		}
	}

	ghost := store.Tile{
		StackID:       st.ID,
		EnvironmentID: en.ID,
		Name:          "worker",
		Slug:          "worker",
		Kind:          "image",
		ImageRef:      "nginx:1",
	}
	v, err := orch.graph.Build(ctx, s, graph.In{
		Show: graph.All,
		Sync: map[string]string{"api": "edited"},
		New:  []store.Tile{ghost},
	})
	if err != nil {
		t.Fatal(err)
	}
	var api, gh *graph.Node
	for i := range v.Nodes {
		switch v.Nodes[i].ID {
		case tl.ID:
			api = &v.Nodes[i]
		case "sync:worker":
			gh = &v.Nodes[i]
		}
	}
	if api == nil || api.Sync != "edited" {
		t.Errorf("api card: %+v, want tag edited", api)
	}
	if gh == nil || gh.Sync != "new" || !gh.Static || (gh.X == 0 && gh.Y == 0) {
		t.Errorf("ghost: %+v, want new, static and placed", gh)
	}
	if len(v.Nodes) != len(plain.Nodes)+1 || !reflect.DeepEqual(v.Edges, plain.Edges) {
		t.Errorf("overlay: %d nodes, edges %+v; want one ghost and the same edges as %d nodes, %+v",
			len(v.Nodes), v.Edges, len(plain.Nodes), plain.Edges)
	}

	// A slice ghost has no row behind it: the canvas still builds.
	slice := ghost
	slice.Name, slice.Slug, slice.Kind, slice.ImageRef = "db", "db", "slice", ""
	if _, err := orch.graph.Build(ctx, s, graph.In{Show: graph.All, New: []store.Tile{slice}}); err != nil {
		t.Errorf("slice ghost: %v", err)
	}
}
