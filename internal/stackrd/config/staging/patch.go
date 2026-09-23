package staging

import (
	"strings"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// splitLines is the tile-column convention: one item per line, blanks
// dropped. The config side calls the same shape splitTileLines.
func splitLines(s string) []string {
	out := []string{}
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// SettingsPatch builds the sparse config patch for a staged settings edit
// from the (in-memory, edited) tile. It carries every settings-owned field so
// that a cleared field applies, and deliberately omits env + domains, which
// keep their own staged groups (SaveEnv / the domain handlers).
//
// It lives here rather than beside the form that used to build it because
// the API stages too now: a config-managed stack that declares
// `ui_edits: stage` forces every surface into the pending set, and two
// builders would be two answers to "what did this edit change".
//
// Its keys are TileConf's yaml keys and it has to stay in step with
// stackconf.tileToConf, which is the same mapping in the other direction.
// It cannot simply marshal a TileConf: every field there is `omitempty`, so
// a cleared value would vanish from the patch instead of clearing anything.
func SettingsPatch(a *repo.Tile) map[string]any {
	p := map[string]any{"limits": map[string]any{"cpu": a.CPULimit, "memory_mb": a.MemLimitMB}}
	if a.Kind == "cron" {
		p["type"] = "cron"
		p["image"] = a.ImageRef
		p["schedule"] = a.Cron
		p["command"] = a.Command
		p["update_policy"] = a.UpdatePolicy
		p["allow_overlap"] = a.AllowOverlap
		p["timeout_minutes"] = a.TimeoutMinutes
		return p
	}
	if a.Kind == "function" {
		p["type"] = "function"
		p["image"] = a.ImageRef
		p["command"] = a.Command
		p["update_policy"] = a.UpdatePolicy
		p["run_on_deploy"] = a.RunOnDeploy
		p["allow_overlap"] = a.AllowOverlap
		p["timeout_minutes"] = a.TimeoutMinutes
		p["depends_on"] = splitLines(a.DependsOn)
		return p
	}
	p["port"] = a.ContainerPort
	p["healthcheck"] = a.HealthcheckCmd
	p["healthcheck_interval"] = a.HealthcheckIntervalS
	p["healthcheck_timeout"] = a.HealthcheckTimeoutS
	p["healthcheck_retries"] = a.HealthcheckRetries
	p["healthcheck_start_period"] = a.HealthcheckStartPeriodS
	p["command"] = a.Command
	p["user"] = a.User
	p["shm_size_mb"] = a.ShmSizeMB
	p["privileged"] = a.Privileged
	p["devices"] = splitLines(a.Devices)
	p["restart"] = a.RestartPolicy
	p["depends_on"] = splitLines(a.DependsOn)
	p["security_headers"] = a.SecHeaders
	p["volumes"] = splitLines(a.Volumes)
	p["files"] = splitLines(a.Files)
	p["storage"] = splitLines(a.Storage)
	p["watch_paths"] = splitLines(a.WatchPaths)
	p["build_args"] = a.BuildArgs
	p["published_ports"] = a.PublishedPorts
	p["traefik_override"] = a.TraefikOverride
	p["update_policy"] = a.UpdatePolicy
	p["wait_for_ci"] = a.WaitForCI
	p["basic_auth_user"] = a.BasicAuthUser
	p["basic_auth_password"] = a.BasicAuthPassword
	switch a.SourceType {
	case "image":
		p["image"] = a.ImageRef
	default: // git
		p["image"] = ""
		p["git_url"] = a.GitURL
		p["connector"] = a.ConnectorID
		p["branch"] = a.GitBranch
		p["build"] = map[string]any{"context": a.BuildContext, "dockerfile": a.DockerfilePath}
	}
	return p
}
