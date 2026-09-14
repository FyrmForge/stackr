// Package hostmetrics reads one machine's own counters: CPU, memory, disk,
// network. It is a leaf on purpose. Both the panel's sampler and the node
// agent take this reading, and the agent must not import infra/metrics for
// it: metrics imports managedtiles, and that edge closed the cycle that kept
// managedtiles from importing the node-aware runtime (docs/plans/35-cluster.md).
package hostmetrics

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// HostCounters is one raw reading of the machine's own counters: cumulative
// CPU jiffies, cumulative network bytes, and the point-in-time memory and
// disk figures. Two readings make a sample.
//
// Exported because the node agent takes the same reading on a worker and
// posts it to the panel, the graphs on a worker's server page are the same
// graphs as the manager's, so the numbers behind them have to be measured
// the same way (docs/plans/32-multi-node-ui.md, host metrics).
type HostCounters struct {
	At time.Time

	CPUIdle, CPUTotal uint64
	RxBytes, TxBytes  uint64
	MemUsed           int64
	DiskUsed          int64

	// OK is false when /proc could not be read at all, which is every
	// non-Linux platform. A sample is never derived from one.
	OK bool
}

// HostSample is two readings turned into rates and levels.
type HostSample struct {
	CPUPct   float64
	MemBytes int64
	RxBps    float64
	TxBps    float64
	DiskUsed int64
}

// ReadHost takes one reading. Linux only; elsewhere OK is false.
func ReadHost(now time.Time) HostCounters {
	c := HostCounters{At: now}
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return c
	}
	line, _, _ := strings.Cut(string(data), "\n")
	fields := strings.Fields(line) // "cpu user nice system idle iowait irq softirq steal ..."
	if len(fields) < 5 || fields[0] != "cpu" {
		return c
	}
	for i, f := range fields[1:] {
		v, _ := strconv.ParseUint(f, 10, 64)
		c.CPUTotal += v
		if i == 3 || i == 4 { // idle + iowait
			c.CPUIdle += v
		}
	}
	c.MemUsed = hostMemUsed()
	c.RxBytes, c.TxBytes = hostNet()
	var st syscall.Statfs_t
	if err := syscall.Statfs("/", &st); err == nil {
		c.DiskUsed = int64(st.Blocks-st.Bfree) * int64(st.Bsize)
	}
	c.OK = true
	return c
}

// Sample turns the previous reading and this one into rates. ok is false on
// the first reading, on a counter reset, and on any platform ReadHost cannot
// serve, in each case there is a reading to keep but no sample to store.
func (c HostCounters) Sample(prev HostCounters) (HostSample, bool) {
	if !c.OK || !prev.OK || prev.At.IsZero() {
		return HostSample{}, false
	}
	dTotal := float64(c.CPUTotal - prev.CPUTotal)
	if c.CPUTotal <= prev.CPUTotal || dTotal <= 0 {
		return HostSample{}, false
	}
	dIdle := float64(c.CPUIdle - prev.CPUIdle)
	s := HostSample{
		CPUPct:   (dTotal - dIdle) / dTotal * 100,
		MemBytes: c.MemUsed,
		DiskUsed: c.DiskUsed,
	}
	dt := c.At.Sub(prev.At).Seconds()
	if dt > 0 && c.RxBytes >= prev.RxBytes && c.TxBytes >= prev.TxBytes {
		s.RxBps = float64(c.RxBytes-prev.RxBytes) / dt
		s.TxBps = float64(c.TxBytes-prev.TxBytes) / dt
	}
	return s, true
}

// hostNet sums cumulative rx/tx over physical-looking interfaces from
// /proc/net/dev. Set HOST_PROC (e.g. /host/proc with -v /proc:/host/proc:ro)
// when stackr itself runs in a container, its own /proc/net/dev only sees
// the container netns.
func hostNet() (rx, tx uint64) {
	data, err := os.ReadFile(ProcNetPath("dev"))
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		// skip loopback and docker-side virtual interfaces (their traffic is
		// already counted on the physical uplink or is purely internal)
		if name == "lo" || strings.HasPrefix(name, "veth") || strings.HasPrefix(name, "br-") ||
			strings.HasPrefix(name, "docker") {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			continue
		}
		r, _ := strconv.ParseUint(f[0], 10, 64)
		t, _ := strconv.ParseUint(f[8], 10, 64)
		rx += r
		tx += t
	}
	return rx, tx
}

// ProcNetPath resolves a /proc/net file in the HOST network namespace.
// /proc/net symlinks to /proc/self/net, so through a HOST_PROC bind mount it
// would show the container's own netns, pid 1's net dir is the host's.
// Exported for the panel's conntrack flow sampler, which reads the same tree.
func ProcNetPath(file string) string {
	if procDir := os.Getenv("HOST_PROC"); procDir != "" {
		return procDir + "/1/net/" + file
	}
	return "/proc/net/" + file
}

// hostMemUsed returns MemTotal - MemAvailable from /proc/meminfo, in bytes.
func hostMemUsed() int64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	var total, avail int64
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.ParseInt(f[1], 10, 64)
		switch f[0] {
		case "MemTotal:":
			total = v * 1024
		case "MemAvailable:":
			avail = v * 1024
		}
	}
	if total == 0 || avail == 0 {
		return 0
	}
	return total - avail
}
