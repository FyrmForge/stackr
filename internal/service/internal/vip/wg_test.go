package vip

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
)

// The mesh interface lets in only the exit links and the controller's call to
// the worker API; every other wg0 packet is dropped, after the accepts.
func TestFilterWGInput(t *testing.T) {
	b := testBase
	b.WGIface = "wg0"
	b.WGController = netip.MustParseAddr("10.207.255.1")
	if err := b.check(); err != nil {
		t.Fatal(err)
	}
	s := renderFilter(nil, b)
	want := []string{
		"-A " + InChain + " -i wg0 -p udp --dport 52000:52255 -j RETURN",
		"-A " + InChain + " -i wg0 -s 10.207.255.1/32 -p tcp --dport 7443 -j RETURN",
		"-A " + InChain + " -i wg0 -j DROP",
	}
	last := -1
	for _, w := range want {
		i := strings.Index(s, w)
		if i < 0 || i < last {
			t.Fatalf("missing or out of order %q in:\n%s", w, s)
		}
		last = i
	}
	if strings.Contains(renderFilter(nil, testBase), "wg0") {
		t.Error("a base without WGIface renders wg0 rules")
	}
	b.WGPanelPort = 8080
	if !strings.Contains(renderFilter(nil, b), "-A "+InChain+" -i wg0 -p tcp --dport 8080 -j RETURN\n-A "+InChain+" -i wg0 -j DROP") {
		t.Error("the panel port is not accepted before the wg0 drop")
	}
	b.WGIface = "wg0 -j ACCEPT"
	if b.check() == nil {
		t.Error("an interface name with a space reached the script")
	}
}

// Full is the whole script from nothing, whatever deltas built the table.
func TestTableFull(t *testing.T) {
	tb := New()
	tb.Restore = func(context.Context, string) error { return nil }
	tb.Exec = func(_ context.Context, argv ...string) error {
		if has(argv, "-D") { // like iptables once the jump is gone
			return context.Canceled
		}
		return nil
	}
	tb.Read = func(context.Context, ...string) (string, error) { return "-A x", nil }
	ctx := context.Background()
	if err := tb.Rebuild(ctx, nil, testBase); err != nil {
		t.Fatal(err)
	}
	if err := tb.Set(ctx, "10.213.1.2", []string{"10.213.1.3"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := tb.Set(ctx, "10.213.1.4", []string{"10.213.1.5"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := tb.Remove(ctx, "10.213.1.4"); err != nil {
		t.Fatal(err)
	}
	full := tb.Full()
	if !strings.Contains(full, "--to-destination 10.213.1.3") || strings.Contains(full, "10.213.1.5") || strings.Contains(full, "-X ") {
		t.Fatalf("full script:\n%s", full)
	}
}

// The controller's rsync door has the forward rule too, matched on the
// original destination port (Docker DNATs the published ports).
func TestAllowTCPForwardRule(t *testing.T) {
	tb := New()
	var got []string
	tb.Exec = func(_ context.Context, argv ...string) error {
		got = append(got, strings.Join(argv, " "))
		if argv[1] == "-S" || argv[1] == "-I" {
			return nil // DOCKER-USER exists, inserts work
		}
		return errors.New("absent")
	}
	if err := tb.AllowTCP(context.Background(), "10.207.255.2", 8873, 8874, true); err != nil {
		t.Fatal(err)
	}
	j := strings.Join(got, "\n")
	for _, w := range []string{"-I INPUT 1 -s 10.207.255.2", "-I DOCKER-USER 1 -s 10.207.255.2 -p tcp -m conntrack --ctorigdstport 8873:8874"} {
		if !strings.Contains(j, w) {
			t.Errorf("missing %q in:\n%s", w, j)
		}
	}
}
