// Package tile is the tile drawer: one templ per tab, each drawn from its
// view struct inside Drawer (v0's panel header with the tile's actions, the
// image-update strip, the tab strip, a refused action's message). Every
// answer replaces #drawer-view whole, so a tab's forms and confirms all
// target it.
package tile

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/FyrmForge/stackr/internal/ui/access"
	c "github.com/FyrmForge/stackr/internal/ui/components"
)

// Root is the id every drawer answer replaces (outerHTML).
const Root = c.DrawerRoot

// Drawer is every tile drawer answer: the shared drawer frame around one
// tab.
func Drawer(v View, body templ.Component) templ.Component {
	d := c.DrawerView{
		Node:     v.Node,
		Title:    v.Name,
		Kind:     v.Source,
		Base:     v.Base,
		Tabs:     v.tabs(),
		Labels:   labels,
		Tab:      v.Tab,
		Error:    v.Error,
		Note:     v.Note,
		Icon:     v.Kind,
		Status:   v.Status,
		Location: v.Location,
		EnvColor: v.EnvColor,
		Actions:  actions(v),
	}
	if v.NewDigest != "" {
		d.Strip = imageStrip(v)
	}
	return c.Drawer(d, body)
}

// View is the drawer around one tab.
type View struct {
	Node     string // the card's node id, for ?drawer=
	Name     string
	Kind     string // service | image | cron | function
	Source   string // the header subtitle: the image it runs or the repo it builds
	Base     string // the drawer URL: tabs add ?tab=, actions add /<verb>
	Tab      string
	Error    string // a refused action's message
	Note     string // what an action did ("redeploy queued")
	Status   string // a TileBadge word; "" = unknown
	Stopped  bool   // offer Start, not Stop / Restart
	Paused   bool   // cron: offer Resume
	Terminal bool   // the viewer may open a shell (tile.write): show the Terminal tab
	ReadOnly bool   // the viewer may not write: no action buttons, no Access or Settings tab
	Location string // "stack / env"
	EnvColor string
	Domained bool // the tile has a domain: only then does it see requests (Metrics tab)

	NewDigest  string // a newer image than the one running; "" = none
	AutoUpdate bool   // the watcher deploys it by itself
}

// Tabs are the tabs a kind has, in strip order. Variables, database
// access, placement and backups are rows of Settings. A key a kind lacks
// falls back to its first tab.
func Tabs(kind string) []string {
	if kind == "cron" || kind == "function" {
		return []string{"runs", "logs", "metrics", "jobs", "settings"}
	}
	return []string{"status", "jobs", "logs", "metrics", "settings"}
}

// tabs is Tabs plus Terminal after Logs, for a viewer who may shell in and
// a kind that keeps a container (cron and function tiles run per job).
func (v View) tabs() []string {
	t := Tabs(v.Kind)
	if i := slices.Index(t, "logs"); v.Terminal && v.Kind != "cron" && v.Kind != "function" {
		t = slices.Insert(slices.Clone(t), i+1, "terminal")
	}
	if v.ReadOnly {
		t = slices.DeleteFunc(slices.Clone(t), func(k string) bool { return k == "settings" })
	}
	return t
}

// Tab is the tab a request asks for, as the kind has it. terminal says
// the Terminal tab is offered.
func Tab(kind, tab string, terminal, readOnly bool) string {
	if !slices.Contains(View{Kind: kind, Terminal: terminal, ReadOnly: readOnly}.tabs(), tab) {
		return Tabs(kind)[0]
	}
	return tab
}

var labels = map[string]string{
	"status": "Overview",
	"jobs":   "Deployments",
}

type StatusView struct {
	Word     string
	Replicas []ReplicaView
	Job      *c.JobStatusView // the newest job; nil = none yet
	JobWhen  time.Time
	URLs     []DomainRow
	Port     string        // the container port; "" = none
	Ports    []string      // published host:container pairs
	Access   []access.Perm // elevated access held or waiting, strongest first
}

type ReplicaView struct{ ID, Name, State, Health string }

type LogsView struct {
	Replicas  []ReplicaView
	Container string // the replica followed; "" = the first
	Run       string // a run's log instead of a replica's
	Runs      bool   // a cron or function: its logs are its runs'
	Pane      c.LogPaneView
	History   string // the log viewer's URL; "" = log history is off
}

// TerminalView is the Terminal tab: the shell endpoint for the chosen
// replica.
type TerminalView struct {
	Replicas  []ReplicaView
	Container string // the replica shelled into; "" = none running
	URL       string // the terminal websocket endpoint, container included
}

