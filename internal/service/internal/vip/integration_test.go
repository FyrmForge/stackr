//go:build integration

package vip

import (
	"context"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Needs root (NET_ADMIN) and edits the real nat table; run it in a throwaway
// netns: go test -c -tags integration -o vip.test ./internal/service/internal/vip/ && unshare -rn ./vip.test
func TestIptables(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root / NET_ADMIN")
	}
	ctx := context.Background()
	rules := func(chain string) string {
		out, _ := exec.Command("iptables", "-t", "nat", "-S", chain).CombinedOutput()
		return string(out)
	}
	tb := New()
	t.Cleanup(func() {
		_ = tb.Rebuild(ctx, nil, Base{})
		for _, hook := range []string{"PREROUTING", "OUTPUT"} {
			_ = run(ctx, "iptables", "-t", "nat", "-D", hook, "-j", Chain)
		}
		_ = run(ctx, "iptables", "-t", "nat", "-X", Chain)
	})

	if err := tb.Set(ctx, "10.99.0.1", []string{"10.99.0.2", "10.99.0.3"}, nil); err != nil {
		t.Fatal(err)
	}
	// A second Set must replace, not append: declaring the chain flushes it.
	if err := tb.Set(ctx, "10.99.0.1", []string{"10.99.0.4"}, nil); err != nil {
		t.Fatal(err)
	}
	if got := rules(vipChain("10.99.0.1")); strings.Contains(got, "10.99.0.2") || !strings.Contains(got, "10.99.0.4") {
		t.Fatalf("chain not replaced:\n%s", got)
	}
	if got := rules("OUTPUT"); strings.Count(got, "-j "+Chain) != 1 {
		t.Fatalf("jump count:\n%s", got)
	}
	// The filter chains: drops after the returns, one jump at position 1
	// even after a second apply and after another rule lands on top.
	base := Base{Range: netip.MustParsePrefix("10.213.0.0/16"), Resolvers: []string{"192.168.1.1"}, ProxyIP: "172.17.0.2", PanelBind: "172.17.0.1", PanelPort: 8080}
	t.Cleanup(func() {
		for _, h := range [][2]string{{"DOCKER-USER", FwdChain}, {"INPUT", InChain}} {
			_ = run(ctx, "iptables", "-D", h[0], "-j", h[1])
			_ = run(ctx, "iptables", "-F", h[1])
			_ = run(ctx, "iptables", "-X", h[1])
		}
	})
	if err := tb.Rebuild(ctx, nil, base); err != nil {
		t.Fatal(err)
	}
	if err := run(ctx, "iptables", "-I", "DOCKER-USER", "1", "-j", "ACCEPT"); err != nil { // a foreign rule on top
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run(ctx, "iptables", "-D", "DOCKER-USER", "-j", "ACCEPT") })
	if err := tb.Rebuild(ctx, nil, base); err != nil { // same base: the jump moves back to 1
		t.Fatal(err)
	}
	out, _ := exec.Command("iptables", "-S", "DOCKER-USER").CombinedOutput()
	if lines := strings.Split(string(out), "\n"); lines[1] != "-A DOCKER-USER -j "+FwdChain || strings.Count(string(out), FwdChain) != 1 {
		t.Fatalf("DOCKER-USER:\n%s", out)
	}
	out, _ = exec.Command("iptables", "-S", FwdChain).CombinedOutput()
	if s := string(out); strings.LastIndex(s, "RETURN") > strings.Index(s, "-d 10.0.0.0/8 -j DROP") {
		t.Fatalf("drops before returns:\n%s", s)
	}

	if err := tb.Remove(ctx, "10.99.0.1"); err != nil {
		t.Fatal(err)
	}
	if got := rules(vipChain("10.99.0.1")); !strings.Contains(got, "No chain") &&
		!strings.Contains(got, "does not exist") {
		t.Fatalf("chain still there:\n%s", got)
	}
}
