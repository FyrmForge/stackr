package upgrade

import (
	"context"
	"slices"
	"testing"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
)

// A box with no registry gets one on its socket; a current one is left alone;
// a stale one is replaced.
func TestEnsureRegistry(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cur := installspec.Registry(dir).Labels
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
		f := proxyFlow(fake)
		f.DataDir = dir
		if err := f.EnsureRegistry(ctx); err != nil {
			t.Fatal(tc.name, err)
		}
		if got := len(fake.Specs) == 1; got != tc.wantRun {
			t.Errorf("%s: ran = %v", tc.name, got)
		}
		// No network, only the socket: on the host's network a host-network
		// tile would share its loopback, on a bridge every container there.
		if s := fake.Specs; tc.wantRun && (s[0].HostNetwork || len(s[0].Ports) != 0 || len(s[0].Networks) != 1 || s[0].Networks[0].Name != "none" || !slices.Contains(s[0].Env, "REGISTRY_HTTP_NET=unix")) {
			t.Errorf("%s: host network %v, ports %v, networks %v, env %v", tc.name, s[0].HostNetwork, s[0].Ports, s[0].Networks, s[0].Env)
		}
	}
}
