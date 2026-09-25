// Package tile is the tile drawer: one templ per tab, each drawn from its
// view struct inside Drawer (v0's panel header with the tile's actions, the
// image-update strip, the tab strip, a refused action's message). Every
// answer replaces #drawer-view whole, so a tab's forms and confirms all
// target it.
package tile

import (
	"slices"
	"strings"

	"github.com/a-h/templ"

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
		Tabs:     Tabs(v.Kind),
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
	Location string // "stack / env"
	EnvColor string

	NewDigest  string // a newer image than the one running; "" = none
	AutoUpdate bool   // the watcher deploys it by itself
}

// Tabs are the tabs a kind has, in strip order (v0's names and order).
// The keys stay the rewrite's: the canvas opens ?tab=status, and a key a
// kind lacks falls back to its first tab.
func Tabs(kind string) []string {
	if kind == "cron" || kind == "function" {
		return []string{"runs", "logs", "jobs", "env", "settings", "backups"}
	}
	return []string{"status", "jobs", "logs", "env", "settings", "backups"}
}

// Tab is the tab a request asks for, as the kind has it: the old domains
// and image tabs live in Settings now.
func Tab(kind, tab string) string {
	if tab == "domains" || tab == "image" {
		tab = "settings"
	}
	if !slices.Contains(Tabs(kind), tab) {
		return Tabs(kind)[0]
	}
	return tab
}

var labels = map[string]string{
	"status": "Overview",
	"jobs":   "Deployments",
	"env":    "Variables",
}

type StatusView struct {
	Word     string
	Replicas []ReplicaView
	Job      *c.JobStatusView // the newest job; nil = none yet
	JobWhen  string
	URLs     []DomainRow
	Port     string   // the container port; "" = none
	Ports    []string // published host:container pairs
}

type ReplicaView struct{ ID, Name, State, Health string }

type LogsView struct {
	Replicas  []ReplicaView
	Container string // the replica followed; "" = the first
	Run       string // a run's log instead of a replica's
	Runs      bool   // a cron or function: its logs are its runs'
	Pane      c.LogPaneView
}

type DomainsView struct {
	Rows  []DomainRow
	Admin bool // raw Caddy snippets are an admin's
}

type DomainRow struct {
	ID, Host, Path, Port string
	HTTPS, Auto          bool
	Redirect             string
	Raw                  string
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

type JobsView struct{ Rows []JobRow }

type JobRow struct{ Kind, State, When, Error string }

type ImageView struct {
	Ref, Digest, LastDigest, LastTag, Checked, LastError string
}

// SettingsView is the one settings form (v0's sections) plus what saves on
// its own below it: domains, the image watch, the danger zone.
type SettingsView struct {
	Vals    map[string]string // form key → value; a key the kind does not carry is absent
	Errors  map[string]string // form key → why the save was refused
	Domains *DomainsView      // nil = the kind has no endpoint
	Image   *ImageView        // nil = nothing pulled to watch
}

// Has: the kind carries this key, so the form draws its field.
func (s SettingsView) Has(key string) bool {
	_, ok := s.Vals[key]
	return ok
}

// Any: the kind carries one of keys, so their section draws.
func (s SettingsView) Any(keys ...string) bool {
	return slices.ContainsFunc(keys, s.Has)
}

// Keys are the drawn fields, posted back so a save writes only these (an
// unticked checkbox posts nothing, and still clears).
func (s SettingsView) Keys() string {
	ks := make([]string, 0, len(s.Vals))
	for k := range s.Vals {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return strings.Join(ks, " ")
}

type RunsView struct {
	Next string // cron: the next tick; "" = paused or none
	Rows []RunRow
	Poll string // re-fetched every 2 s while a run is live; "" = idle
}

type RunRow struct {
	ID, Status, Trigger, Started, Took, Exit string
	Live                                     bool // offer stop
}

type BackupsView struct{ Rows []VolumeRow }

type VolumeRow struct{ ID, Name, State, Drawer string }
