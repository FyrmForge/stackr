package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/imgcopy"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/registry"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

func TestStackrSystemRef(t *testing.T) {
	for in, want := range map[string]string{
		"ghcr.io/fyrmforge/stackr:0.6.0":    "localhost:5000/stackr.system/stackr:0.6.0",
		"localhost:5000/x/stackr:0.6.0-dev": "localhost:5000/stackr.system/stackr:0.6.0-dev",
		"stackr":                            "localhost:5000/stackr.system/stackr:latest",
	} {
		if got := stackrSystemRef(in); got != want {
			t.Errorf("stackrSystemRef(%q) = %q, want %q", in, got, want)
		}
	}
}

// The nodes' refs name stackr.system on the node's own loopback registry.
func TestSystemRefsPointAtRegistry(t *testing.T) {
	for _, ref := range []string{tile.PauseImage, docker.ToolImage} {
		if !strings.HasPrefix(ref, "localhost:5000/stackr.system/") {
			t.Errorf("%s is not a stackr.system ref", ref)
		}
	}
}

func fakePublisher(rows *[]store.RegistryImage, copies *[][2]string) systemPublisher {
	return systemPublisher{
		platforms: func(context.Context) []string { return []string{"amd64"} },
		copy: func(_ context.Context, src, dst string, plat []string) (imgcopy.Result, error) {
			*copies = append(*copies, [2]string{src, dst})
			return imgcopy.Result{Digest: "sha256:" + src, Platforms: plat, Size: 7}, nil
		},
		pushLocal: func(_ context.Context, src, dst string) (imgcopy.Result, error) {
			*copies = append(*copies, [2]string{src, dst})
			return imgcopy.Result{Digest: "sha256:local", Platforms: []string{"amd64"}, Size: 9}, nil
		},
		record:      func(_ context.Context, r store.RegistryImage) error { *rows = append(*rows, r); return nil },
		stackrImage: func(context.Context) (string, error) { return "ghcr.io/fyrmforge/stackr:0.6.0", nil },
	}
}

func TestPublishSystemImages(t *testing.T) {
	var rows []store.RegistryImage
	var copies [][2]string
	p := fakePublisher(&rows, &copies)
	published := 0
	p.published = func() { published = len(copies) }
	if err := p.publish(context.Background()); err != nil {
		t.Fatal(err)
	}
	if published != 1 {
		t.Errorf("published after %d copies, want right after the stackr image", published)
	}
	// The stackr image first: ingress workers wait on it.
	want := [][2]string{
		{"ghcr.io/fyrmforge/stackr:0.6.0", "localhost:5000/stackr.system/stackr:0.6.0"},
		{"alpine:3", "localhost:5000/stackr.system/alpine:3"},
		{"registry.k8s.io/pause:3.10", "localhost:5000/stackr.system/pause:3.10"},
	}
	if len(copies) != len(want) {
		t.Fatalf("copies = %v", copies)
	}
	for i := range want {
		if copies[i] != want[i] {
			t.Errorf("copy %d = %v, want %v", i, copies[i], want[i])
		}
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d", len(rows))
	}
	for _, r := range rows {
		if r.Source != registry.SourceSystem || r.OrgID != nil || !strings.HasPrefix(r.Repo, "stackr.system/") || r.Digest == "" {
			t.Errorf("row = %+v", r)
		}
	}
	if rows[1].Repo != "stackr.system/alpine" || rows[1].Tag != "3" || rows[1].UpstreamRef != "alpine:3" {
		t.Errorf("alpine row = %+v", rows[1])
	}
	if rows[0].Repo != "stackr.system/stackr" || rows[0].Tag != "0.6.0" {
		t.Errorf("stackr row = %+v", rows[0])
	}
}

// A proxy pull waits for the boot publish, up to its deadline.
func TestWaitStackrPublished(t *testing.T) {
	o := &Orchestrator{stackrPublished: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := o.waitStackrPublished(ctx); err == nil {
		t.Error("did not wait for the publish")
	}
	o.markStackrPublished()
	o.markStackrPublished() // twice is fine
	if err := o.waitStackrPublished(context.Background()); err != nil {
		t.Errorf("after the publish: %v", err)
	}
}

// One image failing does not stop the others; the error names it.
func TestPublishSystemImagesKeepsGoing(t *testing.T) {
	var rows []store.RegistryImage
	var copies [][2]string
	p := fakePublisher(&rows, &copies)
	p.copy = func(_ context.Context, src, _ string, _ []string) (imgcopy.Result, error) {
		if src == "alpine:3" {
			return imgcopy.Result{}, errors.New("hub down")
		}
		return imgcopy.Result{Digest: "sha256:x"}, nil
	}
	err := p.publish(context.Background())
	if err == nil || !strings.Contains(err.Error(), "alpine:3") {
		t.Fatalf("err = %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want the other two", len(rows))
	}
}
