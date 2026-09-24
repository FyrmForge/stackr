package traffic_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/traffic"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	ltraffic "github.com/FyrmForge/stackr/internal/service/internal/leaf/traffic"
)

func managed(labels map[string]string) map[string]string {
	labels[docker.LabelManaged] = "true"
	return labels
}

// fake docker + fixture conntrack: web dials db's VIP (the pause container),
// the reply comes from db's replica; an exited container and ssh to the
// host are nobody's.
func world() (*traffic.Flow, *dockerfake.Fake, *time.Time) {
	f := dockerfake.New()
	f.Containers = []docker.Container{
		{ID: "w1", State: "running", IPs: []string{"172.20.0.3"}, Labels: managed(map[string]string{tile.LabelTile: "web", tile.LabelRole: "replica"})},
		{ID: "dp", State: "running", IPs: []string{"172.20.0.2"}, Labels: managed(map[string]string{tile.LabelTile: "db", tile.LabelRole: "pause"})},
		{ID: "d1", State: "running", IPs: []string{"172.20.0.4"}, Labels: managed(map[string]string{tile.LabelTile: "db", tile.LabelRole: "replica"})},
		{ID: "old", State: "exited", IPs: []string{"172.20.0.9"}, Labels: managed(map[string]string{tile.LabelTile: "gone"})},
	}
	now := time.Unix(1000, 0)
	fl := &traffic.Flow{Tiles: tile.New(nil, f, nil), Traffic: ltraffic.New(),
		Path: filepath.Join("testdata", "tick1"), Now: func() time.Time { return now }}
	return fl, f, &now
}

func TestTick(t *testing.T) {
	ctx := context.Background()
	fl, _, now := world()
	if err := fl.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	fl.Path, *now = filepath.Join("testdata", "tick2"), now.Add(5*time.Second)
	if err := fl.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	got := fl.Traffic.Snapshot()
	want := map[ltraffic.Pair]float64{{From: "web", To: "db"}: 1000, {From: "db", To: "web"}: 10000}
	if len(got) != len(want) {
		t.Fatalf("snapshot = %v, want %v", got, want)
	}
	for p, v := range want {
		if got[p] != v {
			t.Errorf("%v = %v, want %v", p, got[p], v)
		}
	}
}

// No table: nothing sampled and Docker never asked.
func TestTickNoTable(t *testing.T) {
	fl, f, _ := world()
	fl.Path = filepath.Join(t.TempDir(), "none")
	if err := fl.Tick(context.Background()); err != nil || len(f.Calls()) != 0 || fl.Traffic.Seq() != 0 {
		t.Errorf("err %v, calls %v, seq %d", err, f.Calls(), fl.Traffic.Seq())
	}
}

func TestCheck(t *testing.T) {
	for path, want := range map[string]string{
		filepath.Join("testdata", "tick1"):  "",
		filepath.Join("testdata", "noacct"): "nf_conntrack_acct=1",
		filepath.Join("testdata", "none"):   "no conntrack table",
	} {
		if got := traffic.Check(path); (want == "") != (got == "") || !strings.Contains(got, want) {
			t.Errorf("Check(%s) = %q, want %q", path, got, want)
		}
	}
}
