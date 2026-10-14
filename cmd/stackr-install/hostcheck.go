package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/term"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/mesh"
)

// minDocker is the oldest Docker Engine stackr runs on. 26.0 (API 1.45) is
// the first whose volume mounts take a subpath, which a tile's config files
// use; 25.0 already attaches a container to several networks when it is
// created, which every tile with a link or an ingress name relies on. Builds
// run the panel image's own docker CLI and buildx, so the host needs no
// buildx of its own.
const minDocker = "26.0"

// minIptables: 1.8 is the first with the nf_tables backend and the -w lock
// flag everywhere; iptables-restore --noflush, which the rules use, is older.
const minIptables = "1.8"

// minKernelWG is the first kernel with WireGuard built in; an older one needs
// the wireguard module (a distro backport) loaded.
const minKernelWG = "5.6"

// hostFacts is what the requirement checks read off the box. gatherHost fills
// it; tests fake it.
type hostFacts struct {
	Docker    string // the daemon's version, "" when none answers
	Firewall  string // Docker's firewall backend, "" when it does not say (iptables)
	Systemd   bool
	IP        bool   // iproute2's ip
	Iptables  string // `iptables --version`, "" when missing
	Kernel    string // the running kernel's release
	WGModule  bool   // the wireguard module is loaded
	Interface []netip.Prefix
	// DockerPools are Docker's default-address-pools (daemon.json): where
	// Docker picks a subnet for a network nobody gave one.
	DockerPools []netip.Prefix
}

func gatherHost(ctx context.Context) hostFacts {
	var f hostFacts
	f.Docker, _ = read(ctx, "docker", "version", "--format", "{{.Server.Version}}")
	f.Firewall, _ = read(ctx, "docker", "info", "--format", "{{.FirewallBackend.Driver}}")
	_, err := os.Stat("/run/systemd/system")
	f.Systemd = err == nil
	f.IP = have("ip")
	f.Iptables, _ = read(ctx, "iptables", "--version")
	b, _ := os.ReadFile("/proc/sys/kernel/osrelease")
	f.Kernel = strings.TrimSpace(string(b))
	_, err = os.Stat("/sys/module/wireguard")
	f.WGModule = err == nil
	f.DockerPools = dockerPools("/etc/docker/daemon.json")
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if i.Flags&net.FlagLoopback != 0 || i.Name == mesh.Iface {
			continue
		}
		as, _ := i.Addrs()
		for _, a := range as {
			if p, err := netip.ParsePrefix(a.String()); err == nil {
				f.Interface = append(f.Interface, p)
			}
		}
	}
	return f
}

// dockerPools reads the default-address-pools bases out of a daemon.json;
// none (no file, no setting) is Docker's built-in 172.17-31 and 192.168 pools.
func dockerPools(path string) []netip.Prefix {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cfg struct {
		Pools []struct {
			Base string `json:"base"`
		} `json:"default-address-pools"`
	}
	if json.Unmarshal(b, &cfg) != nil {
		return nil
	}
	var out []netip.Prefix
	for _, p := range cfg.Pools {
		if pf, err := netip.ParsePrefix(p.Base); err == nil {
			out = append(out, pf)
		}
	}
	return out
}

var leadingVersion = regexp.MustCompile(`v?([0-9]+)\.([0-9]+)`)

// atLeast compares the first major.minor found in have against want.
func atLeast(have, want string) bool {
	h, w := leadingVersion.FindStringSubmatch(have), leadingVersion.FindStringSubmatch(want)
	if h == nil || w == nil {
		return false
	}
	hm, _ := strconv.Atoi(h[1])
	hn, _ := strconv.Atoi(h[2])
	wm, _ := strconv.Atoi(w[1])
	wn, _ := strconv.Atoi(w[2])
	return hm > wm || hm == wm && hn >= wn
}

