package tile

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/robfig/cron/v3"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/registry"
	"github.com/FyrmForge/stackr/internal/service/internal/slug"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// The kinds. service builds from git, image runs a pulled image, managed
// is an instance whose definition comes from its engine. cron and function
// are run-to-completion tiles: built from git or run from an image, they
// keep no container up; a schedule (cron) or a trigger (function) runs one.
const (
	Service  = "service"
	Image    = "image"
	Managed  = "managed"
	Cron     = "cron"
	Function = "function"
)

// RunToCompletion: the kind's deploy stops at the artifact and a run starts
// the container (runpolicy's KeepAlive false).
func RunToCompletion(kind string) bool { return kind == Cron || kind == Function }

// Builds: the tile's artifact is a git build (a service, or a run kind with
// a git_url). The rest pull an image.
func Builds(t store.Tile) bool {
	return t.Kind == Service || (RunToCompletion(t.Kind) && t.GitURL != "")
}

// Pulls: the tile's artifact is a registry image it names (an image tile,
// or a run kind with an image). Managed pulls its engine's, not this.
func Pulls(t store.Tile) bool {
	return t.Kind == Image || (RunToCompletion(t.Kind) && t.GitURL == "")
}

// Function triggers.
const (
	Manual   = "manual"
	OnDeploy = "on_deploy"
)

// DefaultTimeout is a run's timeout when the row says 0 (stackconf: 0 = 30).
const DefaultTimeout = 30

// field is one request key and whether a row carries it.
type field struct {
	key string
	set func(t *store.Tile) bool
}

func str(f func(t *store.Tile) string) func(t *store.Tile) bool {
	return func(t *store.Tile) bool { return strings.TrimSpace(f(t)) != "" }
}

func obj(f func(t *store.Tile) string) func(t *store.Tile) bool {
	return func(t *store.Tile) bool {
		s := strings.TrimSpace(f(t))
		return s != "" && s != "{}"
	}
}

var fields = []field{
	{"git_url", str(func(t *store.Tile) string { return t.GitURL })},
	{"branch", str(func(t *store.Tile) string { return t.GitBranch })},
	{"dockerfile", str(func(t *store.Tile) string { return t.DockerfilePath })},
	{"build_context", str(func(t *store.Tile) string { return t.BuildContext })},
	{"build_args", obj(func(t *store.Tile) string { return t.BuildArgs })},
	{"watch_paths", str(func(t *store.Tile) string { return t.WatchPaths })},
	{"image", str(func(t *store.Tile) string { return t.ImageRef })},
	{"update_policy", func(t *store.Tile) bool { return t.UpdatePolicy == "auto" }},
	{"tag_policy", str(func(t *store.Tile) string { return t.TagPolicy })},
	{"command", str(func(t *store.Tile) string { return t.Command })},
	{"port", func(t *store.Tile) bool { return t.ContainerPort != 0 }},
	{"published_ports", str(func(t *store.Tile) string { return t.PublishedPorts })},
	{"endpoint_protocol", str(func(t *store.Tile) string { return t.EndpointProtocol })},
	{"health_path", str(func(t *store.Tile) string { return t.HealthPath })},
	{"healthcheck", func(t *store.Tile) bool {
		return t.HealthcheckCmd != "" || t.HealthcheckIntervalS != 0 || t.HealthcheckTimeoutS != 0 ||
			t.HealthcheckRetries != 0 || t.HealthcheckStartPeriodS != 0
	}},
	{"user", str(func(t *store.Tile) string { return t.User })},
	{"privileged", func(t *store.Tile) bool { return t.Privileged }},
	{"devices", str(func(t *store.Tile) string { return t.Devices })},
	{"files", str(func(t *store.Tile) string { return t.Files })},
	{"volumes", str(func(t *store.Tile) string { return t.Volumes })},
	{"depends_on", str(func(t *store.Tile) string { return t.DependsOn })},
	{"shared_net", str(func(t *store.Tile) string { return t.SharedNet })},
	{"replicas", func(t *store.Tile) bool { return t.Replicas > 1 }},
	{"env", obj(func(t *store.Tile) string { return t.EnvJSON })},
	{"limits", func(t *store.Tile) bool { return t.CPULimit != 0 || t.MemLimitMB != 0 }},
	{"shm_size_mb", func(t *store.Tile) bool { return t.ShmSizeMB != 0 }},
	{"schedule", str(func(t *store.Tile) string { return t.Schedule })},
	{"trigger", str(func(t *store.Tile) string { return t.Trigger })},
	{"paused", func(t *store.Tile) bool { return t.Paused }},
	{"timeout_minutes", func(t *store.Tile) bool { return t.TimeoutMinutes != 0 }},
}

