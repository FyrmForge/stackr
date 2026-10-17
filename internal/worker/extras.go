package worker

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/FyrmForge/stackr/internal/mesh"
)

const maxBody = 4 << 20

// Facts is GET /v1/facts.
type Facts struct {
	Interfaces  []Iface `json:"interfaces"`
	DiskFree    uint64  `json:"disk_free"`  // bytes free on the Docker root
	DiskTotal   uint64  `json:"disk_total"` // size of that disk
	DockerRoot  string  `json:"docker_root"`
	Docker      string  `json:"docker"` // server version
	DockerError string  `json:"docker_error,omitempty"`
	Kernel      string  `json:"kernel"`
	CPUs        int     `json:"cpus"`
	// Arch is runtime.GOARCH of the worker binary.
	// ponytail: the worker binary's arch, not Docker's Architecture. Same thing on every real install.
	Arch string `json:"arch,omitempty"`
	// Version is the stackr build the worker runs (empty from an older one).
	Version  string  `json:"version,omitempty"`
	MemBytes uint64  `json:"mem_bytes"`
	MemFree  uint64  `json:"mem_free"` // MemAvailable: what a build could use
	UptimeS  float64 `json:"uptime_s"`
	// Seen is where each wg peer (by public key) sends from, for peers with a
	// fresh handshake: a peer behind a NAT shows as the NAT's address.
	Seen map[string]string `json:"seen,omitempty"`
	// Rx is the bytes received from each wg peer (by public key) when the
	// facts were read; the controller watches it move to tell a live
	// tunnel from a dead one within a minute (keepalives every 25 s).
	Rx map[string]int64 `json:"rx,omitempty"`
	// Tx is the bytes sent to each wg peer (by public key).
	Tx map[string]int64 `json:"tx,omitempty"`
	// Handshake is each wg peer's last handshake, unix seconds; peers that
	// never shook hands are left out.
	Handshake map[string]int64 `json:"handshake,omitempty"`
	// Ping is the ICMP round trip to each mesh peer, by transit address.
	Ping map[string]PeerPing `json:"ping,omitempty"`
	// Resolvers are the host's IPv4 DNS servers.
	Resolvers []string `json:"resolvers,omitempty"`
	// WGPort is the UDP port the node's mesh listens on (0 from an older
	// worker: mesh.DefaultPort).
	WGPort int `json:"wg_port,omitempty"`
	// Busy are the ingress ports (80, 443) something already listens on; the
	// controller reads them before it starts a proxy there.
	Busy []int `json:"busy,omitempty"`
}

// IngressPorts are what a node's ingress proxy binds on the host.
var IngressPorts = []int{80, 443}

// busyPorts tries to bind each TCP port on every address; only "in use"
// counts (a non-root fake worker may not bind low ports at all).
func busyPorts(ports []int) []int {
	var out []int
	for _, p := range ports {
		l, err := net.Listen("tcp", ":"+strconv.Itoa(p))
		if errors.Is(err, syscall.EADDRINUSE) {
			out = append(out, p)
		}
		if err == nil {
			_ = l.Close()
		}
	}
	return out
}

type Iface struct {
	Name     string   `json:"name"`
	Prefixes []string `json:"prefixes"`
	// Public is set when a public address is assigned directly on the
	// interface (not private, CGNAT or link-local).
	Public bool `json:"public"`
	// Routes are the IPv4 routes out of the interface, the default left out.
	Routes []string `json:"routes,omitempty"`
}

// LANs are the interface's prefixes, a host address (/32) widened to the
// route that holds it: a cloud private network (Hetzner) hands each box a
// /32 and reaches the rest through a route (10.0.0.0/16 via 10.0.0.1).
// The address is kept, so Masked is the network and Addr the box.
func (i Iface) LANs() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range i.Prefixes {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			continue
		}
		if p.IsSingleIP() {
			best := -1
			for _, r := range i.Routes {
				rp, err := netip.ParsePrefix(r)
				if err == nil && rp.Bits() < p.Bits() && rp.Bits() > best && rp.Contains(p.Addr()) {
					best = rp.Bits()
				}
			}
			if best >= 0 {
				p = netip.PrefixFrom(p.Addr(), best)
			}
		}
		out = append(out, p)
	}
	return out
}

// State is the full applied state, replayed in this order.
type State struct {
	WG     json.RawMessage `json:"wg,omitempty"`
	Routes [][]string      `json:"routes,omitempty"` // argv for `ip`
	Chains string          `json:"chains,omitempty"` // iptables-restore payload
	Jumps  string          `json:"jumps,omitempty"`  // iptables-restore payload
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func isPublic(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsGlobalUnicast() && !a.IsPrivate() && !cgnat.Contains(a)
}

func fail(w http.ResponseWriter, code int, err error) {
	http.Error(w, err.Error(), code)
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		fail(w, 400, err)
		return nil, false
	}
	return b, true
}

