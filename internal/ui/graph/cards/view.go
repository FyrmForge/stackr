// Package cards draws the canvas cards in v0's look (canvas.templ
// nodeCardFace): the env canvas's cards, their sub-tiles and the traffic
// lanes, and the face pieces (Class, Icon, Top, Status) internal/ui/graph
// draws the drill-down cards with. The graph service decides every field;
// the templ only draws. The <graph-node> wrapper around a card (position,
// hidden inputs, node-moved POST) is internal/ui/graph's; a card face and
// its sub-tiles go inside it as siblings, so a sub-tile's click never
// reaches the card's own drawer GET.
package cards

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// CardView is one card on the env canvas. ID is its node id: the tile id
// (service, cron, function, managed, ref), the provision id (slice), the
// volume id, or one of proxy, internet, vars, secrets. Tile and provision
// ids are what the Traffic verb names its ends with, so a lane needs no
// mapping.
type CardView struct {
	ID     string
	Kind   string // service | image | cron | function | managed | slice | ref | volume | proxy | internet | vars | secrets
	Name   string
	Detail string // image or repo; cron schedule; "manual" / "on deploy"; engine; db name
	Drawer string // the drawer GET with its ?tab=; "" = no drawer
	Tab    string // the tab Drawer opens on, pushed as ?tab=

	Host       bool // privileged or devices: the red "host" chip
	NewVersion bool // image watch saw a newer digest or tag

	Footer FooterView
	Subs   []SubView // attached volumes, the hosting instance, replicas 2 to 4
}

// FooterView is a card's live strip (v0 NodeFooter). The env events
// stream re-sends it as event "footer:<card id>" whenever one of its facts
// moves.
type FooterView struct {
	Kind     string // the card's kind: which strip it is
	Status   string // tile word (running, degraded, waiting, ...); "" = idle
	Waiting  string // the param a "waiting" tile misses
	Up, Want int    // replica roll-up, drawn when Want > 1
	LastRun  string // cron/function: "ok · Jul 24 13:16", "never run", "running · since 13:02"
	NextRun  string // cron: "next 14:00"; "" = paused
	Trigger  string // function: "manual" / "on deploy", where a cron's next run goes
	Domain   string // exposure: the first domain
	More     int    // exposure: how many other domains
	Domains  int    // org canvas: "N domains" in place of the hosts (v0 DomainCount)
	Nav      bool   // the card is itself a link: the host is text, never a nested <a>
}

// SubView is a sub-tile: the strip of a card dealt under its card, with
// its own drawer.
type SubView struct {
	ID     string
	Kind   string // volume | instance | replica
	Label  string
	Detail string // mount path, engine
	Status string // replica state: the dot
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

// Width is the lane's stroke: thicker with the rate, 4 px at most (v0).
func (l Lane) Width() string {
	w := min(4, 1.5+math.Log10(1+l.BPS/1024))
	return strconv.FormatFloat(w, 'f', 2, 64)
}

// System says a kind lives in the column behind the divider: the node
// wrapper sets <graph-node system> from it.
func System(kind string) bool {
	return kind == "proxy" || kind == "internet"
}

// Class is a card face's look by kind (v0 nodeClass). System cards and
// ghost refs are dashed placeholders; volumes and slices sit inset; a
// stack or env card is a deck ([data-deck] draws its layers); the rest
// are raised. card-* classes carry the CSS the canvas block keys on.
func Class(kind string) string {
	const base = "card-face relative block h-full rounded-xl border"
	switch {
	case System(kind):
		return base + " card-system border-dashed border-rw-strong bg-rw-bg/80"
	case kind == "ref":
		return base + " card-ref border-dashed border-rw-accent/50 bg-rw-surface/60 hover:border-rw-accent/80"
	case kind == "volume":
		return base + " bg-rw-inset border-rw-border hover:border-rw-accent/60"
	case kind == "slice":
		return base + " bg-rw-inset border-rw-border hover:border-rw-accent/60 shadow-rw"
	case kind == "stack" || kind == "env":
		return base + " card-deck bg-rw-surface border-rw-border hover:border-rw-accent/60"
	}
	return base + " bg-rw-surface border-rw-border hover:border-rw-accent/60 shadow-rw"
}

// KindLabel is the chip under a card's name (v0 kindLabel); "" where the
// canvas already says what the card is.
func KindLabel(kind string) string {
	switch kind {
	case "service", "image":
		return "service"
	case "cron", "function", "slice", "proxy", "connector":
		return kind
	case "managed":
		return "database"
	}
	return ""
}

// run is the colour a last-run line reads in, by its first word.
func run(line string) string {
	switch {
	case strings.HasPrefix(line, "running"):
		return "warn"
	case strings.HasPrefix(line, "ok"):
		return "success"
	case strings.HasPrefix(line, "failed"):
		return "danger"
	}
	return "faint"
}

// word is the footer's status line (v0 nodeStatus): its tone and words.
func word(status, waiting string) (tone, text string) {
	switch status {
	case "running", "done":
		return "success", "Online"
	case "error":
		return "danger", "Crashed"
	case "unhealthy":
		return "warn", "Unhealthy"
	case "waiting":
		if waiting == "" {
			return "warn", "Waiting for a value"
		}
		return "warn", "Waiting for " + waiting
	case "building", "queued":
		return "warn", status
	case "":
		return "faint", "idle"
	}
	return "faint", status
}

// replica is the dot of a replica sub-tile by its container state.
func replica(state string) string {
	switch state {
	case "running":
		return "success"
	case "exited", "dead":
		return "danger"
	}
	return "warn"
}

// tone* spell a tone as the classes templ needs whole (tailwind scans
// the source for full class names).
func toneText(t string) string {
	switch t {
	case "success":
		return "text-rw-success"
	case "danger":
		return "text-rw-danger"
	case "warn":
		return "text-rw-warn"
	}
	return "text-rw-faint"
}

func toneDot(t string) string {
	switch t {
	case "success":
		return "bg-rw-success"
	case "danger":
		return "bg-rw-danger"
	case "warn":
		return "bg-rw-warn"
	}
	return "bg-rw-faint"
}

// domains is v0's domainCount.
func domains(n int) string {
	if n == 1 {
		return "1 domain"
	}
	return itoa(n) + " domains"
}

func itoa(n int) string {
	return strconv.Itoa(n)
}