// reservedRanges are the addresses stackr hands out itself: a box sitting on
// one of them would route its own network into the mesh.
// ponytail: fixed ranges; make them install-time settings when a user needs it.
var reservedRanges = []struct {
	p    netip.Prefix
	what string
}{
	{mesh.Transit, "the mesh's tunnel addresses"},
	{mesh.Supernet, "the cluster's network pool"},
	{mesh.ExitNet, "the exit links"},
}

// requirements lists every unmet requirement, one line each. worker is a
// join (stackrd runs on the host); fresh is a first install, where the box's
// networks are checked against the reserved ranges (later, stackr's own
// networks sit in them).
func requirements(f hostFacts, worker, fresh bool) []string {
	var miss []string
	if f.Docker != "" && !atLeast(f.Docker, minDocker) {
		miss = append(miss, fmt.Sprintf("Docker %s is too old: stackr needs Docker Engine %s or newer", f.Docker, minDocker))
	}
	if strings.Contains(f.Firewall, "nftables") {
		miss = append(miss, "Docker runs its nftables firewall backend: stackr needs the iptables backend (drop \"firewall-backend\" from /etc/docker/daemon.json)")
	}
	switch {
	case f.Iptables == "":
		miss = append(miss, "iptables is missing: install iptables "+minIptables+" or newer")
	case !atLeast(f.Iptables, minIptables):
		miss = append(miss, fmt.Sprintf("%s is too old: stackr needs iptables %s or newer", f.Iptables, minIptables))
	case !worker && strings.Contains(f.Iptables, "legacy"):
		// The panel image's iptables writes nf_tables; Docker's chains
		// would sit in the other backend and never see stackr's rules.
		miss = append(miss, "iptables uses the legacy backend: the controller needs iptables-nft (on Debian/Ubuntu: update-alternatives --set iptables /usr/sbin/iptables-nft, then restart Docker)")
	}
	if !f.WGModule && !atLeast(f.Kernel, minKernelWG) {
		msg := fmt.Sprintf("kernel %s has no WireGuard: stackr needs Linux %s or newer, or the wireguard module", f.Kernel, minKernelWG)
		if worker {
			miss = append(miss, msg)
		} else if f.Kernel != "" {
			// A single controller runs no mesh; it only needs WireGuard
			// once a worker joins.
			miss = append(miss, "warning: "+msg+" (needed once a worker joins)")
		}
	}
	if worker {
		if !f.Systemd {
			miss = append(miss, "systemd is not running: the worker runs as a systemd service")
		}
		if !f.IP {
			miss = append(miss, "the ip command (iproute2) is missing")
		}
	}
	// A Docker pool on stackr's ranges lets Docker hand out a network of its
	// own from the cluster's pool, clashing with an env on another node.
	for _, p := range f.DockerPools {
		for _, r := range reservedRanges {
			if r.p.Overlaps(p.Masked()) {
				miss = append(miss, fmt.Sprintf("Docker's default address pool %s overlaps %s, which stackr reserves for %s: change default-address-pools in /etc/docker/daemon.json",
					p.Masked(), r.p, r.what))
			}
		}
	}
	if fresh {
		for _, p := range f.Interface {
			for _, r := range reservedRanges {
				if r.p.Overlaps(p.Masked()) {
					miss = append(miss, fmt.Sprintf("this box's network %s overlaps %s, which stackr reserves for %s",
						p.Masked(), r.p, r.what))
				}
			}
		}
	}
	return miss
}

// failRequirements is an error naming every unmet requirement; warnings are
// printed and do not fail.
func failRequirements(r runner, miss []string) error {
	var hard []string
	for _, m := range miss {
		if w, ok := strings.CutPrefix(m, "warning: "); ok {
			r.say("  warning:", w)
			continue
		}
		hard = append(hard, "  - "+m)
	}
	if len(hard) == 0 {
		return nil
	}
	return fmt.Errorf("this box does not meet stackr's requirements:\n%s\nSee System requirements in the README",
		strings.Join(hard, "\n"))
}

// portUse is one fixed port and what of stackr's needs it.
type portUse struct {
	addr, proto, what string
}

