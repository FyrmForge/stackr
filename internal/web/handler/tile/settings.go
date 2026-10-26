package tile

import (
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	ui "github.com/FyrmForge/stackr/internal/ui/drawer/tile"
)

// The settings form's keys per kind. ponytail: a mirror of the tile leaf's
// Carries (internal to the service); Validate is still the gate, this only
// picks which fields the form draws. env is the Variables tab's, paused the
// header's.
var (
	buildForm   = []string{"git_url", "branch", "dockerfile", "build_context", "build_args", "watch_paths"}
	watchForm   = []string{"image", "update_policy", "tag_policy"}
	oneShotForm = []string{"image", "command", "timeout_minutes", "files", "volumes", "depends_on", "cpu_limit", "mem_limit_mb", "shm_size_mb"}
	runForm     = []string{
		"command",
		"port",
		"published_ports",
		"endpoint_protocol",
		"health_path",
		"healthcheck_cmd",
		"healthcheck_interval_s",
		"healthcheck_timeout_s",
		"healthcheck_retries",
		"healthcheck_start_period_s",
		"user",
		"privileged",
		"devices",
		"lan",
		"host_network",
		"files",
		"volumes",
		"depends_on",
		"replicas",
		"cpu_limit",
		"mem_limit_mb",
		"shm_size_mb",
		"restart_policy",
	}
	carries = map[string][]string{
		"service":  slices.Concat(buildForm, runForm),
		"image":    slices.Concat(watchForm, runForm),
		"cron":     slices.Concat(buildForm, oneShotForm, []string{"schedule"}),
		"function": slices.Concat(buildForm, oneShotForm, []string{"trigger"}),
	}
)

// field is the row column a form key edits: a *string, *int, *float64 or
// *bool into t; nil = not a settings key.
func field(t *service.Tile, key string) any {
	switch key {
	case "git_url":
		return &t.GitURL
	case "branch":
		return &t.GitBranch
	case "dockerfile":
		return &t.DockerfilePath
	case "build_context":
		return &t.BuildContext
	case "build_args":
		return &t.BuildArgs
	case "watch_paths":
		return &t.WatchPaths
	case "image":
		return &t.ImageRef
	case "update_policy":
		return &t.UpdatePolicy
	case "tag_policy":
		return &t.TagPolicy
	case "command":
		return &t.Command
	case "port":
		return &t.ContainerPort
	case "published_ports":
		return &t.PublishedPorts
	case "endpoint_protocol":
		return &t.EndpointProtocol
	case "health_path":
		return &t.HealthPath
	case "healthcheck_cmd":
		return &t.HealthcheckCmd
	case "healthcheck_interval_s":
		return &t.HealthcheckIntervalS
	case "healthcheck_timeout_s":
		return &t.HealthcheckTimeoutS
	case "healthcheck_retries":
		return &t.HealthcheckRetries
	case "healthcheck_start_period_s":
		return &t.HealthcheckStartPeriodS
	case "user":
		return &t.User
	case "privileged":
		return &t.Privileged
	case "devices":
		return &t.Devices
	case "lan":
		return &t.Lan
	case "host_network":
		return &t.HostNetwork
	case "files":
		return &t.Files
	case "volumes":
		return &t.Volumes
	case "depends_on":
		return &t.DependsOn
	case "replicas":
		return &t.Replicas
	case "cpu_limit":
		return &t.CPULimit
	case "mem_limit_mb":
		return &t.MemLimitMB
	case "shm_size_mb":
		return &t.ShmSizeMB
	case "restart_policy":
		return &t.RestartPolicy
	case "schedule":
		return &t.Schedule
	case "trigger":
		return &t.Trigger
	case "timeout_minutes":
		return &t.TimeoutMinutes
	}
	return nil
}

// shown is a column as its field shows it: 0 and false are empty, build
// args are KEY=VALUE lines.
func shown(key string, p any) string {
	switch p := p.(type) {
	case *string:
		if key == "build_args" {
			return envView(*p).Text
		}
		return *p
	case *int:
		if *p == 0 {
			return ""
		}
		return strconv.Itoa(*p)
	case *float64:
		if *p == 0 {
			return ""
		}
		return strconv.FormatFloat(*p, 'f', -1, 64)
	case *bool:
		if *p {
			return "1"
		}
	}
	return ""
}

