package installspec

import (
	"strconv"
	"strings"
)

const (
	ObserveName   = "stackr-observe"
	ObserveVolume = "stackr-observe-data"
	ObserveRepo   = Repo + "-observe"
	// observeRun is where the container sees ObserveDir.
	observeRun = "/run/stackr-observe"
)

// ObserveImage is the observe image of a release, tagged like the panel's.
func ObserveImage(version string) string {
	return ObserveRepo + ":" + strings.TrimPrefix(version, "v")
}

// ObserveDir is the host directory holding the observe sockets: under the
// data dir, root only, like the registry's.
func ObserveDir(dataDir string) string { return dataDir + "/observe" }

// ObserveMetricsSocket is VictoriaMetrics' socket on the host.
func ObserveMetricsSocket(dataDir string) string { return ObserveDir(dataDir) + "/metrics.sock" }

// ObserveLogsSocket is VictoriaLogs' socket on the host.
func ObserveLogsSocket(dataDir string) string { return ObserveDir(dataDir) + "/logs.sock" }

// ObserveOpts are the knobs that shape the observe container: VictoriaMetrics'
// retention ("30d") and free-disk floor in bytes, and with Logs on
// VictoriaLogs' retention ("7d") and disk cap in bytes.
type ObserveOpts struct {
	Retention     string
	MinFree       uint64
	Logs          bool
	LogsRetention string
	LogsMax       uint64
}

// Observe is the metrics store: VictoriaMetrics under stackr's supervisor,
// no network, serving a unix socket under the data dir. On loopback a
// host-network tile would reach it; through the socket only stackrd does.
// With o.Logs it also runs VictoriaLogs on logs.sock.
func Observe(version, dataDir string, o ObserveOpts) Container {
	cmd := []string{
		"-retention=" + o.Retention,
		"-min-free=" + strconv.FormatUint(o.MinFree, 10),
	}
	if o.Logs {
		cmd = append(cmd, "-logs", "-logs-retention="+o.LogsRetention,
			"-logs-max-size="+strconv.FormatUint(o.LogsMax, 10))
	}
	c := Container{
		Name:    ObserveName,
		Image:   ObserveImage(version),
		Cmd:     cmd,
		Labels:  map[string]string{LabelRole: "observe"},
		Volumes: []string{ObserveVolume + ":/data", ObserveDir(dataDir) + ":" + observeRun},
		Network: "none",
		LogOpts: logCap,
		Restart: "unless-stopped",
	}
	c.Labels[LabelSpec] = c.Hash()
	return c
}
