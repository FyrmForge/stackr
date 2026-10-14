package service

import (
	"encoding/json"
	"strings"
	"testing"
)

// A member of org A must not see org B's names, ranges or node fingerprints
// through the node and cluster views.
func TestRedactedHidesOtherOrgs(t *testing.T) {
	n := NodeOut{
		Name: "w1", OpenTo: []string{"orgb"}, DefaultExit: []string{"exitb"},
		Range: "10.9.0.0/24", WGAddress: "10.9.0.1", Version: "v0.6.9", Arch: "arm64",
		Docker: "27.1", Behind: true, CPU: 8, MemTotal: 1, DiskFree: 2, DiskTotal: 3,
	}
	info := NodeInfo{
		NodeOut:    n,
		EnvList:    []NodeEnvOut{{Path: "orgb/shop/prod", Org: "orgb", Volumes: []NodeVolume{{Name: "orgb-data"}}}},
		ExitFor:    []string{"orgb/shop"},
		IngressFor: []string{"orgb/shop/prod"},
	}
	cl := ClusterOut{
		Nodes:  []ClusterStatusNode{{NodeOut: n, Public: []string{"1.2.3.4"}}},
		Groups: []ClusterGroup{{Name: "internet", Nodes: []string{"w1"}}, {Name: "192.168.7.0/24", Nodes: []string{"w1"}}},
	}
	for _, v := range []any{n.Redacted(), info.Redacted(), cl.Redacted()} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"orgb", "exitb", "10.9.", "v0.6.9", "arm64", "27.1", "192.168", "1.2.3.4", `"behind":true`, `"disk_total":3`} {
			if strings.Contains(string(b), bad) {
				t.Errorf("%T leaks %q: %s", v, bad, b)
			}
		}
	}
	if g := cl.Redacted().Groups; g[0].Name != "internet" {
		t.Errorf("groups = %+v", g)
	}
}