// write parses what the field posted into its column.
func write(key string, p any, v string) error {
	v = strings.TrimSpace(v)
	switch p := p.(type) {
	case *string:
		if key != "build_args" {
			*p = v
			return nil
		}
		blob, err := withLines(key, *p, v)
		*p = blob
		return err
	case *int:
		if v == "" {
			*p = 0
			return nil
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return errs.Invalidf(key, "Use a whole number.")
		}
		*p = n
	case *float64:
		if v == "" {
			*p = 0
			return nil
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return errs.Invalidf(key, "Use a number.")
		}
		*p = f
	case *bool:
		*p = v != ""
	}
	return nil
}

// settingsView is the rows over the stored tile: one value per key the
// kind carries.
func settingsView(t service.Tile) ui.SettingsView {
	s := ui.SettingsView{Name: t.Name, Kind: t.Kind, Vals: map[string]string{}}
	for _, k := range carries[t.Kind] {
		s.Vals[k] = shown(k, field(&t, k))
	}
	return s
}

// posted are the keys a save writes: the form's drawn fields (an unticked
// box posts nothing and still clears) plus any settings key posted.
func posted(form map[string][]string) []string {
	keys := strings.Fields(first(form["fields"]))
	for k := range form {
		if !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	return keys
}

func first(vs []string) string {
	if len(vs) == 0 {
		return ""
	}
	return vs[0]
}

// apply writes the posted keys onto t; every refused key at once.
func apply(t *service.Tile, keys []string, form map[string][]string) error {
	var bad []error
	for _, k := range keys {
		if p := field(t, k); p != nil {
			if err := write(k, p, first(form[k])); err != nil {
				bad = append(bad, err)
			}
		}
	}
	return errors.Join(bad...)
}

// formKey is the form field a service refusal names (Validate speaks the
// stack file's keys).
func formKey(field string) string {
	switch field {
	case "limits.cpu":
		return "cpu_limit"
	case "limits.memory_mb":
		return "mem_limit_mb"
	case "restart":
		return "restart_policy"
	case "healthcheck":
		return "healthcheck_cmd"
	case "healthcheck_interval", "healthcheck_timeout", "healthcheck_start_period":
		return field + "_s"
	case "git_branch":
		return "branch"
	}
	return field
}

// joinMounts folds the Storage row's mount rows and its host and share
// lines into the one volumes value the tile stores. A row with neither a
// volume nor a path is the blank one, left out.
func joinMounts(form url.Values) {
	if !form.Has("mount_path") {
		return
	}
	vols, paths, modes := form["mount_volume"], form["mount_path"], form["mount_mode"]
	var ls []string
	for i, p := range paths {
		name, p := at(vols, i), strings.TrimSpace(p)
		if name == "" && p == "" {
			continue
		}
		l := name + ":" + p
		if at(modes, i) == "ro" {
			l += ":ro"
		}
		ls = append(ls, l)
	}
	for l := range strings.SplitSeq(form.Get("mount_other"), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			ls = append(ls, l)
		}
	}
	form["volumes"] = []string{strings.Join(ls, "\n")}
	for _, k := range []string{"mount_volume", "mount_path", "mount_mode", "mount_other"} {
		delete(form, k)
	}
}

func at(vs []string, i int) string {
	if i < len(vs) {
		return vs[i]
	}
	return ""
}

// mountRows splits a volumes value into its volume lines, as rows, and
// the rest (host: and share: lines, or a line that does not parse).
func mountRows(v string) (rows []ui.MountRow, other string) {
	var rest []string
	for l := range strings.SplitSeq(v, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if name, path, ok := service.MountedVolume(l); ok {
			rows = append(rows, ui.MountRow{Volume: name, Path: path, RO: strings.HasSuffix(l, ":ro")})
		} else {
			rest = append(rest, l)
		}
	}
	return rows, strings.Join(rest, "\n")
}
