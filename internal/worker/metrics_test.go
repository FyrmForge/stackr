package worker

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/wgapply"
)

// A node's sample carries each wg peer's counters and handshake.
func TestMetricsPeers(t *testing.T) {
	rec := &wgapply.Recorder{}
	rec.AddRx("K", 300)
	rec.SetTx("K", 200)
	rec.SetHandshake("K", time.Unix(1_000, 0))
	s := &Server{cfg: Config{HostRoot: t.TempDir(), Mesh: MeshConfig{WG: rec}}}
	m, err := s.Metrics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p := m.Peers["K"]; p.Rx != 300 || p.Tx != 200 || p.Handshake != 1_000 {
		t.Fatalf("peer = %+v", p)
	}
}

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A cgroup v2 dir and /proc files as the kernel writes them.
func TestMetricsParse(t *testing.T) {
	root := t.TempDir()
	cg := filepath.Join(root, "sys/fs/cgroup")
	dir := filepath.Join(cg, "system.slice", "docker-abc.scope")
	write(t, filepath.Join(dir, "cpu.stat"), "usage_usec 1500000\nuser_usec 1000000\nsystem_usec 500000\n")
	write(t, filepath.Join(dir, "memory.current"), "104857600\n")
	write(t, filepath.Join(dir, "memory.stat"), "anon 50000000\ninactive_file 4857600\nactive_file 1000\n")
	write(t, filepath.Join(dir, "io.stat"), "8:0 rbytes=1000 wbytes=2000 rios=1 wios=2\n8:16 rbytes=10 wbytes=20 rios=1 wios=1\n")
	write(t, filepath.Join(dir, "cgroup.procs"), "4242\n4243\n")
	write(t, filepath.Join(root, "proc/4242/net/dev"),
		"Inter-|   Receive |  Transmit\n face |bytes packets errs drop fifo frame compressed multicast|bytes\n"+
			"    lo:  999 1 0 0 0 0 0 0  999 1 0 0 0 0 0 0\n  eth0: 5000 10 0 0 0 0 0 0 7000 12 0 0 0 0 0 0\n")
	write(t, filepath.Join(root, "proc/stat"), "cpu  100 0 50 800 50 0 0 0 0 0\ncpu0 50 0 25 400 25 0 0 0 0 0\ncpu1 50 0 25 400 25 0 0 0 0 0\nintr 1\n")
	write(t, filepath.Join(root, "proc/meminfo"), "MemTotal:       2048 kB\nMemFree:         512 kB\nMemAvailable:   1024 kB\n")

	if got := containerCgroup(cg, "abc"); got != dir {
		t.Fatalf("cgroup dir = %q", got)
	}
	var s ContainerSample
	readCgroup(dir, &s)
	if s.CPUUsec != 1500000 || s.Mem != 100000000 || s.IORead != 1010 || s.IOWrite != 2020 {
		t.Fatalf("cgroup sample = %+v", s)
	}
	if rx, tx := netDev(readFile(filepath.Join(root, "proc/4242/net/dev"))); rx != 5000 || tx != 7000 {
		t.Fatalf("net = %d %d", rx, tx)
	}
	h := hostSample(root)
	if h.CPUTotal != 1000 || h.CPUIdle != 850 || h.CPUs != 2 || h.MemTotal != 2048<<10 || h.MemAvail != 1024<<10 {
		t.Fatalf("host = %+v", h)
	}
	if containerCgroup(cg, "nope") != "" {
		t.Fatal("missing container has a dir")
	}

	// In an LXC the cgroup root is the box's own and lxcfs's meminfo is the
	// reader's: memory comes from the root's memory.current minus page cache.
	write(t, filepath.Join(cg, "memory.current"), "1500000\n")
	write(t, filepath.Join(cg, "memory.max"), "max\n")
	write(t, filepath.Join(cg, "memory.stat"), "anon 400000\nfile 1000000\nshmem 10000\n")
	if h := hostSample(root); h.MemTotal != 2048<<10 || h.MemAvail != 2048<<10-500000 {
		t.Fatalf("lxc host = %+v", h)
	}
	write(t, filepath.Join(cg, "memory.max"), "1000000\n")
	if h := hostSample(root); h.MemTotal != 1000000 || h.MemAvail != 500000 {
		t.Fatalf("lxc host with a limit = %+v", h)
	}
}

// A container's network is read through the pid Docker reports: the panel
// container has its own pid namespace, so the host's cgroup.procs reads 0s.
func TestMetricsNetUsesDockerPid(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sys/fs/cgroup/system.slice/docker-abc.scope")
	write(t, filepath.Join(root, "sys/fs/cgroup/cgroup.controllers"), "cpu io memory\n")
	write(t, filepath.Join(dir, "cgroup.procs"), "0\n0\n")
	write(t, filepath.Join(root, "proc/0/net/dev"), "  eth0: 1 1 0 0 0 0 0 0 1 1 0 0 0 0 0 0\n")
	write(t, filepath.Join(root, "proc/4242/net/dev"), "  eth0: 5000 10 0 0 0 0 0 0 7000 12 0 0 0 0 0 0\n")

	sd, err := os.MkdirTemp("", "wk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sd) })
	sock := filepath.Join(sd, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/containers/json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `[{"Id":"abc","Names":["/app-1"],"Labels":{"stackr.managed":"true","stackr.tile":"t","stackr.role":"replica"}}]`)
	})
	mux.HandleFunc("/containers/abc/json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"State":{"Pid":4242}}`)
	})
	docker := &http.Server{Handler: mux}
	go func() { _ = docker.Serve(ln) }()
	t.Cleanup(func() { _ = docker.Close() })

	s := &Server{cfg: Config{HostRoot: root, DockerSock: sock}}
	m, err := s.Metrics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Containers) != 1 || m.Containers[0].NetRx != 5000 || m.Containers[0].NetTx != 7000 {
		t.Fatalf("containers = %+v", m.Containers)
	}
}