// refusals are the kind refusals tilelifecycle words itself; any other key
// gets "<kind> tiles do not take <key>".
var refusals = map[string]string{
	"schedule":    "schedule applies to cron tiles only",
	"trigger":     "run_on_deploy applies to function tiles only",
	"paused":      "only cron tiles have a schedule to pause",
	"command":     "command does not apply to a %s",
	"port":        "a %s has no endpoint; port does not apply",
	"user":        "user applies to service tiles only",
	"privileged":  "privileged applies to service tiles only",
	"devices":     "devices apply to service tiles only",
	"healthcheck": "healthcheck applies to service tiles only",
}

func refuse(kind, key string) error {
	if msg, ok := refusals[key]; ok {
		if strings.Contains(msg, "%s") {
			return errs.Invalidf(key, msg, kind)
		}
		return errs.Invalidf(key, "%s", msg)
	}
	return errs.Invalidf(key, "%s tiles do not take %s", kind, key)
}

func keys(ks ...[]string) map[string]bool {
	m := map[string]bool{}
	for _, k := range ks {
		for _, s := range k {
			m[s] = true
		}
	}
	return m
}

var (
	buildKeys = []string{"git_url", "branch", "dockerfile", "build_context", "build_args", "watch_paths"}
	watchKeys = []string{"image", "update_policy", "tag_policy"}
	runKeys   = []string{
		"command",
		"port",
		"published_ports",
		"endpoint_protocol",
		"health_path",
		"healthcheck",
		"user",
		"privileged",
		"devices",
		"files",
		"volumes",
		"depends_on",
		"shared_net",
		"replicas",
		"env",
		"limits",
		"shm_size_mb",
	}
	// oneShotKeys: what a run-to-completion container takes. No endpoint, no
	// health gate, one container; the source is a git build or an image.
	oneShotKeys = []string{
		"image",
		"command",
		"timeout_minutes",
		"files",
		"volumes",
		"depends_on",
		"shared_net",
		"env",
		"limits",
		"shm_size_mb",
	}
)

// Carries is B26: the one whitelist of what each kind may carry. Restart
// policy is base, every kind has it. A managed instance's image, command,
// port and volumes come from its engine; the row carries only the knobs
// the engine leaves open (an image override, env, limits, published ports).
var Carries = map[string]map[string]bool{
	Service:  keys(buildKeys, runKeys),
	Image:    keys(watchKeys, runKeys),
	Managed:  keys([]string{"image", "env", "limits", "shm_size_mb", "published_ports"}),
	Cron:     keys(buildKeys, oneShotKeys, []string{"schedule", "paused"}),
	Function: keys(buildKeys, oneShotKeys, []string{"trigger"}),
}

