package tile

import (
	"slices"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Changed is the set of keys a write moved, in the request/file vocabulary
// ("source", "port", "limits"), plus "domain …" keys a caller adds.
type Changed map[string]bool

func (c Changed) Any() bool { return len(c) > 0 }

// ProxyOnly: everything that moved lives in the route. Vacuously true on an
// empty set, so check Any first. The default arm is fail-safe: an
// unclassified key earns a redeploy, never a silent no-op.
func (c Changed) ProxyOnly() bool {
	for f := range c {
		if !strings.HasPrefix(f, "domain ") {
			return false
		}
	}
	return true
}

// NeedsBuild: a key that feeds the built artifact. A save never rebuilds
// (builds come from a push, a button or the API); the release differ asks
// this to know whether a change needs one.
func (c Changed) NeedsBuild() bool {
	for _, f := range []string{"source", "image", "git_url", "branch", "build_context", "dockerfile", "build_args"} {
		if c[f] {
			return true
		}
	}
	return false
}

// NeedsCronReload: the scheduler's view of the tile moved. Command and
// timeouts are read off the row at each run, so they need no reload; a
// kind change ("source") adds a cron. One leaving cron is the caller's to
// see (it holds the old kind).
func (c Changed) NeedsCronReload() bool { return c["schedule"] || c["paused"] || c["source"] }

// watcherOnly keys feed the image watcher or the build path and nothing that
// runs: they earn no effect (the old differ redeployed on them, a wart).
var watcherOnly = map[string]bool{"watch_paths": true, "update_policy": true, "tag_policy": true}

// Effect is what a write earns, cheapest first.
type Effect string

const (
	Route      Effect = "route"       // rewrite the proxy route only
	Redeploy   Effect = "redeploy"    // recreate the containers from the image on disk
	CronReload Effect = "cron-reload" // rebuild the schedule table
)

// Effects: nothing moved (or only watcher keys) → nothing. A serving tile
// (service, image) always rewrites its route and redeploys unless only
// route keys moved. A managed instance has no route of its own. A run-to-
// completion tile reads schedule, command and timeouts off the row at each
// run: it earns a redeploy only for a build key, and a cron reload when
// the schedule table's view moved.
func Effects(kind string, c Changed) []Effect {
	var out []Effect
	if kind == Slice {
		// No container: a moved target or default reaches the consumers,
		// and the plan redeploys them.
		return nil
	}
	if kind == Cron && c.NeedsCronReload() {
		out = append(out, CronReload)
	}
	if RunToCompletion(kind) {
		// slice_access: the binding's access is minted at deploy.
		if c.NeedsBuild() || c["slice_access"] {
			out = append(out, Redeploy)
		}
		return out
	}
	rest := Changed{}
	for k := range c {
		if !watcherOnly[k] {
			rest[k] = true
		}
	}
	switch {
	case !rest.Any():
		return out
	case kind == Managed:
		return append(out, Redeploy)
	case rest.ProxyOnly():
		return append(out, Route)
	}
	return append(out, Route, Redeploy)
}

// Diff names what moved between the stored row and the edited one. Both must
// be loaded-then-edited rows: identity and timestamps are not compared.
func Diff(old, cur store.Tile) Changed {
	c := Changed{}
	mark := func(name string, same bool) {
		if !same {
			c[name] = true
		}
	}
	oldR, _ := canonRestart(old.RestartPolicy)
	curR, _ := canonRestart(cur.RestartPolicy)
	mark("source", old.Kind == cur.Kind)
	mark("image", old.ImageRef == cur.ImageRef)
	mark("git_url", old.GitURL == cur.GitURL)
	mark("branch", old.GitBranch == cur.GitBranch)
	mark("dockerfile", old.DockerfilePath == cur.DockerfilePath)
	mark("build_context", old.BuildContext == cur.BuildContext)
	mark("build_args", old.BuildArgs == cur.BuildArgs)
	mark("watch_paths", old.WatchPaths == cur.WatchPaths)
	mark("update_policy", canonPolicy(old.UpdatePolicy) == canonPolicy(cur.UpdatePolicy))
	mark("tag_policy", old.TagPolicy == cur.TagPolicy)
	mark("env", old.EnvJSON == cur.EnvJSON)
	mark("volumes", old.Volumes == cur.Volumes)
	mark("command", old.Command == cur.Command)
	mark("port", old.ContainerPort == cur.ContainerPort)
	mark("published_ports", old.PublishedPorts == cur.PublishedPorts)
	mark("endpoint_protocol", old.EndpointProtocol == cur.EndpointProtocol)
	mark("health_path", old.HealthPath == cur.HealthPath)
	mark("healthcheck", old.HealthcheckCmd == cur.HealthcheckCmd &&
		old.HealthcheckIntervalS == cur.HealthcheckIntervalS &&
		old.HealthcheckTimeoutS == cur.HealthcheckTimeoutS &&
		old.HealthcheckRetries == cur.HealthcheckRetries &&
		old.HealthcheckStartPeriodS == cur.HealthcheckStartPeriodS)
	mark("limits", old.CPULimit == cur.CPULimit && old.MemLimitMB == cur.MemLimitMB)
	mark("user", old.User == cur.User)
	mark("shm_size_mb", old.ShmSizeMB == cur.ShmSizeMB)
	mark("privileged", old.Privileged == cur.Privileged)
	mark("devices", old.Devices == cur.Devices)
	mark("restart", oldR == curR)
	mark("depends_on", old.DependsOn == cur.DependsOn)
	mark("files", old.Files == cur.Files)
	mark("shared_net", old.SharedNet == cur.SharedNet)
	mark("replicas", old.Replicas == cur.Replicas)
	mark("schedule", old.Schedule == cur.Schedule)
	mark("trigger", old.Trigger == cur.Trigger)
	mark("paused", old.Paused == cur.Paused)
	mark("timeout_minutes", old.TimeoutMinutes == cur.TimeoutMinutes)
	mark("provision_from", deref(old.ProvisionFrom) == deref(cur.ProvisionFrom))
	mark("default_access", deref(old.DefaultAccess) == deref(cur.DefaultAccess))
	mark("slice_access", slices.Equal(old.SliceAccess, cur.SliceAccess))
	return c
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func canonPolicy(v string) string {
	if v == "" {
		return "manual"
	}
	return v
}
