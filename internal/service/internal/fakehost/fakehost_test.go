package fakehost

import (
	"context"
	"errors"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
)

func TestStateSurvivesReload(t *testing.T) {
	ctx, dir := context.Background(), t.TempDir()
	d, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.EnsureNetwork(ctx, "env-a", netip.Prefix{}, nil); !errors.Is(err, docker.ErrNeedsSubnet) {
		t.Fatalf("no subnet: %v", err)
	}
	if err := d.EnsureNetwork(ctx, "env-a", netip.MustParsePrefix("10.213.0.0/24"), nil); err != nil {
		t.Fatal(err)
	}
	if err := d.EnsureNetwork(ctx, "env-b", netip.MustParsePrefix("10.213.0.0/24"), nil); !errors.Is(err, docker.ErrSubnetTaken) {
		t.Fatalf("taken subnet: %v", err)
	}
	if used, err := d.UsedSubnets(ctx); err != nil || len(used) != 1 || used[0].String() != "10.213.0.0/24" {
		t.Fatalf("used = %v, %v", used, err)
	}
	id, err := d.Run(ctx, docker.ContainerSpec{Name: "web", Image: "nginx", Networks: []docker.NetAttach{{Name: "env-a"}}})
	if err != nil {
		t.Fatal(err)
	}

	d, err = New(dir) // what hamr dev's restart does
	if err != nil {
		t.Fatal(err)
	}
	det, err := d.Inspect(ctx, "web")
	if err != nil || det.ID != id || !det.Running || det.Networks["env-a"] != "10.213.0.2" {
		t.Fatalf("after reload: %+v %v", det, err)
	}
	if _, err := d.Inspect(ctx, installspec.ProxyName); err != nil {
		t.Fatalf("the proxy stand-in is missing: %v", err)
	}
}

func TestRunExitsAndWaitReturns(t *testing.T) {
	ctx, dir := context.Background(), t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "chaos.json"), []byte(`{"stream_line_ms": 5, "run_fail_rate": 1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	id, err := d.Run(ctx, docker.ContainerSpec{Name: "job", Labels: map[string]string{"stackr.role": "run"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if code, err := d.Wait(ctx, id); code != 1 || err != nil {
		t.Fatalf("Wait = %d, %v; want 1 (run_fail_rate 1)", code, err)
	}
}

func TestBlipFailsThenRecovers(t *testing.T) {
	ctx, dir := context.Background(), t.TempDir()
	d, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "chaos.json")
	if err := os.WriteFile(p, []byte(`{"blip_seconds": 60}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.List(ctx, nil); !errors.Is(err, errDown) {
		t.Fatalf("during the blip: %v", err)
	}
	old := time.Now().Add(-2 * time.Minute) // the blip was saved two minutes ago
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := d.List(ctx, nil); err != nil {
		t.Fatalf("after the blip: %v", err)
	}
}

func TestPinnedDigestsStayApart(t *testing.T) {
	ctx := context.Background()
	d, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, b := "nginx@sha256:aaaa", "nginx@sha256:bbbb"
	for _, ref := range []string{a, b} {
		if err := d.Pull(ctx, ref, "", io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	for _, ref := range []string{a, b} {
		if _, err := d.LocalDigest(ctx, ref); err != nil {
			t.Fatalf("%s after pulling both: %v", ref, err)
		}
	}
}
