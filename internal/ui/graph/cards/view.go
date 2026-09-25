// Package cards draws the env canvas's cards, their sub-tiles and the
// traffic lanes (ui-plan §2). The graph service decides every field; the
// templ only draws. The <graph-node> wrapper around a card (position,
// hidden inputs, node-moved POST) is internal/ui/graph's; a card body and
// its sub-tiles go inside it as siblings, so a sub-tile's click never
// reaches the card's own drawer GET.
package cards

import "fmt"

// CardView is one card on the env canvas. ID is its node id: the tile id
// (service, cron, function, managed, ref), the provision id (slice), the
// volume id, or one of proxy, internet, vars, secrets. Tile and provision
// ids are what the Traffic verb names its ends with, so a lane needs no
// mapping.
type CardView struct {
	ID     string
	Kind   string // service | cron | function | managed | slice | ref | volume | proxy | internet | vars | secrets
	Name   string
	Detail string // image or repo@branch; cron schedule; "manual" / "on deploy"; engine; size
	Drawer string // the drawer GET with its ?tab=; "" = no drawer
	Tab    string // the tab Drawer opens on, pushed as ?tab=

	Host       bool   // privileged or devices: the red "host" chip
	NewVersion bool   // image watch saw a newer digest or tag
	Volumes    string // attached volumes as the chip reads: "uploads +2"; "" = none

	Footer FooterView
	Subs   []SubView // attached volumes, the hosting instance, replicas 2+
}

// FooterView is a card's live strip. The env events stream re-sends it as
// event "footer:<card id>" whenever one of its facts moves.
type FooterView struct {
	Status   string // tile word (running, degraded, waiting, ...); "" = none
	Waiting  string // the param a "waiting" tile misses
	Up, Want int    // replica roll-up, drawn when Want > 1
	LastRun  string // cron/function: "ok · Jul 24 13:16", "never run", "running · since 13:02"
	NextRun  string // cron: "Jul 24 14:00"; "" = paused or none
	Domain   string // exposure: the first domain
	More     int    // exposure: how many other domains
	Count    int    // vars/secrets: how many names; never a value
	Note     string // anything else the service says ("managed tile, open home")
}

// SubView is a sub-tile: a small static node under its card with its own
// drawer.
type SubView struct {
	ID     string
	Kind   string // volume | instance | replica
	Label  string
	Drawer string
	Tab    string
}

// Lane is one traffic direction at the last sample (the Traffic verb's
// Edge): From and To are node ids.
type Lane struct {
	From, To string
	BPS      float64
}

// Rate spells bytes per second the way a lane label reads it.
func Rate(bps float64) string {
	switch {
	case bps >= 1<<20:
		return fmt.Sprintf("%.1f MB/s", bps/(1<<20))
	case bps >= 1<<10:
		return fmt.Sprintf("%.1f KB/s", bps/(1<<10))
	}
	return fmt.Sprintf("%.0f B/s", bps)
}

// System says a kind lives in the column behind the divider: the node
// wrapper sets <graph-node system> from it.
func System(kind string) bool {
	return kind == "proxy" || kind == "internet"
}

// dashed cards are not the env's own: system cards and ghost refs.
func dashed(kind string) bool {
	return System(kind) || kind == "ref"
}
