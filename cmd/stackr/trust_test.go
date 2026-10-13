package main

import (
	"strings"
	"testing"
)

const trustJSON = `{"fingerprint":"ab12","created_at":"2026-01-02T00:00:00Z","expires_at":"2036-01-02T00:00:00Z",` +
	`"rolling_over":true,"rollover_started_at":"2026-10-01T00:00:00Z","on_old_root":[{"id":"n1","name":"h3"}],` +
	`"break_glass_at":null,"not_rejoined":[{"id":"n2","name":"vm106"}]}`

// trust prints the root, the rollover and the nodes still to rejoin.
func TestClusterTrust(t *testing.T) {
	fake(t, map[string]string{"/admin/cluster-trust": trustJSON})
	code, out, errw := cli(t, "cluster", "trust")
	for _, want := range []string{"ab12", "Rollover running since", "on the old root: h3", "Not rejoined: vm106"} {
		if code != 0 || !strings.Contains(out, want) {
			t.Fatalf("trust = %d, no %q in %q %q", code, want, out, errw)
		}
	}
}

// rotate-root and revoke-controller-cert ask first; -y sends one POST.
func TestClusterRotateRootAndRevoke(t *testing.T) {
	for _, c := range []struct {
		args      []string
		path, say string
	}{
		{[]string{"rotate-root"}, "rotate-root", "Rotating the root"},
		{[]string{"rotate-root", "--finish"}, "finish-rollover", "Rollover finished"},
		{[]string{"revoke-controller-cert"}, "revoke-controller-cert", "new certificate"},
	} {
		r := fake(t, map[string]string{})
		if code, _, _ := cli(t, append([]string{"cluster"}, c.args...)...); code != 3 || len(writes(r)) != 0 {
			t.Errorf("%v without -y = %d, writes %v", c.args, code, writes(r))
		}
		code, out, errw := cli(t, append([]string{"-y", "cluster"}, c.args...)...)
		if w := writes(r); code != 0 || !strings.Contains(out, c.say) || len(w) != 1 || !strings.HasPrefix(w[0], "POST /api/v1/admin/cluster-trust/"+c.path) {
			t.Errorf("%v -y = %d %q %q, writes %v", c.args, code, out, errw, w)
		}
	}
}

// break-glass states the gap in its question, then lists the nodes to rejoin.
func TestClusterBreakGlass(t *testing.T) {
	r := fake(t, map[string]string{"/admin/cluster-trust": trustJSON})
	code, _, errw := cli(t, "cluster", "break-glass")
	for _, want := range []string{"Tunnels between nodes stop until each node rejoins", "Apps keep running", "Deploys and traffic between nodes stop"} {
		if code != 3 || !strings.Contains(errw, want) {
			t.Fatalf("question = %d, no %q in %q", code, want, errw)
		}
	}
	if len(writes(r)) != 0 {
		t.Fatalf("wrote without -y: %v", writes(r))
	}
	code, out, errw := cli(t, "-y", "cluster", "break-glass")
	if w := writes(r); code != 0 || len(w) != 1 || !strings.HasPrefix(w[0], "POST /api/v1/admin/cluster-trust/break-glass") ||
		!strings.Contains(out, "Nodes not rejoined: vm106") || !strings.Contains(out, "stackr node rejoin") {
		t.Errorf("-y = %d %q %q, writes %v", code, out, errw, w)
	}
}
