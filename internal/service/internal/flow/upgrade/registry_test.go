package upgrade

import (
	"context"
	"slices"
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
		// On the host's network, bound to its loopback by env: on a bridge it
		// would be open to every container there, a build step included.
		if s := fake.Specs; tc.wantRun && (!s[0].HostNetwork || len(s[0].Ports) != 0 || !slices.Contains(s[0].Env, "REGISTRY_HTTP_ADDR=127.0.0.1:5000")) {
			t.Errorf("%s: host network %v, ports %v, env %v", tc.name, s[0].HostNetwork, s[0].Ports, s[0].Env)
		}
	}
}