type DomainsView struct {
	Rows    []DomainRow
	Admin   bool          // raw Caddy snippets are an admin's
	Managed []string      // the managed domains the tile may use: the add form's list
	Replace *ReplaceOffer // an add refused on a record stackr did not create
	NoIP    *NoIPRefusal  // an add refused on an ingress node with no public IP
	Port    string        // the tile's container port: the add form's prefill
	// AutoHost is the generated name the auto button would attach, "" = not known.
	AutoHost string
}

// preview is the address line before anything is typed: the form's own
// script keeps it live.
func (d DomainsView) preview() string {
	h := d.typed("host")
	if len(d.Managed) > 0 {
		h = d.Managed[0]
	}
	return "https://" + h
}

// ReplaceOffer is the "Replace it" question: Vals is the refused add's
// form, replace on, posted again on yes; Form is that form, drawn again
// open so a Cancel keeps what was typed.
type ReplaceOffer struct {
	Host, Vals string
	Form       map[string]string
	Adopt      bool // the record already points at our node: ask to manage it, not replace it
}

// NoIPRefusal names the ingress node with no public IP; Link opens its
// Network tab ("" = the viewer is not a server admin).
type NoIPRefusal struct {
	Node, Link string
}

// typed is the refused add's value of form key k, "" with none.
func (d DomainsView) typed(k string) string {
	if d.Replace == nil {
		return ""
	}
	return d.Replace.Form[k]
}

type DomainRow struct {
	ID, Host, Path, Port string
	HTTPS, Auto          bool
	Redirect             string
	Raw                  string
}

// RedirectCode is the status the Caddy route sends: a generated redirect
// (a rename's old host) is a 308, a declared one a 301.
func (r DomainRow) RedirectCode() string {
	if r.Auto {
		return "308"
	}
	return "301"
}

type EnvView struct {
	Rows []EnvRow
	Text string   // KEY=VALUE per line, for the editor
	Kept []string // multi-line values the editor cannot hold; they stay as they are
}

type EnvRow struct {
	Name, Value string
	Ref         bool // the value holds a ${{ }} reference
}

// AccessView is the slices a tile reaches: the entries its slice_access
// names, the creds it holds (a slice it only refs shows once bound), and
// the env's slice tiles for the add form.
type AccessView struct {
	Entries  []AccessEntry
	Bindings []AccessBinding
	Slices   []string
}

// AccessEntry is one slice_access entry; Link "" = no such slice tile.
type AccessEntry struct{ Slice, Link, Access string }

type AccessBinding struct {
	Slice, Link, Access, User string
	Since                     time.Time
}

type JobsView struct{ Rows []JobRow }

// Cancel is the cancel action's URL; "" = no button (finished, or no verb).
type JobRow struct {
	Kind, State, Error, Cancel string
	When                       time.Time
}

type ImageView struct {
	Ref, Digest, LastDigest, LastTag, LastError string
	Checked                                     time.Time // zero = never
}

// SettingsView is the Settings tab: one accordion row per group, each row
// its own form. What saves on its own (domains, variables, database
// access, ingress and exits) sits in its row.
type SettingsView struct {
	Name, Kind string
	Vals       map[string]string // form key → value; a key the kind does not carry is absent
	Errors     map[string]string // form key → why the save was refused
	From       map[string]string // an empty limit: the rung it inherits from ("org")
	Inherit    map[string]string // an empty limit: the value it inherits ("1 cores")
	Open       string            // the row open on load; "" = none
	Domains    *DomainsView      // nil = the kind has no endpoint
	Image      *ImageView        // nil = nothing pulled to watch
	Env        EnvView
	Access     AccessView
	Volumes    []VolumeRow
	Placement  string // the ingress and exits fragment's URL
	Ingress    string // where traffic comes in, "h4 (from env)"; "" = not known
}

// Has: the kind carries this key, so the form draws its field.
func (s SettingsView) Has(key string) bool {
	_, ok := s.Vals[key]
	return ok
}

// Any: the kind carries one of keys, so their row draws.
func (s SettingsView) Any(keys ...string) bool {
	return slices.ContainsFunc(keys, s.Has)
}

// Keys are the drawn fields among keys, posted back so a row's save
// writes only these (an unticked checkbox posts nothing, and still
// clears).
func (s SettingsView) Keys(keys ...string) string {
	ks := slices.DeleteFunc(slices.Clone(keys), func(k string) bool { return !s.Has(k) })
	slices.Sort(ks)
	return strings.Join(ks, " ")
}

