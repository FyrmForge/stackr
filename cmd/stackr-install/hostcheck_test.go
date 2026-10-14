package main

import (
	"net/netip"
	"strings"
	"testing"
)

func TestAtLeast(t *testing.T) {
	for _, c := range []struct {
		have, want string
		ok         bool
	}{
		{"29.0.0", minDocker, true},
		{"25.0.3", minDocker, false}, // volume subpath mounts need 26
		{"25.0.3", "25.0", true},
		{"24.0.9", "25.0", false},
		{"iptables v1.8.10 (nf_tables)", "1.8", true},
		{"iptables v1.6.1", "1.8", false},
		{"6.8.0-45-generic", "5.6", true},
		{"5.4.0-200-generic", "5.6", false},
		{"", "25.0", false},
	} {
		if got := atLeast(c.have, c.want); got != c.ok {
			t.Errorf("atLeast(%q, %q) = %v", c.have, c.want, got)
		}
	}
}

// A box that meets everything: a current Ubuntu with Docker.
func goodHost() hostFacts {
	return hostFacts{Docker: "29.0.0", Systemd: true, IP: true, Iptables: "iptables v1.8.10 (nf_tables)",
		Kernel: "6.8.0-45-generic", Interface: []netip.Prefix{netip.MustParsePrefix("192.168.1.20/24")}}
}

func TestRequirements(t *testing.T) {
	for _, c := range []struct {
		name   string
		edit   func(*hostFacts)
		worker bool
		fresh  bool
		want   string // a substring of the one problem, "" for none
	}{
		{"all met, worker", func(*hostFacts) {}, true, true, ""},
		{"all met, controller", func(*hostFacts) {}, false, true, ""},
		{"old docker", func(f *hostFacts) { f.Docker = "24.0.7" }, true, true, "Docker 24.0.7 is too old"},
		{"nftables backend", func(f *hostFacts) { f.Firewall = "nftables" }, false, true, "nftables firewall backend"},
		{"no iptables", func(f *hostFacts) { f.Iptables = "" }, true, true, "iptables is missing"},
		{"old iptables", func(f *hostFacts) { f.Iptables = "iptables v1.6.1" }, true, true, "is too old: stackr needs iptables 1.8"},
		{"legacy on a controller", func(f *hostFacts) { f.Iptables = "iptables v1.8.9 (legacy)" }, false, true, "legacy backend"},
		{"legacy on a worker is fine", func(f *hostFacts) { f.Iptables = "iptables v1.8.9 (legacy)" }, true, true, ""},
		{"old kernel, module loaded", func(f *hostFacts) { f.Kernel, f.WGModule = "5.4.0", true }, true, true, ""},
		{"old kernel, no module, worker", func(f *hostFacts) { f.Kernel = "5.4.0" }, true, true, "has no WireGuard"},
		{"no systemd on a worker", func(f *hostFacts) { f.Systemd = false }, true, true, "systemd is not running"},
		{"no systemd on a controller", func(f *hostFacts) { f.Systemd = false }, false, true, ""},
		{"no ip on a worker", func(f *hostFacts) { f.IP = false }, true, true, "iproute2"},
		{"LAN in the supernet", func(f *hostFacts) {
			f.Interface = []netip.Prefix{netip.MustParsePrefix("10.210.4.7/16")}
		}, true, true, "overlaps 10.208.0.0/12"},
		{"LAN on the transit", func(f *hostFacts) {
			f.Interface = []netip.Prefix{netip.MustParsePrefix("10.207.0.1/16")}
		}, false, true, "overlaps 10.207.255.0/24"},
		{"a broad 10/8 LAN", func(f *hostFacts) {
			f.Interface = []netip.Prefix{netip.MustParsePrefix("10.0.0.5/8")}
		}, true, true, "overlaps"},
		{"Docker pool on the cluster pool", func(f *hostFacts) {
			f.DockerPools = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
		}, true, false, "default address pool 10.0.0.0/8 overlaps"},
		{"Docker pool elsewhere", func(f *hostFacts) {
			f.DockerPools = []netip.Prefix{netip.MustParsePrefix("172.80.0.0/12")}
		}, true, true, ""},
		{"overlap on a rejoin is ours", func(f *hostFacts) {
			f.Interface = []netip.Prefix{netip.MustParsePrefix("10.210.4.1/24")}
		}, true, false, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := goodHost()
			c.edit(&f)
			var hard []string
			for _, m := range requirements(f, c.worker, c.fresh) {
				if !strings.HasPrefix(m, "warning: ") {
					hard = append(hard, m)
				}
			}
			switch {
			case c.want == "" && len(hard) > 0:
				t.Fatalf("problems = %q, want none", hard)
			case c.want != "" && (len(hard) == 0 || !strings.Contains(strings.Join(hard, "\n"), c.want)):
				t.Fatalf("problems = %q, want one with %q", hard, c.want)
			}
		})
	}
}

// A controller with no WireGuard only warns: it needs it once a worker joins.
func TestRequirementsControllerWireGuardWarns(t *testing.T) {
	f := goodHost()
	f.Kernel = "5.4.0"
	miss := requirements(f, false, true)
	if len(miss) != 1 || !strings.HasPrefix(miss[0], "warning: ") {
		t.Fatalf("problems = %q, want one warning", miss)
	}
	if err := failRequirements(runner{out: &strings.Builder{}}, miss); err != nil {
		t.Fatalf("a warning failed the install: %v", err)
	}
}

func TestBusyPorts(t *testing.T) {
	taken := map[string]bool{"tcp :7443": true, "udp :51871": true}
	got := busyPorts(fixedPorts(true, 51871), func(proto, addr string) bool { return taken[proto+" "+addr] })
	if len(got) != 2 || !strings.Contains(got[0], "UDP port 51871") || !strings.Contains(got[1], "TCP port 7443 is in use: stackr needs it for the worker API") {
		t.Fatalf("busy = %q", got)
	}
	ctl := fixedPorts(false, 51900)
	if len(ctl) != 2 || ctl[0].addr != ":51900" || ctl[1].addr != "127.0.0.1:5000" {
		t.Fatalf("controller ports = %v", ctl)
	}
}