// Validate is one gate over the finished row, create and update alike; first
// refusal wins. It also normalises in place: update_policy "" → manual,
// restart folded to its canonical word, replicas 0 → 1, empty JSON → {}.
func Validate(t *store.Tile) error {
	allowed, ok := Carries[t.Kind]
	if !ok {
		return errs.Invalidf("kind", "kind must be service, image, managed, cron or function")
	}
	for _, f := range fields {
		if f.set(t) && !allowed[f.key] {
			return refuse(t.Kind, f.key)
		}
	}
	switch t.Kind {
	case Service:
		if t.GitURL == "" {
			return errs.Invalidf("git_url", "a git source needs a git_url")
		}
		if !gitURL.MatchString(t.GitURL) {
			return errs.Invalidf("git_url", "use a GitHub URL: https://github.com/owner/repo or git@github.com:owner/repo")
		}
	case Image:
		if t.ImageRef == "" {
			return errs.Invalidf("image", "an image source needs an image (set git_url to switch to a git build)")
		}
	case Cron, Function:
		if err := checkRun(t); err != nil {
			return err
		}
	}
	if t.ImageRef != "" {
		if _, _, _, ok := registry.ParseRef(t.ImageRef); !ok {
			return errs.Invalidf("image", "%q is not an image reference", t.ImageRef)
		}
	}
	if t.UpdatePolicy == "" {
		t.UpdatePolicy = "manual"
	}
	if t.UpdatePolicy != "manual" && t.UpdatePolicy != "auto" {
		return errs.Invalidf("update_policy", "update_policy must be manual or auto")
	}
	rp, ok := canonRestart(t.RestartPolicy)
	if !ok {
		return errs.Invalidf("restart", "restart must be always, on-failure or no")
	}
	t.RestartPolicy = rp

	switch {
	case t.CPULimit < 0:
		return errs.Invalidf("limits.cpu", "cpu limit must not be negative")
	case t.MemLimitMB < 0:
		return errs.Invalidf("limits.memory_mb", "memory limit must not be negative")
	case t.ShmSizeMB < 0:
		return errs.Invalidf("shm_size_mb", "shm_size_mb must not be negative")
	case t.HealthcheckIntervalS < 0 || t.HealthcheckTimeoutS < 0 || t.HealthcheckRetries < 0 ||
		t.HealthcheckStartPeriodS < 0:
		return errs.Invalidf("healthcheck", "healthcheck knobs must not be negative")
	case t.Replicas < 0:
		return errs.Invalidf("replicas", "replicas: a whole number, at least 1")
	case t.TimeoutMinutes < 0:
		return errs.Invalidf("timeout_minutes", "timeout_minutes must not be negative")
	case t.ContainerPort < 0 || t.ContainerPort > 65535:
		return errs.Invalidf("port", "port must be between 1 and 65535")
	}
	if t.Replicas == 0 {
		t.Replicas = 1
	}
	if t.Replicas > 1 && strings.TrimSpace(t.Volumes) != "" {
		return errs.Invalidf("replicas", "this tile holds a volume, so it can only run one replica: two writers on one volume corrupt it")
	}
	return checkLists(t)
}

// checkRun: a cron or function builds from git or runs an image, exactly
// one; a cron's schedule parses; a function's trigger is a known word.
// Normalises trigger "" → manual and timeout 0 → the default.
func checkRun(t *store.Tile) error {
	switch {
	case t.GitURL != "" && t.ImageRef != "":
		return errs.Invalidf("image", "a %s builds from git_url or runs an image, not both", t.Kind)
	case t.GitURL == "" && t.ImageRef == "":
		return errs.Invalidf("git_url", "a %s needs a git_url or an image", t.Kind)
	case t.GitURL != "" && !gitURL.MatchString(t.GitURL):
		return errs.Invalidf("git_url", "use a GitHub URL: https://github.com/owner/repo or git@github.com:owner/repo")
	}
	if t.TimeoutMinutes == 0 {
		t.TimeoutMinutes = DefaultTimeout
	}
	if t.Kind == Cron {
		if strings.TrimSpace(t.Schedule) == "" {
			return errs.Invalidf("schedule", "a cron needs a schedule")
		}
		if _, err := cron.ParseStandard(t.Schedule); err != nil {
			return errs.Invalidf("schedule", "%s", err.Error())
		}
		return nil
	}
	if t.Trigger == "" {
		t.Trigger = Manual
	}
	if t.Trigger != Manual && t.Trigger != OnDeploy {
		return errs.Invalidf("trigger", "trigger must be manual or on_deploy")
	}
	return nil
}