// The rows' form keys. Shm size and depends on sit under Run's advanced.
var (
	sourceKeys   = []string{"git_url", "branch", "image", "update_policy", "tag_policy"}
	buildKeys    = []string{"dockerfile", "build_context", "watch_paths", "build_args"}
	scheduleKeys = []string{"schedule", "trigger", "timeout_minutes"}
	runKeys      = []string{"command", "user", "restart_policy", "replicas", "shm_size_mb", "depends_on"}
	networkKeys  = []string{"port", "endpoint_protocol"}
	storageKeys  = []string{"volumes", "files"}
	healthKeys   = []string{"health_path", "healthcheck_cmd", "healthcheck_interval_s", "healthcheck_timeout_s", "healthcheck_retries", "healthcheck_start_period_s"}
	resourceKeys = []string{"cpu_limit", "mem_limit_mb"}
	hostKeys     = []string{"privileged", "devices", "lan", "published_ports", "host_network"}
)

// defaults are what a field holds when nobody set it; every other key's
// default is empty.
var defaults = map[string]string{
	"replicas":          "1",
	"restart_policy":    "always",
	"update_policy":     "manual",
	"trigger":           "manual",
	"endpoint_protocol": "http",
	"dockerfile":        "Dockerfile",
	"build_context":     ".",
}

// identity keys say what the tile is, not how it differs: never marked.
var identity = map[string]bool{"git_url": true, "branch": true, "image": true, "schedule": true}

// Changed: the field differs from what the tile would hold unset.
func (s SettingsView) Changed(key string) bool {
	v := s.Vals[key]
	return !identity[key] && v != "" && v != defaults[key]
}

// builds: a repo tile, or a run kind pointed at a repo.
func (s SettingsView) builds() bool {
	return s.Any(buildKeys...) && (s.Kind == "service" || s.Vals["git_url"] != "")
}

// lines counts a textarea's non-empty lines.
func lines(v string) int {
	n := 0
	for l := range strings.SplitSeq(v, "\n") {
		if strings.TrimSpace(l) != "" {
			n++
		}
	}
	return n
}

