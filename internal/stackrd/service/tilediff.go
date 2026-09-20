package service

import (
	"strings"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Changed is the set of config keys a write moved, in the config engine's
// vocabulary ("security_headers", "source", "schedule"). It is what decides
// which side effects a write earns.
type Changed map[string]bool

// Any reports whether anything at all moved.
func (c Changed) Any() bool { return len(c) > 0 }

// ProxyOnly reports whether everything that moved lives in the Traefik route
// rather than in the container spec. Those apply by rewriting the route;
// anything else needs the container rebuilt.
//
// Nothing changed is not proxy-only: a save that moved no field earns no
// redeploy, and the caller checks Any first.
func (c Changed) ProxyOnly() bool {
	for f := range c {
		switch {
		case f == "security_headers", f == "basic_auth_user",
			f == "basic_auth_password", f == "traefik_override":
		case strings.HasPrefix(f, "domain "), strings.HasPrefix(f, "domain +"),
			strings.HasPrefix(f, "domain -"):
			// A config apply names each domain it added, removed or changed
			// individually. They are route-only too: a host moving does not
			// change the container.
		default:
			return false
		}
	}
	return true
}

// NeedsBuild reports whether a changed field feeds the built artifact, which
// is the only reason to rebuild a run-to-completion tile: a cron reads its
// schedule, command and timeouts off the row at each run.
func (c Changed) NeedsBuild() bool {
	// Both "source" and "source_type": the config differ emits the first for
	// an image change and the second for a switch to a git build
	// (config/stackconf/plan.go), and DiffTiles emits "source". Listing both
	// costs nothing; missing one means a cron that switched source never
	// rebuilds.
	for _, f := range []string{"source", "source_type", "image", "git_url", "connector",
		"branch", "build_context", "dockerfile", "build_args"} {
		if c[f] {
			return true
		}
	}
	return false
}

// NeedsDBRedeploy reports whether a changed field is one a managed
// instance's container embodies. Restarting a database is disruptive enough
// that it should follow from a field that actually needs it.
//
// Both vocabularies are listed because both differs feed this: the config
// differ names cpu_limit and memory_mb separately where DiffTiles folds them
// into "limits", and only the config differ ever emits "env" (the file owns a
// managed instance's declared variables; no handler edits them).
//
// node_group and replicas are placement, which lives in the service spec
// exactly like the limits do. Leaving them out meant a group pin applied to a
// managed instance wrote the row and touched nothing, so the plan read
// "applied" with the database still scaled to zero on the node it was
// supposed to have left.
func (c Changed) NeedsDBRedeploy() bool {
	for _, f := range []string{"external_port", "cpu_limit", "memory_mb", "limits",
		"env", "image", "shm_size_mb", "node_group", "replicas"} {
		if c[f] {
			return true
		}
	}
	return false
}

// NeedsCronReload reports whether the scheduler's view of this tile moved.
func (c Changed) NeedsCronReload() bool {
	return c["schedule"] || c["command"] || c["source"] || c["image"] ||
		c["git_url"] || c["timeout_minutes"] || c["allow_overlap"]
}

// canonPolicy and canonRestart fold a field's spellings-of-the-same-thing
// together. Both enums have an empty spelling that means the default, and a
// row written before the service canonicalised on write holds it.
func canonPolicy(v string) string {
	if v == "" {
		return "off"
	}
	return v
}

func canonRestart(v string) string {
	n, err := runtime.NormalizeRestart(v)
	if err != nil {
		return v // not our problem here; Validate refuses it
	}
	return n
}

// DiffTiles names what moved between the stored row and the edited one, in
// the same vocabulary the config differ uses.
//
// This exists because the two HTTP surfaces had no notion of "what changed"
// at all: the panel wrote the row and unconditionally rewrote the route, the
// API wrote the row and did nothing, and only a config apply chose. So a port,
// limit, healthcheck, command or replica edit made anywhere but a config file
// took effect on the next *manual* deploy and not before — the headline class
// B row of point 3.
//
// Both sides must be rows loaded from the store and then edited, not freshly
// built structs: four columns (status, slug, shared_net, home_node) are
// excluded from UpdateTile by the store and would otherwise read as changes.
func DiffTiles(old, cur *repo.Tile) Changed {
	c := Changed{}
	if old == nil || cur == nil {
		return c
	}
	mark := func(name string, same bool) {
		if !same {
			c[name] = true
		}
	}
	// Canonical forms on both sides. Validate rewrites the edited row's
	// enums to their canonical spelling ("" -> "off"), and rows written
	// before that existed hold the raw one — so comparing them straight
	// reported a change the user never made, and redeployed on a save that
	// touched nothing.
	mark("source", old.SourceType == cur.SourceType)
	mark("image", old.ImageRef == cur.ImageRef)
	mark("git_url", old.GitURL == cur.GitURL)
	mark("branch", old.GitBranch == cur.GitBranch)
	mark("connector", old.ConnectorID == cur.ConnectorID)
	mark("dockerfile", old.DockerfilePath == cur.DockerfilePath)
	mark("build_context", old.BuildContext == cur.BuildContext)
	mark("build_args", old.BuildArgs == cur.BuildArgs)

	mark("port", old.ContainerPort == cur.ContainerPort)
	// A managed instance's published port. Missing here entirely until point
	// 5, so the only surface that redeployed on a port change was the one
	// that redeployed on every save.
	mark("external_port", old.ExternalPort == cur.ExternalPort)
	mark("published_ports", old.PublishedPorts == cur.PublishedPorts)
	mark("command", old.Command == cur.Command)
	mark("user", old.User == cur.User)
	mark("restart", canonRestart(old.RestartPolicy) == canonRestart(cur.RestartPolicy))
	mark("privileged", old.Privileged == cur.Privileged)
	mark("devices", old.Devices == cur.Devices)
	mark("shm_size_mb", old.ShmSizeMB == cur.ShmSizeMB)
	mark("depends_on", old.DependsOn == cur.DependsOn)

	mark("limits", old.CPULimit == cur.CPULimit && old.MemLimitMB == cur.MemLimitMB)
	mark("healthcheck", old.HealthcheckCmd == cur.HealthcheckCmd &&
		old.HealthcheckIntervalS == cur.HealthcheckIntervalS &&
		old.HealthcheckTimeoutS == cur.HealthcheckTimeoutS &&
		old.HealthcheckRetries == cur.HealthcheckRetries &&
		old.HealthcheckStartPeriodS == cur.HealthcheckStartPeriodS)

	mark("volumes", old.Volumes == cur.Volumes)
	mark("files", old.Files == cur.Files)
	mark("storage", old.Storage == cur.Storage)
	mark("watch_paths", old.WatchPaths == cur.WatchPaths)

	mark("replicas", old.Replicas == cur.Replicas)
	mark("node_group", old.NodeGroup == cur.NodeGroup)

	mark("schedule", old.Cron == cur.Cron)
	mark("timeout_minutes", old.TimeoutMinutes == cur.TimeoutMinutes)
	mark("allow_overlap", old.AllowOverlap == cur.AllowOverlap)
	mark("run_on_deploy", old.RunOnDeploy == cur.RunOnDeploy)

	mark("update_policy", canonPolicy(old.UpdatePolicy) == canonPolicy(cur.UpdatePolicy))
	mark("wait_for_ci", old.WaitForCI == cur.WaitForCI)

	// Route-only fields. Kept last, and named exactly as ProxyOnly expects.
	mark("security_headers", old.SecHeaders == cur.SecHeaders)
	mark("basic_auth_user", old.BasicAuthUser == cur.BasicAuthUser)
	mark("basic_auth_password", old.BasicAuthPassword == cur.BasicAuthPassword)
	mark("traefik_override", old.TraefikOverride == cur.TraefikOverride)
	return c
}
