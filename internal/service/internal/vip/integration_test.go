//go:build integration

package vip

import (
	"context"
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
		_ = tb.Rebuild(ctx, nil)
		for _, hook := range []string{"PREROUTING", "OUTPUT"} {
			_ = run(ctx, "iptables", "-t", "nat", "-D", hook, "-j", Chain)
		}
		_ = run(ctx, "iptables", "-t", "nat", "-X", Chain)
	})

	if err := tb.Set(ctx, "10.99.0.1", []string{"10.99.0.2", "10.99.0.3"}); err != nil {
		t.Fatal(err)
	}
	// A second Set must replace, not append: declaring the chain flushes it.
	if err := tb.Set(ctx, "10.99.0.1", []string{"10.99.0.4"}); err != nil {
		t.Fatal(err)
	}
	if got := rules(vipChain("10.99.0.1")); strings.Contains(got, "10.99.0.2") || !strings.Contains(got, "10.99.0.4") {
		t.Fatalf("chain not replaced:\n%s", got)
	}
	if got := rules("OUTPUT"); strings.Count(got, "-j "+Chain) != 1 {
		t.Fatalf("jump count:\n%s", got)
	}
	if err := tb.Remove(ctx, "10.99.0.1"); err != nil {
		t.Fatal(err)
	}
	if got := rules(vipChain("10.99.0.1")); !strings.Contains(got, "No chain") && !strings.Contains(got, "does not exist") {
		t.Fatalf("chain still there:\n%s", got)
	}
}
