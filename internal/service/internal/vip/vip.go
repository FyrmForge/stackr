// Package vip keeps the iptables DNAT rules that turn a virtual IP (a pause
// container's address) into its live replica IPs. IPs in, rules out: it knows
// nothing about tiles. The rules live in stackr-owned chains: STACKR-VIP in
// the nat table, STACKR-FWD and STACKR-IN in filter (filter.go), so nothing
// else's rules are touched.
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
	vips map[string]Entry
	base Base
	// early are replicas at their health gate: their lan rules exist before
	// the VIP routes to them (Allow); apply drops one once a VIP lists it.
	early map[string][]string
	// Restore applies a restore script, Exec runs an argv, Read runs one and
	// returns its output; tests swap all three.
	Restore func(ctx context.Context, script string) error
	Exec    func(ctx context.Context, argv ...string) error
	Read    func(ctx context.Context, argv ...string) (string, error)
}

func New() *Table {
	return &Table{vips: map[string]Entry{}, Restore: restore, Exec: run, Read: read}
}

// Set rewrites the rules for one VIP; lan are the tile's lan grant lines.
func (t *Table) Set(ctx context.Context, vip string, replicas, lan []string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	next := clone(t.vips)
	next[vip] = Entry{slices.Clone(replicas), slices.Clone(lan)}
	return t.apply(ctx, next, t.base)
}

// Allow opens a tile's lan grants to one replica address before the VIP
// routes to it, so an app that needs the LAN at start passes its health gate.
// No lan clears it. Route's Set takes over once the replica is routed.
func (t *Table) Allow(ctx context.Context, ip string, lan []string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if a, err := netip.ParseAddr(ip); err != nil || !a.Is4() {
		return fmt.Errorf("vip: %q is not an IPv4 address", ip)
	}
	old, had := t.early[ip]
	if len(lan) == 0 && !had {
		return nil
	}
	if t.early == nil {
		t.early = map[string][]string{}
	}
	if len(lan) == 0 {
		delete(t.early, ip)
	} else {
		t.early[ip] = slices.Clone(lan)
	}
	if err := t.apply(ctx, clone(t.vips), t.base); err != nil {
		if had {
			t.early[ip] = old
		} else {
			delete(t.early, ip)
		}
		return err
	}
	return nil
}

// Legacy reports a host whose iptables lacks DOCKER-USER while Docker runs:
// the legacy backend. The drops still hold; the hairpin ACCEPT cannot skip
// Docker's isolation chains there.
func (t *Table) Legacy(ctx context.Context) bool {
	return t.Exec(ctx, "iptables", "-S", "DOCKER-USER") != nil
}

// Remove drops one VIP's rules.
func (t *Table) Remove(ctx context.Context, vip string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	next := clone(t.vips)
	delete(next, vip)
	return t.apply(ctx, next, t.base)
}

// Rebuild replaces every rule with all and the filter inputs with b, in one
// apply (stackrd calls it on start).
func (t *Table) Rebuild(ctx context.Context, all map[string]Entry, b Base) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.apply(ctx, clone(all), b)
}

// Merge rewrites the rules of every VIP in all and keeps the rest as they
// are, with b as the filter inputs: a rebuild that could not read every tile
// must not drop the ones it could not read. drop names the VIPs that are
// known to be gone (a deleted tile, a tile with no replicas): they go even
// though the rebuild is partial.
func (t *Table) Merge(ctx context.Context, all map[string]Entry, drop []string, b Base) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	next := clone(t.vips)
	for _, v := range drop {
		delete(next, v)
	}
	maps.Copy(next, clone(all))
	return t.apply(ctx, next, b)
}

func (t *Table) apply(ctx context.Context, next map[string]Entry, base Base) error {
	// The script is text: an address that is not one must never reach it.
	for v, e := range next {
		for _, ip := range append([]string{v}, e.Replicas...) {
			if a, err := netip.ParseAddr(ip); err != nil || !a.Is4() {
				return fmt.Errorf("vip: %q is not an IPv4 address", ip)
			}
		}
		for _, l := range e.Lan {
			if _, err := parseLan(l); err != nil {
				return err
			}
		}
	}
	// Replicas a VIP routes to are no longer early.
	early := maps.Clone(t.early)
	if early == nil {
		early = map[string][]string{}
	}
	for _, e := range next {
		for _, ip := range e.Replicas {
			delete(early, ip)
		}
	}
	for _, l := range early {
		for _, line := range l {
			if _, err := parseLan(line); err != nil {
				return err
			}
		}
	}
	if err := base.check(); err != nil {
		return err
	}
	withEarly := base
	withEarly.early = early
	if err := t.Restore(ctx, Render(t.vips, next, withEarly)); err != nil {
		return err
	}
	t.vips, t.base, t.early = next, base, early
	for _, hook := range []string{"PREROUTING", "OUTPUT"} {
		// -C fails when the jump is missing; only then insert it.
		if t.Exec(ctx, "iptables", "-t", "nat", "-C", hook, "-j", Chain) != nil {
			if err := t.Exec(ctx, "iptables", "-t", "nat", "-I", hook, "-j", Chain); err != nil {
				return err
			}
		}
	}
	if !base.Range.IsValid() {
		return nil
	}
	// FWD hangs off DOCKER-USER, which Docker leaves alone on a restart; a
	// host without it falls back to FORWARD.
	fwd := "FORWARD"
	if t.Exec(ctx, "iptables", "-S", "DOCKER-USER") == nil {
		fwd = "DOCKER-USER"
		// Delete until -D fails: the jump an older build left in FORWARD goes.
		for t.Exec(ctx, "iptables", "-D", "FORWARD", "-j", FwdChain) == nil {
		}
	}
	for _, h := range [][2]string{{fwd, FwdChain}, {"INPUT", InChain}} {
		hook, chain := h[0], h[1]
		// Docker puts its own jumps on top at every daemon start: ours must
		// be rule 1, so anything else is removed and re-inserted there.
		out, err := t.Read(ctx, "iptables", "-S", hook, "1")
		if err == nil && strings.TrimSpace(out) == "-A "+hook+" -j "+chain {
			continue
		}
		// Delete until -D fails: a duplicate jump must not survive.
		for t.Exec(ctx, "iptables", "-D", hook, "-j", chain) == nil {
		}
		if err := t.Exec(ctx, "iptables", "-I", hook, "1", "-j", chain); err != nil {
			return err
		}
	}
	return nil
}

// Render is the iptables-restore --noflush script taking the rules from prev
// to next. Declaring a chain flushes it, so Chain and every VIP chain in next
// are rewritten whole; chains only in prev are flushed and deleted after the
// jumps to them are gone. Replicas share a VIP's traffic evenly: rule i of n
// takes 1/(n-i) of what is left (DECIDE 20).
func Render(prev, next map[string]Entry, base Base) string {
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
		ips := next[v].Replicas
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
	if base.Range.IsValid() {
		b.WriteString(renderFilter(next, base))
	}
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

func read(ctx context.Context, argv ...string) (string, error) {
	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).Output()
	return string(out), err
}

func clone(m map[string]Entry) map[string]Entry {
	out := make(map[string]Entry, len(m))
	for k, v := range m {
		out[k] = Entry{slices.Clone(v.Replicas), slices.Clone(v.Lan)}
	}
	return out
}
