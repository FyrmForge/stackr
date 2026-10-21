package worker

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/FyrmForge/stackr/internal/mesh"
)

// Metrics is GET /v1/metrics: raw counters of this host and its tile
// containers, read from /proc and cgroup v2 files (never Docker's stats call,
// which costs about a second per container). The controller turns the
// cumulative ones into rates between two samples.
type Metrics struct {
	At         time.Time         `json:"at"`
	Host       HostSample        `json:"host"`
	Containers []ContainerSample `json:"containers"`
	// NoCgroup: the host is not on cgroup v2, so no container samples.
	NoCgroup bool `json:"no_cgroup,omitempty"`
	// Peers is each mesh wg peer's counters, by public key; none without a mesh.
	Peers map[string]PeerSample `json:"peers,omitempty"`
}

// PeerSample is one wg peer: bytes received from it and sent to it
// (cumulative), and its last handshake (unix seconds, 0 = never).
type PeerSample struct {
	Rx        int64 `json:"rx"`
	Tx        int64 `json:"tx"`
	Handshake int64 `json:"handshake,omitempty"`
}

// HostSample: CPU jiffies (cumulative), memory and the Docker root's disk.
type HostSample struct {
	CPUTotal  uint64 `json:"cpu_total"`
	CPUIdle   uint64 `json:"cpu_idle"`
	CPUs      int    `json:"cpus"`
	MemTotal  uint64 `json:"mem_total"`
	MemAvail  uint64 `json:"mem_avail"`
	DiskTotal uint64 `json:"disk_total"`
	DiskFree  uint64 `json:"disk_free"`
}

// ContainerSample is one tile container. CPU, IO and net are cumulative.
// Replicas share their pause container's network namespace, so a tile's
// network is its pause's counters.
type ContainerSample struct {
	Tile    string `json:"tile"`
	Name    string `json:"name"`
	Role    string `json:"role"` // replica | pause | run
	CPUUsec uint64 `json:"cpu_usec"`
	Mem     uint64 `json:"mem"` // memory.current minus inactive file cache
	IORead  uint64 `json:"io_read"`
	IOWrite uint64 `json:"io_write"`
	NetRx   uint64 `json:"net_rx"`
	NetTx   uint64 `json:"net_tx"`
}

const labelRole = "stackr.role" // tile.LabelRole

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	m, err := s.Metrics(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(m)
}

// Metrics reads one sample. HostRoot prefixes /proc and /sys/fs/cgroup: the
// panel container sees the host's through read-only binds.
func (s *Server) Metrics(ctx context.Context) (Metrics, error) {
	root := s.cfg.HostRoot
	m := Metrics{At: time.Now()}
	m.Host = hostSample(root)
	m.Host.DiskTotal, m.Host.DiskFree = diskOf(s.cfg.DiskPath)
	if s.cfg.Mesh.WG != nil {
		if st, err := s.cfg.Mesh.WG.Stats(mesh.Iface); err == nil && len(st) > 0 {
			m.Peers = map[string]PeerSample{}
			for k, p := range st {
				ps := PeerSample{Rx: p.Rx, Tx: p.Tx}
				if !p.Handshake.IsZero() {
					ps.Handshake = p.Handshake.Unix()
				}
				m.Peers[k] = ps
			}
		}
	}
	cg := filepath.Join(root, "/sys/fs/cgroup")
	if _, err := os.Stat(filepath.Join(cg, "cgroup.controllers")); err != nil {
		m.NoCgroup = true
		return m, nil
	}
	cs, err := s.tileContainers(ctx)
	if err != nil {
		return m, err
	}
	for _, c := range cs {
		dir := containerCgroup(cg, c.ID)
		if dir == "" {
			continue
		}
		smp := ContainerSample{Tile: c.Labels[labelTile], Name: strings.TrimPrefix(firstOr(c.Names), "/"), Role: c.Labels[labelRole]}
		readCgroup(dir, &smp)
		if pid := firstPid(dir); pid != "" {
			smp.NetRx, smp.NetTx = netDev(readFile(filepath.Join(root, "/proc", pid, "net/dev")))
		}
		m.Containers = append(m.Containers, smp)
	}
	return m, nil
}

type dockerCtr struct {
	ID     string
	Names  []string
	Labels map[string]string
}

func (s *Server) tileContainers(ctx context.Context) ([]dockerCtr, error) {
	hc := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", s.cfg.DockerSock)
		}}}
	defer hc.CloseIdleConnections()
	f := `{"label":["` + labelManaged + `=true"],"status":["running"]}`
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://local/containers/json?filters="+url.QueryEscape(f), nil)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var cs []dockerCtr
	if err := json.NewDecoder(resp.Body).Decode(&cs); err != nil {
		return nil, err
	}
	out := cs[:0]
	for _, c := range cs {
		if c.Labels[labelTile] != "" {
			out = append(out, c)
		}
	}
	return out, nil
}

