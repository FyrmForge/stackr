package upgrade

import (
	"context"
	"testing"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/panel"
)

// A box with no observe container gets one; a current running one is left
// alone; a stale one (new version or knob) is replaced after the pull; a dev
// build touches nothing. Remove keeps the volume.
func TestEnsureObserve(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cur := installspec.Observe("v0.8.3", dir, "30d", 5<<30).Labels
	for _, tc := range []struct {
		name    string
		version string
		have    map[string]string
		wantRun bool
	}{
		{"none", "v0.8.3", nil, true},
		{"stale", "v0.8.3", map[string]string{installspec.LabelRole: "observe", installspec.LabelSpec: "old"}, true},
		{"current", "v0.8.3", cur, false},
		{"dev", "dev", nil, false},
	} {
		fake := dockerfake.New()
		if tc.have != nil {
			fake.Containers = []docker.Container{{ID: "old", State: "running", Labels: tc.have}}
		}
		f := &Flow{Docker: fake, Panel: panel.New(fake), DataDir: dir, Version: tc.version}
		if err := f.EnsureObserve(ctx, "30d", 5<<30); err != nil {
			t.Fatal(tc.name, err)
		}
		if got := len(fake.Specs) == 1; got != tc.wantRun {
			t.Fatalf("%s: ran = %v", tc.name, got)
		}
		if !tc.wantRun {
			continue
		}
		// No network, only the socket: on loopback a host-network tile would reach it.
		s := fake.Specs[0]
		if s.HostNetwork || len(s.Ports) != 0 || len(s.Networks) != 1 || s.Networks[0].Name != "none" {
			t.Errorf("%s: host network %v, ports %v, networks %v", tc.name, s.HostNetwork, s.Ports, s.Networks)
		}
		if s.Image != "ghcr.io/fyrmforge/stackr-observe:0.8.3" {
			t.Errorf("%s: image %s", tc.name, s.Image)
		}
	}
}
