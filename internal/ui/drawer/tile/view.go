// Package tile is the tile drawer: one templ per tab, each drawn from its
// view struct inside Drawer (header, tab strip, a refused action's
// message). Every answer replaces #drawer-view whole, so a tab's forms and
// confirms all target it.
package tile

import (
	"github.com/a-h/templ"

	c "github.com/FyrmForge/stackr/internal/ui/components"
)

// Root is the id every drawer answer replaces (outerHTML).
const Root = c.DrawerRoot

// Drawer is every tile drawer answer: the shared drawer frame (header, tab
// strip, refusal or note) around one tab.
func Drawer(v View, body templ.Component) templ.Component {
	return c.Drawer(c.DrawerView{Node: v.Node, Title: v.Name, Kind: v.Kind, Base: v.Base, Tabs: Tabs(v.Kind), Tab: v.Tab,
		Error: v.Error, Note: v.Note}, body)
}

// View is the drawer around one tab.
type View struct {
	Node  string // the card's node id, for ?drawer=
	Name  string
	Kind  string // service | image | cron | function
	Base  string // the drawer URL: tabs add ?tab=, actions add /<verb>
	Tab   string
	Error string // a refused action's message
	Note  string // what an action did ("redeploy queued")
}

// Tabs are the tabs a kind has, in strip order.
func Tabs(kind string) []string {
	switch kind {
	case "cron", "function":
		return []string{"status", "runs", "logs", "env", "settings", "jobs", "image", "backups"}
	case "service":
		return []string{"status", "logs", "domains", "env", "settings", "jobs", "backups"}
	}
	return []string{"status", "logs", "domains", "env", "settings", "jobs", "image", "backups"}
}

type StatusView struct {
	Word     string
	Replicas []ReplicaView
	Job      *c.JobStatusView // the newest job; nil = none yet
	LastRun  string           // cron/function
	NextRun  string           // cron: "" = paused or none
	Stopped  bool             // offer start, not stop
}

type ReplicaView struct{ ID, Name, State, Health string }

type LogsView struct {
	Replicas  []ReplicaView
	Container string // the replica followed; "" = the first
	Run       string // a run's log instead of a replica's
	Pane      c.LogPaneView
}

type DomainsView struct {
	Rows  []DomainRow
	Admin bool // raw Caddy snippets are an admin's
}

type DomainRow struct {
	ID, Host, Path, Port string
	HTTPS, Auto          bool
	Raw                  string
}

type EnvView struct {
	JSON string // the tile's env block, a JSON object
}

type JobsView struct{ Rows []JobRow }

type JobRow struct{ Kind, State, When, Error string }

type ImageView struct {
	Ref, Digest, LastDigest, LastTag, Checked, LastError string
	NewVersion                                           bool
	UpdatePolicy, TagPolicy                              string
}

type RunsView struct {
	Cron   bool
	Paused bool
	Rows   []RunRow
}

type RunRow struct {
	ID, Status, Trigger, Started, Took, Exit string
	Live                                     bool // offer stop
}

type BackupsView struct{ Rows []VolumeRow }

type VolumeRow struct{ ID, Name, State, Drawer string }