var gitURL = regexp.MustCompile(`^(https://github\.com/|git@github\.com:)[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// canonRestart folds the spellings of the same policy: "", always and
// unless-stopped are all "always".
func canonRestart(v string) (string, bool) {
	switch v {
	case "", "always", "unless-stopped":
		return "always", true
	case "on-failure", "no":
		return v, true
	}
	return v, false
}

func checkLists(t *store.Tile) error {
	for _, blob := range []struct {
		key string
		v   *string
	}{
		{"env", &t.EnvJSON},
		{"build_args", &t.BuildArgs},
	} {
		if strings.TrimSpace(*blob.v) == "" {
			*blob.v = "{}"
		}
		var m map[string]string
		if err := json.Unmarshal([]byte(*blob.v), &m); err != nil {
			return errs.Invalidf(blob.key, "%s must be a map of names to strings", blob.key)
		}
		if blob.key == "env" {
			for k := range m {
				if !slug.ValidEnvKey(k) {
					return errs.Invalidf("env", "%q is not an env var name: letters, digits and _, not starting with a digit", k)
				}
			}
		}
	}
	for _, list := range []struct {
		key   string
		v     string
		parse func(string) error
	}{
		{"volumes", t.Volumes, parseMount},
		{"files", t.Files, func(l string) error {
			_, _, err := ParseFileMount(l)
			return err
		}},
		{"devices", t.Devices, func(l string) error {
			_, err := ParseDevice(l)
			return err
		}},
		{"depends_on", t.DependsOn, func(l string) error {
			_, _, err := ParseDep(l)
			return err
		}},
		{"published_ports", t.PublishedPorts, parsePorts},
	} {
		for _, l := range Lines(list.v) {
			if err := list.parse(l); err != nil {
				return errs.Invalidf(list.key, "%s", err.Error())
			}
		}
	}
	return nil
}

// Lines is the tile-column list convention: one item per line, blanks dropped.
func Lines(s string) []string {
	out := []string{}
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// parseMount: "volume-slug:/container/path[:ro]".
func parseMount(l string) error {
	parts := strings.Split(l, ":")
	if len(parts) < 2 || len(parts) > 3 || !slug.Valid(parts[0]) || !path.IsAbs(parts[1]) ||
		(len(parts) == 3 && parts[2] != "ro") {
		return fmt.Errorf("%q: want volume:/container/path[:ro]", l)
	}
	return nil
}

// parsePorts: "host:container", both 1-65535.
func parsePorts(l string) error {
	h, c, ok := strings.Cut(l, ":")
	if !ok || !port(h) || !port(c) {
		return fmt.Errorf("%q: want hostport:containerport", l)
	}
	return nil
}

func port(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n > 0 && n < 65536
}

// ParseDevice: "host[:container[:perms]]"; container defaults to host,
// perms to rwm; both paths absolute.
func ParseDevice(l string) (docker.Device, error) {
	parts := strings.Split(l, ":")
	d := docker.Device{Host: parts[0], Container: parts[0], Perms: "rwm"}
	if len(parts) > 1 && parts[1] != "" {
		d.Container = parts[1]
	}
	if len(parts) > 2 {
		d.Perms = parts[2]
	}
	if len(parts) > 3 || !path.IsAbs(d.Host) || !path.IsAbs(d.Container) ||
		d.Perms == "" || strings.Trim(d.Perms, "rwm") != "" {
		return d, fmt.Errorf("%q: want /dev/host[:/dev/container[:rwm]]", l)
	}
	return d, nil
}

// ParseFileMount: "repo/path:/container/path[:template]". The source is
// repo-relative: no "..", not absolute, not ".".
func ParseFileMount(l string) (src, dst string, err error) {
	parts := strings.Split(l, ":")
	if len(parts) < 2 || len(parts) > 3 || (len(parts) == 3 && parts[2] != "template") {
		return "", "", fmt.Errorf("%q: want repo/path:/container/path[:template]", l)
	}
	src, dst = path.Clean(parts[0]), parts[1]
	if src == "." || path.IsAbs(src) || src == ".." || strings.HasPrefix(src, "../") || !path.IsAbs(dst) {
		return "", "", fmt.Errorf("%q: the source is a path inside the repo, the target an absolute path", l)
	}
	return src, dst, nil
}

// ParseDep: "slug" or "slug:condition"; bare means started.
func ParseDep(l string) (slugName, cond string, err error) {
	slugName, cond, _ = strings.Cut(l, ":")
	if cond == "" {
		cond = "started"
	}
	if !slug.Valid(slugName) || (cond != "started" && cond != "healthy" && cond != "completed") {
		return "", "", fmt.Errorf("%q: want tile[:started|healthy|completed]", l)
	}
	return slugName, cond, nil
}