// Interfaces is this machine's interfaces with their prefixes; the join
// request reports the same list before a worker exists.
func Interfaces() []Iface {
	out := []Iface{}
	rs := routes(readFile("/proc/net/route"))
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		fi := Iface{Name: i.Name, Prefixes: []string{}, Routes: rs[i.Name]}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if p, err := netip.ParsePrefix(a.String()); err == nil {
				fi.Prefixes = append(fi.Prefixes, p.String())
				fi.Public = fi.Public || isPublic(p.Addr())
			}
		}
		out = append(out, fi)
	}
	return out
}

// ProbePrefix is where the port-forward probe's path lives; a node's Caddy
// answers its id there (domain.Install.Probe).
const ProbePrefix = "/.well-known/stackr-probe/"

// Probe is POST /v1/probe: fetch URL from this node, which sits outside the
// network being probed, and say who answered.
type Probe struct {
	URL string `json:"url"`
}

// ProbeResult is the answer's status and body ("" when nothing answered).
type ProbeResult struct {
	Status int    `json:"status"`
	Body   string `json:"body"`
}

var probeClient = &http.Client{
	Timeout:       5 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func (s *Server) probe(w http.ResponseWriter, r *http.Request) {
	b, ok := readBody(w, r)
	if !ok {
		return
	}
	var p Probe
	if err := json.Unmarshal(b, &p); err != nil {
		fail(w, 400, err)
		return
	}
	u, err := url.Parse(p.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || !strings.HasPrefix(u.Path, ProbePrefix) {
		fail(w, 400, fmt.Errorf("%q is not a probe URL", p.URL))
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, u.String(), nil)
	if err != nil {
		fail(w, 400, err)
		return
	}
	var out ProbeResult
	if resp, err := probeClient.Do(req); err == nil {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 128))
		_ = resp.Body.Close()
		out = ProbeResult{Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// routes reads /proc/net/route (hex, little-endian): IPv4 routes by
// interface, the default left out.
// ponytail: IPv4 only; IPv6 /128 cloud addresses stay single hosts.
func routes(table string) map[string][]string {
	le := func(h string) (uint32, bool) {
		v, err := strconv.ParseUint(h, 16, 32)
		return bits.ReverseBytes32(uint32(v)), err == nil
	}
	out := map[string][]string{}
	for _, l := range strings.Split(table, "\n") {
		f := strings.Fields(l)
		if len(f) < 8 {
			continue
		}
		d, ok1 := le(f[1])
		m, ok2 := le(f[7])
		if !ok1 || !ok2 || m == 0 {
			continue
		}
		p := netip.PrefixFrom(netip.AddrFrom4([4]byte{byte(d >> 24), byte(d >> 16), byte(d >> 8), byte(d)}), bits.OnesCount32(m))
		out[f[0]] = append(out[f[0]], p.Masked().String())
	}
	return out
}

func (s *Server) facts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.Facts(r.Context()))
}

// HostFacts is the machine itself: kernel, cores, memory and arch. The
// installer sends it at join, before a worker runs, so the pending row has a
// System line.
func HostFacts() Facts {
	b, _ := os.ReadFile("/proc/sys/kernel/osrelease")
	return Facts{Kernel: strings.TrimSpace(string(b)), CPUs: runtime.NumCPU(), Arch: runtime.GOARCH, MemBytes: meminfo("MemTotal:")}
}

// Facts is what the node reports about itself: GET /v1/facts, and the body
// of a recovery call.
func (s *Server) Facts(ctx context.Context) Facts {
	f := HostFacts()
	f.Version, f.Interfaces = s.cfg.Version, Interfaces()
	f.MemFree = meminfo("MemAvailable:")
	if f2 := strings.Fields(readFile("/proc/uptime")); len(f2) > 0 {
		f.UptimeS, _ = strconv.ParseFloat(f2[0], 64)
	}
	if err := s.dockerInfo(ctx, &f); err != nil {
		f.DockerError = err.Error()
	}
	f.Resolvers = Resolvers()
	f.WGPort = cmp.Or(s.cfg.Mesh.WGPort, mesh.DefaultPort)
	f.Busy = busyPorts(IngressPorts)
	if s.cfg.Mesh.WG != nil {
		if seen, err := s.cfg.Mesh.WG.Seen(mesh.Iface); err == nil && len(seen) > 0 {
			f.Seen = map[string]string{}
			for k, ep := range seen {
				f.Seen[k] = ep.String()
			}
		}
		if st, err := s.cfg.Mesh.WG.Stats(mesh.Iface); err == nil && len(st) > 0 {
			f.Rx, f.Tx = map[string]int64{}, map[string]int64{}
			for k, p := range st {
				f.Rx[k], f.Tx[k] = p.Rx, p.Tx
				if !p.Handshake.IsZero() {
					if f.Handshake == nil {
						f.Handshake = map[string]int64{}
					}
					f.Handshake[k] = p.Handshake.Unix()
				}
			}
		}
		f.Ping = s.pingPeers(ctx)
	}
	return f
}

func readFile(p string) string { b, _ := os.ReadFile(p); return string(b) }

// ResolvConfs are read and joined: on a systemd-resolved host the first is the
// 127.0.0.53 stub and the second names the real upstreams.
var ResolvConfs = []string{"/etc/resolv.conf", "/run/systemd/resolve/resolv.conf"}

// Resolvers are this host's IPv4 DNS servers, loopback stubs left out.
func Resolvers() []string {
	var out []string
	for _, p := range ResolvConfs {
		for _, r := range ParseResolvers(readFile(p)) {
			if !slices.Contains(out, r) {
				out = append(out, r)
			}
		}
	}
	return out
}

// ParseResolvers reads the IPv4 nameservers of a resolv.conf.
func ParseResolvers(conf string) []string {
	var out []string
	for _, line := range strings.Split(conf, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != "nameserver" {
			continue
		}
		if a, err := netip.ParseAddr(f[1]); err == nil && a.Is4() && !a.IsLoopback() {
			out = append(out, f[1])
		}
	}
	return out
}

// meminfo is one /proc/meminfo line ("MemTotal:") in bytes.
func meminfo(key string) uint64 {
	for _, l := range strings.Split(readFile("/proc/meminfo"), "\n") {
		if v, ok := strings.CutPrefix(l, key); ok {
			if f := strings.Fields(v); len(f) > 0 {
				kb, _ := strconv.ParseUint(f[0], 10, 64)
				return kb << 10
			}
		}
	}
	return 0
}

func (s *Server) dockerInfo(ctx context.Context, f *Facts) error {
	hc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", s.cfg.DockerSock)
		}}}
	defer hc.CloseIdleConnections()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://local/info", nil)
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	var info struct{ ServerVersion, DockerRootDir string }
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return err
	}
	f.Docker, f.DockerRoot = info.ServerVersion, info.DockerRootDir
	var st syscall.Statfs_t
	if err := syscall.Statfs(info.DockerRootDir, &st); err != nil {
		// ponytail: the controller's worker runs in the panel container; its data dir is the docker root's disk on a default install.
		if err := syscall.Statfs(s.cfg.StateDir, &st); err != nil {
			return err
		}
	}
	f.DiskFree, f.DiskTotal = st.Bavail*uint64(st.Bsize), st.Blocks*uint64(st.Bsize)
	return nil
}

