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
		"health_http_path",
		"health_http_status",
		"health_http_interval_s",
		"health_http_timeout_s",
		"health_http_retries",
		"health_exec_cmd",
		"health_exec_interval_s",
		"health_exec_timeout_s",
		"health_exec_retries",
		"health_exec_start_period_s",
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
	case "health_http_path":
		return &t.HealthHTTPPath
	case "health_http_status":
		return &t.HealthHTTPStatus
	case "health_http_interval_s":
		return &t.HealthHTTPIntervalS
	case "health_http_timeout_s":
		return &t.HealthHTTPTimeoutS
	case "health_http_retries":
		return &t.HealthHTTPRetries
	case "health_exec_cmd":
		return &t.HealthExecCmd
	case "health_exec_interval_s":
		return &t.HealthExecIntervalS
	case "health_exec_timeout_s":
		return &t.HealthExecTimeoutS
	case "health_exec_retries":
		return &t.HealthExecRetries
	case "health_exec_start_period_s":
		return &t.HealthExecStartPeriodS
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
		if key == "health_http_status" && *p <= 5 {
			return strconv.Itoa(*p) + "xx"
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
		if key == "health_http_status" {
			n, err := parseStatus(v)
			*p = n
			return err
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

// parseStatus reads the Expected status field: a code 200 to 599 or a class
// 1xx to 5xx, stored as the int the stack file keeps (a class is its digit).
func parseStatus(v string) (int, error) {
	if len(v) == 3 && strings.EqualFold(v[1:], "xx") && v[0] >= '1' && v[0] <= '5' {
		return int(v[0] - '0'), nil
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 200 && n <= 599 {
		return n, nil
	}
	return 0, errs.Invalidf("health_http_status", "use a status code like 200 or a class like 2xx")
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
	bad := map[string]error{}
	for _, k := range keys {
		if p := field(t, k); p != nil {
			if err := write(k, p, first(form[k])); err != nil {
				bad[k] = err
			}
		}
	}
	// An empty path or command turns its check off: the rest of that check
	// clears with it, so Save never meets "needs a path" / "needs a cmd" and
	// never refuses a field of a check that is going away.
	off := func(lead string, clear ...*int) {
		if !slices.Contains(keys, lead) {
			return
		}
		if p, _ := field(t, lead).(*string); p != nil && *p == "" {
			for _, c := range clear {
				*c = 0
			}
			for k := range bad {
				if strings.HasPrefix(k, lead[:strings.LastIndex(lead, "_")+1]) {
					delete(bad, k)
				}
			}
		}
	}
	off("health_http_path", &t.HealthHTTPStatus, &t.HealthHTTPIntervalS, &t.HealthHTTPTimeoutS, &t.HealthHTTPRetries)
	off("health_exec_cmd", &t.HealthExecIntervalS, &t.HealthExecTimeoutS, &t.HealthExecRetries, &t.HealthExecStartPeriodS)
	var out []error
	for _, k := range keys { // keys order, not map order
		if bad[k] != nil {
			out = append(out, bad[k])
		}
	}
	return errors.Join(out...)
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
	case "health.http", "health.http.path":
		return "health_http_path"
	case "health.exec", "health.exec.cmd":
		return "health_exec_cmd"
	case "health.http.status":
		return "health_http_status"
	case "health.http.interval", "health.http.timeout", "health.exec.interval", "health.exec.timeout", "health.exec.start_period":
		return strings.NewReplacer(".", "_").Replace(field) + "_s"
	case "health.http.retries", "health.exec.retries":
		return strings.NewReplacer(".", "_").Replace(field)
	case "git_branch":
		return "branch"
	case "image_ref": // an operated tile's image is changed on its own (message 27)
		return "image"
	}
	return field
}

// joinMounts folds the Storage row's mount rows and its host and share
// lines into the one volumes value the tile stores. A row with neither a
// volume nor a path is the blank one, left out. The mount_ keys stay, so a
// refused save draws the rows as typed (typedMounts).
func joinMounts(form url.Values) {
	if !form.Has("mount_path") {
		return
	}
	var ls []string
	rows, other := postedMounts(form)
	for _, r := range rows {
		l := r.Volume + ":" + r.Path
		if r.RO {
			l += ":ro"
		}
		ls = append(ls, l)
	}
	form["volumes"] = []string{strings.Join(append(ls, other...), "\n")}
}

// postedMounts are the posted mount rows, the blank ones left out, and the
// host and share lines; rows numbered by their line in the joined value.
func postedMounts(form url.Values) (rows []ui.MountRow, other []string) {
	vols, paths, modes := form["mount_volume"], form["mount_path"], form["mount_mode"]
	for i, p := range paths {
		name, p := at(vols, i), strings.TrimSpace(p)
		if name == "" && p == "" {
			continue
		}
		rows = append(rows, ui.MountRow{Volume: name, Path: p, RO: at(modes, i) == "ro", Line: len(rows) + 1})
	}
	for l := range strings.SplitSeq(form.Get("mount_other"), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			other = append(other, l)
		}
	}
	return rows, other
}

// typedMounts are the Storage rows as a refused save posted them: each row
// where it was typed, the host and share lines in their box.
func typedMounts(form url.Values) (m ui.Mounts) {
	rows, other := postedMounts(form)
	m.Rows, m.Other = rows, strings.Join(other, "\n")
	for i := range other {
		m.OtherLines = append(m.OtherLines, len(rows)+i+1)
	}
	return m
}

func at(vs []string, i int) string {
	if i < len(vs) {
		return vs[i]
	}
	return ""
}

// mountRows splits a volumes value into its volume lines, as rows (one
// that does not parse too, so a refused row stays where it was typed), and
// the host: and share: lines.
func mountRows(v string) (m ui.Mounts) {
	var rest []string
	n := 0
	for l := range strings.SplitSeq(v, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		n++
		name, path, ok := service.MountedVolume(l)
		switch {
		case ok:
		case strings.HasPrefix(l, "host:"), strings.HasPrefix(l, "share:"):
			rest = append(rest, l)
			m.OtherLines = append(m.OtherLines, n)
			continue
		default: // a row typed half ("uploads:") stays a row
			name, path, _ = strings.Cut(l, ":")
			path = strings.TrimSuffix(path, ":ro")
		}
		m.Rows = append(m.Rows, ui.MountRow{Volume: name, Path: path, RO: strings.HasSuffix(l, ":ro"), Line: n})
	}
	m.Other = strings.Join(rest, "\n")
	return m
}
