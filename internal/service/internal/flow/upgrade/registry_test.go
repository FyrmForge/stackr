package upgrade

import (
	"context"
	"testing"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
)

// A box with no registry gets one on loopback; a current one is left alone;
// a stale one is replaced.
func TestEnsureRegistry(t *testing.T) {
	ctx := context.Background()
	cur := installspec.Registry().Labels
	for _, tc := range []struct {
		name    string
		have    map[string]string
		wantRun bool
	}{
		{"none", nil, true},
		{"stale", map[string]string{installspec.LabelRole: "registry", installspec.LabelSpec: "old"}, true},
		{"current", cur, false},
	} {
		fake := dockerfake.New()
		if tc.have != nil {
			fake.Containers = []docker.Container{{ID: "old", Labels: tc.have}}
		}
		if err := proxyFlow(fake).EnsureRegistry(ctx); err != nil {
			t.Fatal(tc.name, err)
		}
		if got := len(fake.Specs) == 1; got != tc.wantRun {
			t.Errorf("%s: ran = %v", tc.name, got)
		}
		if tc.wantRun && fake.Specs[0].Ports["127.0.0.1:5000"] != "5000" {
			t.Errorf("%s: ports %v", tc.name, fake.Specs[0].Ports)
		}
	}
}