func (s *Server) restore(ctx context.Context, payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	out, err := s.cfg.Runner.Run(ctx, payload, "iptables-restore", "--noflush")
	if err != nil {
		return fmt.Errorf("iptables-restore: %w: %s", err, out)
	}
	return nil
}

// jumpsFirst puts each jump of a saved payload ("-I <hook> 1 -j <chain>") at
// rule 1 of its hook, once: Docker puts its own rules on top when it creates a
// network or restarts, and a restart of the worker must not stack copies.
func (s *Server) jumpsFirst(ctx context.Context, payload string) error {
	run := func(args ...string) (string, error) {
		out, err := s.cfg.Runner.Run(ctx, nil, "iptables", args...)
		return string(out), err
	}
	tbl := ""
	for _, l := range strings.Split(payload, "\n") {
		f := strings.Fields(l)
		switch {
		case len(f) == 1 && strings.HasPrefix(f[0], "*"):
			tbl = f[0][1:]
		case len(f) >= 4 && f[0] == "-I" && f[len(f)-2] == "-j" && tbl != "":
			hook, chain := f[1], f[len(f)-1]
			if out, err := run("-t", tbl, "-S", hook); err == nil && onlyFirst(out, "-A "+hook+" -j "+chain) {
				continue
			}
			for i := 0; i < 50; i++ { // every copy goes; capped in case -D never fails
				if _, err := run("-t", tbl, "-D", hook, "-j", chain); err != nil {
					break
				}
			}
			if out, err := run("-t", tbl, "-I", hook, "1", "-j", chain); err != nil {
				return fmt.Errorf("iptables -I %s: %w: %s", hook, err, out)
			}
		}
	}
	return nil
}

