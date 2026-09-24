// Package vip keeps the iptables DNAT rules that turn a virtual IP (a pause
// container's address) into its live replica IPs. IPs in, rules out: it knows
// nothing about tiles. Everything lives in stackr-owned chains in the nat
// table, so nothing else's rules are touched.
package vip

import (
	"context"
	"fmt"
	"maps"
	"net/netip"
	"os/exec"
	"slices"
	"strings"
	"sync"
)

// Chain is the one chain PREROUTING and OUTPUT jump to; each VIP gets its own
// sub-chain under it.
const Chain = "STACKR-VIP"

func vipChain(ip string) string { return "STKR-" + ip } // <= 20 chars, iptables allows 28

// Table is the full VIP → replicas map, applied as one atomic
// iptables-restore on every change.
type Table struct {
	mu   sync.Mutex
	vips map[string][]string
	// Restore applies a restore script, Exec runs an argv; tests swap both.
	Restore func(ctx context.Context, script string) error
	Exec    func(ctx context.Context, argv ...string) error
}

func New() *Table {
	return &Table{vips: map[string][]string{}, Restore: restore, Exec: run}
}

// Set rewrites the rules for one VIP.
func (t *Table) Set(ctx context.Context, vip string, replicas []string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	next := clone(t.vips)
	next[vip] = slices.Clone(replicas)
	return t.apply(ctx, next)
}

// Remove drops one VIP's rules.
func (t *Table) Remove(ctx context.Context, vip string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	next := clone(t.vips)
	delete(next, vip)
	return t.apply(ctx, next)
}

// Rebuild replaces every rule with all (stackrd calls it on start).
func (t *Table) Rebuild(ctx context.Context, all map[string][]string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.apply(ctx, clone(all))
}

func (t *Table) apply(ctx context.Context, next map[string][]string) error {
	// The script is text: an address that is not one must never reach it.
	for v, ips := range next {
		for _, ip := range append([]string{v}, ips...) {
			if a, err := netip.ParseAddr(ip); err != nil || !a.Is4() {
				return fmt.Errorf("vip: %q is not an IPv4 address", ip)
			}
		}
	}
	if err := t.Restore(ctx, Render(t.vips, next)); err != nil {
		return err
	}
	t.vips = next
	for _, hook := range []string{"PREROUTING", "OUTPUT"} {
		// -C fails when the jump is missing; only then insert it.
		if t.Exec(ctx, "iptables", "-t", "nat", "-C", hook, "-j", Chain) != nil {
			if err := t.Exec(ctx, "iptables", "-t", "nat", "-I", hook, "-j", Chain); err != nil {
				return err
			}
		}
	}
	return nil
}

// Render is the iptables-restore --noflush script taking the rules from prev
// to next. Declaring a chain flushes it, so Chain and every VIP chain in next
// are rewritten whole; chains only in prev are flushed and deleted after the
// jumps to them are gone. Replicas share a VIP's traffic evenly: rule i of n
// takes 1/(n-i) of what is left (DECIDE 20).
func Render(prev, next map[string][]string) string {
	var b strings.Builder
	b.WriteString("*nat\n:" + Chain + " - [0:0]\n")
	vips := slices.Sorted(maps.Keys(next))
	var gone []string
	for _, v := range slices.Sorted(maps.Keys(prev)) {
		if _, ok := next[v]; !ok {
			gone = append(gone, v)
		}
	}
	for _, v := range append(slices.Clone(vips), gone...) {
		b.WriteString(":" + vipChain(v) + " - [0:0]\n")
	}
	for _, v := range vips {
		fmt.Fprintf(&b, "-A %s -d %s/32 -j %s\n", Chain, v, vipChain(v))
		ips := next[v]
		for i, ip := range ips {
			if left := len(ips) - i; left > 1 {
				fmt.Fprintf(&b, "-A %s -m statistic --mode random --probability %.5f -j DNAT --to-destination %s\n",
					vipChain(v), 1/float64(left), ip)
			} else {
				fmt.Fprintf(&b, "-A %s -j DNAT --to-destination %s\n", vipChain(v), ip)
			}
		}
	}
	for _, v := range gone {
		b.WriteString("-X " + vipChain(v) + "\n")
	}
	b.WriteString("COMMIT\n")
	return b.String()
}

func restore(ctx context.Context, script string) error {
	cmd := exec.CommandContext(ctx, "iptables-restore", "--noflush")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("iptables-restore: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func run(ctx context.Context, argv ...string) error {
	if out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func clone(m map[string][]string) map[string][]string {
	out := make(map[string][]string, len(m))
	for k, v := range m {
		out[k] = slices.Clone(v)
	}
	return out
}