// count is "no volumes", "1 volume", "2 volumes".
func count(n int, one, many string) string {
	switch n {
	case 0:
		return "no " + many
	case 1:
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// join is a summary's parts, the empty ones dropped.
func join(parts ...string) string {
	return strings.Join(slices.DeleteFunc(parts, func(p string) bool { return p == "" }), " · ")
}

func generalSummary(s SettingsView) string { return join(s.Name, s.Kind) }

func sourceSummary(s SettingsView) string {
	if u := s.Vals["git_url"]; u != "" {
		u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "git@")
		return join(u, cmp.Or(s.Vals["branch"], "default branch"))
	}
	pol := ""
	if s.Has("update_policy") {
		pol = "notify on a new version"
		if s.Vals["update_policy"] == "auto" {
			pol = "deploy new versions"
		}
	}
	return join(cmp.Or(s.Vals["image"], "no image"), pol)
}

func buildSummary(s SettingsView) string {
	args := ""
	if n := lines(s.Vals["build_args"]); n > 0 {
		args = count(n, "build arg", "build args")
	}
	return join(cmp.Or(s.Vals["dockerfile"], "Dockerfile"), "context "+cmp.Or(s.Vals["build_context"], "."), args)
}

func scheduleSummary(s SettingsView) string {
	when := s.Vals["schedule"]
	if s.Has("trigger") {
		when = "runs manually"
		if s.Vals["trigger"] == "on_deploy" {
			when = "runs on every deploy"
		}
	}
	most := ""
	if m := s.Vals["timeout_minutes"]; m != "" {
		most = "max " + m + " min"
	}
	return join(cmp.Or(when, "no schedule"), most)
}

var restartWords = map[string]string{"always": "restart always", "on-failure": "restart on failure", "no": "never restart"}

func runSummary(s SettingsView) string {
	cmdWord := "image command"
	if s.Vals["command"] != "" {
		cmdWord = "own command"
	}
	restart, reps := "", ""
	if s.Has("restart_policy") {
		restart = restartWords[cmp.Or(s.Vals["restart_policy"], "always")]
	}
	if s.Has("replicas") {
		n, _ := strconv.Atoi(s.Vals["replicas"])
		reps = count(max(n, 1), "replica", "replicas")
	}
	return join(cmdWord, restart, reps)
}

func networkSummary(s SettingsView) string {
	port := ""
	if s.Has("port") {
		port = "no port"
		if p := s.Vals["port"]; p != "" {
			port = p + " " + cmp.Or(s.Vals["endpoint_protocol"], "http")
		}
	}
	doms := ""
	if s.Domains != nil {
		doms = count(len(s.Domains.Rows), "domain", "domains")
	}
	ing := ""
	if s.Ingress != "" {
		ing = "ingress " + s.Ingress
	}
	return join(port, doms, ing)
}

func storageSummary(s SettingsView) string {
	return join(count(lines(s.Vals["volumes"]), "volume", "volumes"), count(lines(s.Vals["files"]), "file", "files"))
}

func healthSummary(s SettingsView) string {
	var what []string
	if p := s.Vals["health_path"]; p != "" {
		what = append(what, "GET "+p)
	}
	if s.Vals["healthcheck_cmd"] != "" {
		what = append(what, "a command")
	}
	if len(what) == 0 {
		return "no check"
	}
	every := ""
	if i := s.Vals["healthcheck_interval_s"]; i != "" {
		every = "every " + i + " s"
	}
	return join(strings.Join(what, " and "), every)
}

// limit is one limit in words: its own value, or what it inherits and
// from where.
func (s SettingsView) limit(key, one, many string) string {
	v, from := s.Vals[key], ""
	if v == "" {
		v, from = s.Inherit[key], s.From[key]
	}
	w := "no limit"
	switch v {
	case "", "0":
	case "1":
		w = v + " " + one
	default:
		w = v + " " + many
	}
	if from != "" {
		w += " (from " + from + ")"
	}
	return w
}

func resourcesSummary(s SettingsView) string {
	return join(s.limit("cpu_limit", "core", "cores"), s.limit("mem_limit_mb", "MB", "MB"))
}

func variablesSummary(s SettingsView) string {
	if len(s.Env.Rows) == 0 {
		return "none"
	}
	return strconv.Itoa(len(s.Env.Rows)) + " set"
}

func accessSummary(s SettingsView) string {
	var parts []string
	for _, e := range s.Access.Entries {
		parts = append(parts, e.Slice+" "+e.Access)
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

func hostSummary(s SettingsView) string {
	var parts []string
	if s.Vals["privileged"] != "" {
		parts = append(parts, "privileged")
	}
	if n := lines(s.Vals["devices"]); n > 0 {
		parts = append(parts, count(n, "device", "devices"))
	}
	if s.Vals["lan"] != "" {
		parts = append(parts, "LAN")
	}
	if n := lines(s.Vals["published_ports"]); n > 0 {
		parts = append(parts, count(n, "server port", "server ports"))
	}
	if s.Vals["host_network"] != "" {
		parts = append(parts, "host network")
	}
	if len(parts) == 0 {
		return "none, needs an admin"
	}
	return strings.Join(parts, ", ")
}

// settingsRows are the accordion's rows the kind has, in order; a row is
// marked when one of its fields is.
func settingsRows(v View, s SettingsView) []c.AccRow {
	all := []struct {
		on        bool
		key, name string
		sum       func(SettingsView) string
		keys      []string
		body      templ.Component
	}{
		{true, "general", "General", generalSummary, nil, general(v, s)},
		{s.Any(sourceKeys...), "source", "Source", sourceSummary, sourceKeys, sourceRow(v, s)},
		{s.builds(), "build", "Build", buildSummary, buildKeys, buildRow(v, s)},
		{s.Any(scheduleKeys...), "schedule", "Schedule", scheduleSummary, scheduleKeys, scheduleRow(v, s)},
		{s.Any(runKeys...), "run", "Run", runSummary, runKeys, runRow(v, s)},
		{true, "network", "Network", networkSummary, networkKeys, networkRow(v, s)},
		{s.Any(storageKeys...), "storage", "Storage", storageSummary, storageKeys, storageRow(v, s)},
		{s.Any(healthKeys...), "health", "Health", healthSummary, healthKeys, healthRow(v, s)},
		{s.Any(resourceKeys...), "resources", "Resources", resourcesSummary, resourceKeys, resourcesRow(v, s)},
		{true, "variables", "Variables", variablesSummary, nil, Env(v, s.Env)},
		{true, "dbaccess", "Database access", accessSummary, nil, Access(v, s.Access)},
		{s.Any(hostKeys...), "host", "Host access", hostSummary, hostKeys, hostRow(v, s)},
		{true, "delete", "Delete", func(SettingsView) string { return "" }, nil, deleteRow(v)},
	}
	var out []c.AccRow
	for _, r := range all {
		if r.on {
			out = append(out, c.AccRow{Key: r.key, Title: r.name, Summary: r.sum(s), Changed: slices.ContainsFunc(r.keys, s.Changed), Body: r.body})
		}
	}
	return out
}

type RunsView struct {
	Next time.Time // cron: the next tick; zero = paused or none
	Rows []RunRow
	Poll string // re-fetched every 2 s while a run is live; "" = idle
}

type RunRow struct {
	ID, Status, Trigger, Took, Exit string
	Started                         time.Time
	Mem, CPU                        string // peak memory and CPU time; "" = not measured
	Live                            bool   // offer stop
}

type VolumeRow struct{ ID, Name, State, Drawer string }