// onlyFirst reports whether rule is the first of the -A lines in an
// `iptables -S` listing and appears there once.
func onlyFirst(listing, rule string) bool {
	var rules []string
	for _, l := range strings.Split(listing, "\n") {
		if strings.HasPrefix(l, "-A ") {
			rules = append(rules, strings.TrimSpace(l))
		}
	}
	return len(rules) > 0 && rules[0] == rule && slices.Index(rules[1:], rule) < 0
}

// savedJumps is the jumps payload of the applied state, "" when there is none.
func (s *Server) savedJumps() string {
	b, err := os.ReadFile(s.statePath())
	if err != nil {
		return ""
	}
	var st State
	_ = json.Unmarshal(b, &st)
	return st.Jumps
}

func (s *Server) iptables(w http.ResponseWriter, r *http.Request) {
	b, ok := readBody(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.restore(r.Context(), b); err != nil {
		fail(w, 500, err)
		return
	}
	// A deploy makes its network before it pushes rules: Docker's rules for it
	// went on top of ours.
	if err := s.jumpsFirst(r.Context(), s.savedJumps()); err != nil {
		fail(w, 500, err)
		return
	}
	// The payload redeclares chains the exits put rules in.
	if err := s.reassert(r.Context()); err != nil {
		fail(w, 500, err)
		return
	}
	w.WriteHeader(204)
}

var errNoWG = errors.New("wg not available on this node")

func (s *Server) applyWG(ctx context.Context, cfg []byte) error {
	if s.cfg.WG == nil {
		return errNoWG
	}
	return s.cfg.WG.Apply(ctx, cfg)
}

func (s *Server) wg(w http.ResponseWriter, r *http.Request) {
	b, ok := readBody(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.applyWG(r.Context(), b); err != nil {
		code := 500
		if err == errNoWG {
			code = 501
		}
		fail(w, code, err)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) statePath() string { return filepath.Join(s.cfg.StateDir, "applied.json") }

func (s *Server) putState(w http.ResponseWriter, r *http.Request) {
	b, ok := readBody(w, r)
	if !ok {
		return
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		fail(w, 400, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := writeAtomic(s.statePath(), b); err != nil {
		fail(w, 500, err)
		return
	}
	w.WriteHeader(204)
}

// writeAtomic: temp file, fsync, rename, so a crash never leaves half a state.
func writeAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".applied-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func (s *Server) getState(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	b, err := os.ReadFile(s.statePath())
	s.mu.Unlock()
	if errors.Is(err, os.ErrNotExist) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		fail(w, 500, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

// replay re-applies the saved state in order: wg, routes, chains, then the
// jumps, which wait for Docker to have made DOCKER-USER, then the exits.
func (s *Server) replay(w http.ResponseWriter, r *http.Request) {
	if err := s.replayState(r.Context()); err != nil {
		fail(w, 500, err)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) replayState(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var st State
	b, err := os.ReadFile(s.statePath())
	if err == nil {
		err = json.Unmarshal(b, &st)
	} else if errors.Is(err, os.ErrNotExist) {
		err = nil // nothing pushed yet
	}
	if err != nil {
		return err
	}
	ms, err := s.savedMesh()
	if err != nil {
		return fmt.Errorf("mesh state: %w", err)
	}
	if ms != nil {
		if err := s.meshNet(*ms); err != nil {
			return fmt.Errorf("mesh: %w", err)
		}
	}
	if len(st.WG) > 0 {
		if err := s.applyWG(ctx, st.WG); err != nil {
			return fmt.Errorf("wg: %w", err)
		}
	}
	for _, argv := range st.Routes {
		if out, err := s.cfg.Runner.Run(ctx, nil, "ip", argv...); err != nil {
			return fmt.Errorf("ip %v: %w: %s", argv, err, out)
		}
	}
	if err := s.restore(ctx, []byte(st.Chains)); err != nil {
		return err
	}
	if st.Jumps != "" || ms != nil && len(ms.Links)+len(ms.Serves) > 0 {
		if err := s.waitDockerUser(ctx); err != nil {
			return err
		}
	}
	if st.Jumps != "" {
		if err := s.jumpsFirst(ctx, st.Jumps); err != nil {
			return err
		}
	}
	if ms != nil {
		if err := s.meshExits(ctx, *ms); err != nil {
			return fmt.Errorf("exits: %w", err)
		}
	}
	return nil
}

func (s *Server) waitDockerUser(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.DockerUserWait)
	defer cancel()
	for {
		if _, err := s.cfg.Runner.Run(ctx, nil, "iptables", "-n", "-L", "DOCKER-USER"); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("DOCKER-USER chain never appeared")
		case <-time.After(s.cfg.DockerUserPoll):
		}
	}
}
