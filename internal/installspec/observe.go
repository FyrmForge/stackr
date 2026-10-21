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

// Observe is the metrics store: VictoriaMetrics under stackr's supervisor,
// no network, serving a unix socket under the data dir. On loopback a
// host-network tile would reach it; through the socket only stackrd does.
// retention is VictoriaMetrics' -retentionPeriod ("30d"), minFree the bytes
// of free disk below which it stops writing.
func Observe(version, dataDir, retention string, minFree uint64) Container {
	c := Container{
		Name:  ObserveName,
		Image: ObserveImage(version),
		Cmd: []string{
			"-retention=" + retention,
			"-min-free=" + strconv.FormatUint(minFree, 10),
		},
		Labels:  map[string]string{LabelRole: "observe"},
		Volumes: []string{ObserveVolume + ":/data", ObserveDir(dataDir) + ":" + observeRun},
		Network: "none",
		LogOpts: logCap,
		Restart: "unless-stopped",
	}
	c.Labels[LabelSpec] = c.Hash()
	return c
}