func firstOr(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

// containerCgroup is a container's cgroup dir: the systemd driver's scope or
// the cgroupfs driver's directory; "" when neither exists.
func containerCgroup(cg, id string) string {
	for _, d := range []string{
		filepath.Join(cg, "system.slice", "docker-"+id+".scope"),
		filepath.Join(cg, "docker", id),
	} {
		if _, err := os.Stat(d); err == nil {
			return d
		}
	}
	return ""
}

// readCgroup fills CPU, memory and IO from a cgroup v2 dir.
func readCgroup(dir string, s *ContainerSample) {
	s.CPUUsec = kv(readFile(filepath.Join(dir, "cpu.stat")))["usage_usec"]
	cur, _ := strconv.ParseUint(strings.TrimSpace(readFile(filepath.Join(dir, "memory.current"))), 10, 64)
	inactive := kv(readFile(filepath.Join(dir, "memory.stat")))["inactive_file"]
	if inactive < cur {
		s.Mem = cur - inactive
	}
	// io.stat: "MAJ:MIN rbytes=N wbytes=N ..." per device.
	for _, f := range strings.Fields(readFile(filepath.Join(dir, "io.stat"))) {
		k, v, _ := strings.Cut(f, "=")
		n, _ := strconv.ParseUint(v, 10, 64)
		switch k {
		case "rbytes":
			s.IORead += n
		case "wbytes":
			s.IOWrite += n
		}
	}
}

// kv reads "key value" lines.
func kv(s string) map[string]uint64 {
	out := map[string]uint64{}
	for _, l := range strings.Split(s, "\n") {
		if f := strings.Fields(l); len(f) == 2 {
			out[f[0]], _ = strconv.ParseUint(f[1], 10, 64)
		}
	}
	return out
}

func firstPid(dir string) string {
	p, _, _ := strings.Cut(readFile(filepath.Join(dir, "cgroup.procs")), "\n")
	return strings.TrimSpace(p)
}

// netDev sums /proc/<pid>/net/dev bytes over every interface but lo.
func netDev(s string) (rx, tx uint64) {
	for _, l := range strings.Split(s, "\n") {
		name, rest, ok := strings.Cut(l, ":")
		if !ok || strings.TrimSpace(name) == "lo" {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			continue
		}
		r, _ := strconv.ParseUint(f[0], 10, 64)
		t, _ := strconv.ParseUint(f[8], 10, 64)
		rx, tx = rx+r, tx+t
	}
	return rx, tx
}

func hostSample(root string) HostSample {
	h := HostSample{}
	for _, l := range strings.Split(readFile(filepath.Join(root, "/proc/stat")), "\n") {
		f := strings.Fields(l)
		if len(f) > 4 && f[0] == "cpu" {
			for i, v := range f[1:] {
				n, _ := strconv.ParseUint(v, 10, 64)
				h.CPUTotal += n
				if i == 3 || i == 4 { // idle, iowait
					h.CPUIdle += n
				}
			}
		}
		if strings.HasPrefix(l, "cpu") && len(f) > 0 && f[0] != "cpu" {
			h.CPUs++
		}
	}
	mem := readFile(filepath.Join(root, "/proc/meminfo"))
	h.MemTotal, h.MemAvail = meminfoIn(mem, "MemTotal:"), meminfoIn(mem, "MemAvailable:")
	// In an LXC, lxcfs answers /proc/meminfo for the reader's own cgroup, so
	// the panel container sees only itself. The cgroup root it mounts is the
	// LXC's, whose memory.current is the whole box; a bare host's true root
	// has none and keeps meminfo. Used is that minus page cache, as free has it.
	cg := filepath.Join(root, "/sys/fs/cgroup")
	if cur, err := strconv.ParseUint(strings.TrimSpace(readFile(filepath.Join(cg, "memory.current"))), 10, 64); err == nil && cur > 0 {
		if lim, err := strconv.ParseUint(strings.TrimSpace(readFile(filepath.Join(cg, "memory.max"))), 10, 64); err == nil && lim < h.MemTotal {
			h.MemTotal = lim
		}
		used := cur - min(kv(readFile(filepath.Join(cg, "memory.stat")))["file"], cur)
		h.MemAvail = h.MemTotal - min(used, h.MemTotal)
	}
	return h
}

func meminfoIn(s, key string) uint64 {
	for _, l := range strings.Split(s, "\n") {
		if v, ok := strings.CutPrefix(l, key); ok {
			if f := strings.Fields(v); len(f) > 0 {
				kb, _ := strconv.ParseUint(f[0], 10, 64)
				return kb << 10
			}
		}
	}
	return 0
}

// diskOf is the filesystem holding p; "" means "/".
func diskOf(p string) (total, free uint64) {
	if p == "" {
		p = "/"
	}
	var st syscall.Statfs_t
	if syscall.Statfs(p, &st) != nil {
		return 0, 0
	}
	return st.Blocks * uint64(st.Bsize), st.Bavail * uint64(st.Bsize)
}