// fixedPorts are the ports a role binds on the host. 80/443 are the
// controller's own --http-port/--https-port check; a worker's proxy checks
// them when it is tagged ingress.
func fixedPorts(worker bool, wgPort int) []portUse {
	wg := portUse{":" + strconv.Itoa(wgPort), "udp", "the mesh's WireGuard (--wg-port)"}
	if !worker {
		return []portUse{wg, {"127.0.0.1:" + installspec.RegistryPort, "tcp", "the controller's image registry"}}
	}
	return []portUse{wg,
		{":" + joinWorkerPort, "tcp", "the worker API"},
		{":7444", "tcp", "the worker's liveness answer"},
		{"127.0.0.1:" + installspec.RegistryPort, "tcp", "the worker's registry front"},
	}
}

// busyPorts names every fixed port something else holds.
func busyPorts(ps []portUse, busy func(proto, addr string) bool) []string {
	var out []string
	for _, p := range ps {
		if busy(p.proto, p.addr) {
			out = append(out, fmt.Sprintf("%s port %s is in use: stackr needs it for %s",
				strings.ToUpper(p.proto), strings.TrimPrefix(p.addr, ":"), p.what))
		}
	}
	return out
}

// hostBusy tries to bind; for TCP a connect first, since a low port cannot
// be bound by a non-root dry run whether it is taken or not.
func hostBusy(proto, addr string) bool {
	if proto == "udp" {
		c, err := net.ListenPacket("udp", addr)
		if err != nil {
			return true
		}
		_ = c.Close()
		return false
	}
	host, port, _ := net.SplitHostPort(addr)
	if c, err := net.Dial("tcp", net.JoinHostPort(cmpHost(host), port)); err == nil {
		_ = c.Close()
		return true
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return true
	}
	_ = l.Close()
	return false
}

func cmpHost(h string) string {
	if h == "" {
		return "127.0.0.1"
	}
	return h
}

var errNoDocker = errors.New("Docker is not installed") //nolint:staticcheck // Docker is a name, and the line is shown as is

// ensureDocker makes sure Docker answers, offering Docker's install script
// when it is missing: --yes runs it, a terminal asks, anything else stops
// with what to do. An installed Docker is never upgraded here.
func (r runner) ensureDocker(ctx context.Context, yes bool) error {
	if _, err := read(ctx, "docker", "version", "--format", "{{.Server.Version}}"); err == nil {
		r.say("  have docker")
		return nil
	}
	if have("docker") {
		return errors.New("docker is installed but its daemon does not answer; start it (systemctl start docker) and re-run")
	}
	if !yes {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return fmt.Errorf("%w: install Docker Engine %s or newer (https://docs.docker.com/engine/install/), or re-run with --yes to use Docker's install script", errNoDocker, minDocker)
		}
		var ok string
		if err := ask([]question{{title: "Docker is not installed. Install it with Docker's install script (get.docker.com)?",
			kind: askYesNo, def: func() string { return "n" }, out: &ok}}); err != nil {
			return err
		}
		if ok != "Yes" {
			return fmt.Errorf("%w: install Docker Engine %s or newer and re-run", errNoDocker, minDocker)
		}
	}
	if !have("curl") {
		return errors.New("curl is missing: install curl (or Docker itself) and re-run")
	}
	r.say("  installing Docker from get.docker.com")
	if err := r.run(ctx, "sh", "-c", "curl -fsSL https://get.docker.com | sh"); err != nil {
		return fmt.Errorf("install docker: %w", err)
	}
	return r.run(ctx, "systemctl", "enable", "--now", "docker")
}

// checkWGPort: a port number for --wg-port.
func checkWGPort(n int) error {
	if n < 1 || n > 65535 {
		return fmt.Errorf("--wg-port %d is not a port", n)
	}
	return nil
}

// meshUp reports whether this box already runs stackr's mesh, which then
// holds the WireGuard port itself.
func meshUp() bool {
	_, err := os.Stat("/sys/class/net/" + mesh.Iface)
	return err == nil
}
