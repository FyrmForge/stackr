package dockerfake

import (
	"context"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
)

func TestVolumeDeviceAndFirstOptsWin(t *testing.T) {
	ctx, f := context.Background(), &Fake{}
	if found, dev, _ := f.VolumeDevice(ctx, "v"); found || dev != "" {
		t.Fatalf("missing: %v %q", found, dev)
	}
	_ = f.CreateVolume(ctx, "plain", "local", nil, nil)
	if found, dev, _ := f.VolumeDevice(ctx, "plain"); !found || dev != "" {
		t.Fatalf("plain: %v %q", found, dev)
	}
	_ = f.CreateVolume(ctx, "b", "local", docker.BindOpts("/mnt/d/b"), nil)
	_ = f.CreateVolume(ctx, "b", "local", docker.BindOpts("/mnt/other"), nil)
	if found, dev, _ := f.VolumeDevice(ctx, "b"); !found || dev != "/mnt/d/b" {
		t.Fatalf("bound: %v %q", found, dev)
	}
	if len(f.Created) != 3 || f.Created[2].Opts["device"] != "/mnt/other" {
		t.Fatalf("every call is recorded: %+v", f.Created)
	}
	_ = f.RemoveVolume(ctx, "b")
	if found, _, _ := f.VolumeDevice(ctx, "b"); found {
		t.Fatal("removed volume still found")
	}
}
