package tile

import (
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

// watcherOnly keys feed the image watcher or the build path and nothing that
// runs: they earn no effect (the old differ redeployed on them, a wart).
var watcherOnly = map[string]bool{"watch_paths": true, "update_policy": true, "tag_policy": true}

// Effect is what a write earns, cheapest first.
type Effect string

const (
	Route    Effect = "route"    // rewrite the proxy route only
	Redeploy Effect = "redeploy" // recreate the containers from the image on disk
)

// Effects: nothing moved (or only watcher keys) → nothing. A serving tile
// (service, image) always rewrites its route and redeploys unless only
// route keys moved. A managed instance has no route of its own.
func Effects(kind string, c Changed) []Effect {
	rest := Changed{}
	for k := range c {
		if !watcherOnly[k] {
			rest[k] = true
		}
	}
	if !rest.Any() {
		return nil
	}
	if kind == Managed {
		return []Effect{Redeploy}
	}
	if rest.ProxyOnly() {
		return []Effect{Route}
	}
	return []Effect{Route, Redeploy}
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
	return c
}

func canonPolicy(v string) string {
	if v == "" {
		return "manual"
	}
	return v
}
