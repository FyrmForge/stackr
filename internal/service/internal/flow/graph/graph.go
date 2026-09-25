// Package graph builds one canvas: which cards and edges exist at a level
// of the tree, their worst-of status, ghost refs, and where unsaved cards
// go (arrange.go). It reads many leaves and writes only through
// leaf/canvas. The web layer maps a View to templ; it decides nothing.
package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/canvas"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/connector"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/image"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/managed"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/org"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
	lrun "github.com/FyrmForge/stackr/internal/service/internal/leaf/run"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/stack"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	ltraffic "github.com/FyrmForge/stackr/internal/service/internal/leaf/traffic"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type Flow struct {
	Orgs     *org.Leaf
	Stacks   *stack.Leaf
	Envs     *environment.Leaf
	Tiles    *tile.Leaf
	Params   *params.Leaf
	Volumes  *volume.Leaf
	Domains  *domain.Leaf
	Managed  *managed.Leaf
	Conns    *connector.Leaf
	Releases *release.Leaf
	Jobs     *job.Leaf
	Runs     *lrun.Leaf
	Canvas   *canvas.Leaf
	Images   *image.Leaf
}

// Node kinds besides the tile kinds (service, image, managed, cron,
// function), which a tile card carries as is.
const (
	KindOrg       = "org"
	KindStack     = "stack"
	KindEnv       = "env"
	KindConnector = "connector"
	KindVars      = "vars"
	KindSlice     = "slice"
	KindRef       = "ref" // ghost: lives up the tree or in another env
	KindVolume    = "volume"
	KindReplica   = "replica" // sub-tile only
	KindProxy     = "proxy"
	KindInternet  = "internet"
)

// Edge kinds (data-edge-kind).
const (
	EdgeRef     = "ref"
	EdgeIngress = "ingress"
	EdgeEgress  = "egress"
	EdgeShared  = "shared"
	EdgeStartup = "startup"
	EdgeConfig  = "config"
	EdgeSource  = "source"
)

// Card sizes and gaps in world pixels, v0's geometry (graph-ref §6).
const (
	CardW  = 220
	CardH  = 96
	ShortH = 62 // a detached volume
	SubH   = 30 // the room each sub-tile strip takes when placing (never drawn height)
	Grid   = 22
	GapX   = 40 // clear space a placement keeps around a card
	GapY   = 30

	colSystemX = -280
	colGap     = 360
	rowStart   = 80
	rowGap     = 140
	localGap   = CardW + 80 // column pitch inside a cluster
	wallGap    = 60         // the divider stands this far right of the system column
)

type View struct {
	Scope   canvas.Scope
	Nodes   []Node
	Edges   []Edge
	Notes   []store.Annotation
	Walled  bool   // system and workload cards both drawn: a divider between them
	Divider int    // world x of that divider (0 is a real place)
	Compare []Rung // stack and env canvas: the env-compare pill, ladder order
}

type Node struct {
	ID     string // org:<id> stack:<id> env:<id> connector:<id> vars; env: tile id, provision id (slice), instance tile id or ref:<kind>.<slug> (ghost), volume id, proxy, internet
	Kind   string
	Name   string
	Slug   string // drill-down and drawer address
	Detail string
	Status string // worst-of word; "" before the status pass
	X, Y   int
	W, H   int
	Saved  bool // the position is a row, not arranged
	System bool // behind the divider
	Static bool // not draggable (ghost refs)
	Color  string
	Deck   int // 0-2 layers behind a drill-down card
	Subs   []Sub

	// Tile facts (session C's footers and chips).
	Replicas   int
	Running    int
	Domains    []string
	Volumes    []string
	Host       bool // privileged or devices
	LastRun    *store.Run
	NextRun    *time.Time // unpaused cron only
	NewVersion bool       // image watch saw a newer digest
	Waiting    string     // the param a parked job waits for
	// Vars card counts; never a value.
	Params, Secrets int
}

// Sub is a sub-tile; Slug is the hosting instance's tile slug, Detail the
// strip's right-hand word (a mount path, an engine).
type Sub struct{ ID, Kind, Name, Status, Slug, Detail string }

type Edge struct{ Kind, From, To string }

// Rung is one env in the compare pill: the release it runs, and whether
// the rung below it runs a newer one.
type Rung struct {
	EnvID, Name, Slug, Color string
	Release                  int // 0 = none yet
	Behind                   bool
}

// Show is what the server draws; the look toggles are the element's.
type Show struct{ System, Refs, Startup, Traffic bool }

// All draws everything.
var All = Show{
	System:  true,
	Refs:    true,
	Startup: true,
	Traffic: true,
}

// In is one build: status=false skips every Docker read (positions and the
// SSE node-set check need structure only). Traffic is the env's last
// sample, fetched by the orchestrator (flow/traffic is not ours to call).
type In struct {
	Show    Show
	Status  bool
	Traffic []ltraffic.Edge
}

// Build is the canvas at s.
func (f *Flow) Build(ctx context.Context, s canvas.Scope, in In) (View, error) {
	v := View{Scope: s}
	var err error
	switch s.Kind {
	case canvas.Home:
		err = f.home(ctx, &v, s.ID, in)
	case canvas.Org:
		err = f.org(ctx, &v, s.ID, in)
	case canvas.Stack:
		err = f.stack(ctx, &v, s.ID, in)
	case canvas.Env:
		err = f.env(ctx, &v, s.ID, in)
	default:
		err = fmt.Errorf("graph: unknown canvas %q", s.Kind)
	}
	if err != nil {
		return v, err
	}
	// Arranged before the toggles filter, so turning one off never moves
	// a card (v0), and Place saves the points a filtered canvas drew.
	saved, err := f.Canvas.Positions(ctx, s)
	if err != nil {
		return v, err
	}
	arrange(&v, saved)
	filter(&v, in.Show)
	wall(&v)
	v.Notes, err = f.Canvas.Annotations(ctx, s)
	return v, err
}

// wall stands the divider right of the system column when both sides
// have cards; it follows the system cards, never the workloads (v0).
func wall(v *View) {
	system, work := false, false
	right := 0
	for _, n := range v.Nodes {
		switch {
		case !n.System:
			work = true
		case !system || n.X+n.W > right:
			right = n.X + n.W
			system = true
		}
	}
	v.Walled = system && work
	if v.Walled {
		v.Divider = right + wallGap
	}
}

func card(id, kind, name string) Node {
	return Node{
		ID:   id,
		Kind: kind,
		Name: name,
		W:    CardW,
		H:    CardH,
	}
}

// deck is the layers drawn behind a drill-down card: one per child, two at
// most (v0's deckLayers).
func deck(children int) int { return max(0, min(children, 2)) }

func plural(n int, one string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %ss", n, one)
}

// ---- home, org, stack ------------------------------------------------------

func (f *Flow) home(ctx context.Context, v *View, userID string, in In) error {
	orgs, err := f.Orgs.ListForUser(ctx, userID)
	if err != nil {
		return err
	}
	for _, og := range orgs {
		sts, err := f.Stacks.List(ctx, og.ID)
		if err != nil {
			return err
		}
		ms, err := f.Orgs.Members(ctx, og.ID)
		if err != nil {
			return err
		}
		n := card("org:"+og.ID, KindOrg, og.Name)
		n.Slug, n.Deck = og.Slug, deck(len(sts))
		n.Detail = plural(len(sts), "stack") + " · " + plural(len(ms), "member") // v0's org card line
		if og.SetupDoneAt == nil {
			n.Detail = "Setup unfinished" // it answers nothing but its wizard (v0 orgDetail)
		}
		if in.Status {
			for _, st := range sts {
				ts, err := f.Tiles.ListByStack(ctx, st.ID)
				if err != nil {
					return err
				}
				if n.Status, err = f.worstOf(ctx, n.Status, ts); err != nil {
					return err
				}
			}
		}
		v.Nodes = append(v.Nodes, n)
	}
	return nil
}

func (f *Flow) org(ctx context.Context, v *View, orgID string, in In) error {
	sts, err := f.Stacks.List(ctx, orgID)
	if err != nil {
		return err
	}
	cs, err := f.Conns.List(ctx, orgID)
	if err != nil {
		return err
	}
	for _, c := range cs {
		n := card("connector:"+c.ID, KindConnector, c.Name)
		n.Slug, n.Detail = c.ID, c.Host
		v.Nodes = append(v.Nodes, n)
	}
	vars, err := f.vars(ctx, params.Scope{Kind: "org", ID: orgID})
	if err != nil {
		return err
	}
	if vars.shown() {
		v.Nodes = append(v.Nodes, vars)
	}
	proxied := false
	for _, st := range sts {
		es, err := f.Envs.List(ctx, st.ID)
		if err != nil {
			return err
		}
		ts, err := f.Tiles.ListByStack(ctx, st.ID)
		if err != nil {
			return err
		}
		n := card("stack:"+st.ID, KindStack, st.Name)
		n.Slug, n.Detail, n.Deck = st.Slug, plural(len(es), "environment"), deck(len(es))
		if in.Status {
			if n.Status, err = f.worstOf(ctx, "", ts); err != nil {
				return err
			}
		}
		if n.Domains, err = f.hosts(ctx, ts); err != nil {
			return err
		}
		if len(n.Domains) > 0 {
			v.Edges = append(v.Edges, Edge{EdgeIngress, KindProxy, n.ID})
			proxied = true
		}
		v.Nodes = append(v.Nodes, n)
		if st.ConfigConnectorID != "" && st.ConfigRepo != "" {
			v.Edges = append(v.Edges, Edge{EdgeConfig, "connector:" + st.ConfigConnectorID, n.ID})
		}
		for _, c := range cs {
			if buildsFrom(ts, c.Host) {
				v.Edges = append(v.Edges, Edge{EdgeSource, "connector:" + c.ID, n.ID})
			}
		}
		if vars.shown() && reads(ts, params.KindOrgParam) {
			v.Edges = append(v.Edges, Edge{EdgeShared, vars.ID, n.ID})
		}
	}
	if proxied {
		v.Nodes = append(v.Nodes, proxyCard())
	}
	return nil
}

func (f *Flow) stack(ctx context.Context, v *View, stackID string, in In) error {
	es, rs, err := f.ladder(ctx, stackID)
	if err != nil {
		return err
	}
	v.Compare = rs
	vars, err := f.vars(ctx, params.Scope{Kind: "stack", ID: stackID})
	if err != nil {
		return err
	}
	if vars.shown() {
		v.Nodes = append(v.Nodes, vars)
	}
	proxied := false
	for i, e := range es {
		ts, err := f.Tiles.List(ctx, e.ID)
		if err != nil {
			return err
		}
		n := card("env:"+e.ID, KindEnv, e.Name)
		n.Slug = e.Slug
		n.Color = rs[i].Color
		n.Detail = plural(len(ts), "tile") // v0's env card line
		n.Deck = deck(len(ts))
		if in.Status {
			if n.Status, err = f.worstOf(ctx, "", ts); err != nil {
				return err
			}
		}
		if n.Domains, err = f.hosts(ctx, ts); err != nil {
			return err
		}
		if len(n.Domains) > 0 {
			v.Edges = append(v.Edges, Edge{EdgeIngress, KindProxy, n.ID})
			proxied = true
		}
		v.Nodes = append(v.Nodes, n)
		if vars.shown() && reads(ts, params.KindParam) {
			v.Edges = append(v.Edges, Edge{EdgeShared, vars.ID, n.ID})
		}
	}
	if proxied {
		v.Nodes = append(v.Nodes, proxyCard())
	}
	return nil
}

// ladder is a stack's envs, the ladder first and the envs off it (PR envs,
// ...) after, and the env-compare pill's rung for each, in the same order.
// The stack and the env canvas both draw the pill (v0 envCompareWidget).
func (f *Flow) ladder(ctx context.Context, stackID string) ([]store.Environment, []Rung, error) {
	es, err := f.Envs.Ladder(ctx, stackID)
	if err != nil {
		return nil, nil, err
	}
	all, err := f.Envs.List(ctx, stackID)
	if err != nil {
		return nil, nil, err
	}
	es = append(es, rest(all, es)...)
	hues := environment.Hues(all)
	rs := make([]Rung, 0, len(es))
	prev := 0
	for i, e := range es {
		r := Rung{
			EnvID: e.ID,
			Name:  e.Name,
			Slug:  e.Slug,
			Color: hues[e.ID],
		}
		if e.ReleaseID != nil {
			rel, err := f.Releases.Get(ctx, *e.ReleaseID)
			if err != nil {
				return nil, nil, err
			}
			r.Release = rel.Number
		}
		r.Behind = i > 0 && prev > r.Release
		prev = r.Release
		rs = append(rs, r)
	}
	return es, rs, nil
}

// rest is the envs off the ladder (PR envs, ...), after it.
func rest(all, ladder []store.Environment) []store.Environment {
	on := map[string]bool{}
	for _, e := range ladder {
		on[e.ID] = true
	}
	var out []store.Environment
	for _, e := range all {
		if !on[e.ID] {
			out = append(out, e)
		}
	}
	return out
}

// shown: v0 drew no vars card for a scope with nothing in it; the drawer's
// params tab is where the first one gets added.
func (n Node) shown() bool {
	return n.Params+n.Secrets > 0
}

func (f *Flow) vars(ctx context.Context, s params.Scope) (Node, error) {
	ps, err := f.Params.List(ctx, s, true)
	n := card(KindVars, KindVars, "Variables")
	n.Static = true // v0's vars card is pinned where the layout puts it
	for _, p := range ps {
		if p.Kind == params.Secret {
			n.Secrets++
		} else {
			n.Params++
		}
	}
	n.Detail = plural(n.Params, "param") + " · " + plural(n.Secrets, "secret")
	return n, err
}

func buildsFrom(ts []store.Tile, host string) bool {
	return slices.ContainsFunc(ts, func(t store.Tile) bool {
		return t.GitURL != "" && connector.Host(t.GitURL) == host
	})
}

// reads: any tile of ts uses a ref of kind k.
func reads(ts []store.Tile, k params.Kind) bool {
	for _, t := range ts {
		for _, r := range refs(t) {
			if r.Kind == k {
				return true
			}
		}
	}
	return false
}

// refs are every well-formed ${{ }} in a tile's env values and command.
func refs(t store.Tile) []params.Ref {
	var env map[string]string
	_ = json.Unmarshal([]byte(t.EnvJSON), &env)
	src := []string{t.Command}
	for _, k := range slices.Sorted(maps.Keys(env)) {
		src = append(src, env[k])
	}
	var out []params.Ref
	for _, s := range src {
		for _, b := range params.Refs(s) {
			if r, err := params.Parse(b); err == nil {
				out = append(out, r)
			}
		}
	}
	return out
}

// ---- status ---------------------------------------------------------------

// rank orders the words for the worst-of roll-up (graph-ref §1): error >
// building/queued > unhealthy > the rest.
var rank = map[string]int{
	"error":     7,
	"building":  6,
	"queued":    6,
	"waiting":   6,
	"unhealthy": 5,
	"degraded":  4,
	"stopped":   3,
	"running":   2,
	"done":      1,
	"none":      0,
	"":          -1,
}

func worse(a, b string) string {
	if rank[b] > rank[a] {
		return b
	}
	return a
}

func (f *Flow) worstOf(ctx context.Context, w string, ts []store.Tile) (string, error) {
	for _, t := range ts {
		st, err := f.status(ctx, t)
		if err != nil {
			return w, err
		}
		w = worse(w, st.word)
	}
	return w, nil
}

type status struct {
	word     string
	state    tile.State
	lastRun  *store.Run
	waiting  string
	replicas []Sub
}

// status is one tile's word: a live job wins, then a failed one over a
// box that is not running, then the containers. A cron or function has no
// standing container: its last run speaks.
func (f *Flow) status(ctx context.Context, t store.Tile) (status, error) {
	var s status
	j, hasJob, err := f.Jobs.Last(ctx, t.ID)
	if err != nil {
		return s, err
	}
	if tile.RunToCompletion(t.Kind) {
		s.word = "none"
		if r, ok, err := f.Runs.Last(ctx, t.ID); err != nil {
			return s, err
		} else if ok {
			s.lastRun = &r
			s.word = map[string]string{
				"queued":  "queued",
				"running": "running",
				"ok":      "done",
				"failed":  "error",
			}[r.Status]
		}
	} else {
		if s.state, err = f.Tiles.State(ctx, t); err != nil {
			return s, err
		}
		s.word = s.state.Word
		// v0 deals replicas 2 to 4 under the card (the card is replica 1).
		for i, c := range s.state.Replicas {
			if i == 0 {
				continue
			}
			if i >= 4 {
				break
			}
			s.replicas = append(s.replicas, Sub{
				ID:     fmt.Sprintf("replica:%s:%d", t.ID, i+1),
				Kind:   KindReplica,
				Name:   fmt.Sprintf("replica %d", i+1),
				Status: c.State,
			})
		}
	}
	switch {
	case hasJob && j.State == job.Queued:
		s.word = "queued"
	case hasJob && j.State == job.Running:
		s.word = "building"
	case hasJob && j.State == job.Waiting:
		s.word = "waiting"
		if j.WaitingParam != nil {
			s.waiting = *j.WaitingParam
		}
	case hasJob && j.State == job.Failed && s.word != "running":
		s.word = "error"
	}
	return s, nil
}

// ---- env -------------------------------------------------------------------

func (f *Flow) env(ctx context.Context, v *View, envID string, in In) error {
	e, err := f.Envs.Get(ctx, envID)
	if err != nil {
		return err
	}
	if _, v.Compare, err = f.ladder(ctx, e.StackID); err != nil {
		return err
	}
	ts, err := f.Tiles.List(ctx, envID)
	if err != nil {
		return err
	}
	bySlug, byID := map[string]store.Tile{}, map[string]store.Tile{}
	for _, t := range ts {
		bySlug[t.Slug], byID[t.ID] = t, t
	}
	vars, err := f.vars(ctx, params.Scope{Kind: "env", ID: envID})
	if err != nil {
		return err
	}
	if vars.shown() {
		v.Nodes = append(v.Nodes, vars)
	}
	vs, err := f.Volumes.List(ctx, volume.Scope{Kind: "env", ID: envID})
	if err != nil {
		return err
	}
	vols := map[string]string{}
	for _, vol := range vs {
		vols[vol.Slug] = vol.ID
	}

	// Slices first: an instance of this env that hosts one rides under it
	// and loses its own card.
	hosted := map[string]bool{}
	var slices []Node
	for _, t := range ts {
		ps, err := f.Managed.ForConsumer(ctx, t.ID)
		if err != nil {
			return err
		}
		for _, p := range ps {
			n := card(p.ID, KindSlice, p.DBName)
			n.Slug, n.Detail = p.ID, p.DBName
			inst, err := f.Managed.Get(ctx, p.InstanceID)
			if err != nil {
				return err
			}
			host, err := f.Tiles.Get(ctx, inst.TileID)
			if err != nil {
				return err
			}
			if host.EnvironmentID == envID {
				hosted[host.ID] = true
				n.Subs = append(n.Subs, Sub{
					ID:     host.ID,
					Kind:   tile.Managed,
					Name:   host.Name,
					Slug:   host.Slug,
					Detail: inst.Engine,
				})
			} else {
				g := ghost(v, host.ID, host.Name, inst.Engine)
				v.Edges = append(v.Edges, Edge{EdgeShared, n.ID, g})
			}
			v.Edges = append(v.Edges, Edge{EdgeRef, t.ID, n.ID})
			slices = append(slices, n)
		}
	}

	mounted := map[string]bool{}
	proxied := false
	for _, t := range ts {
		if hosted[t.ID] {
			continue
		}
		n, err := f.tileCard(ctx, t, vols, in.Status)
		if err != nil {
			return err
		}
		for _, sl := range n.Volumes {
			mounted[sl] = true
		}
		if len(n.Domains) > 0 {
			proxied = true
			v.Edges = append(v.Edges, Edge{EdgeIngress, KindProxy, n.ID})
		}
		v.Nodes = append(v.Nodes, n)
	}
	v.Nodes = append(v.Nodes, slices...)

	// Refs, then startup edges where no ref already joins the pair.
	joined := map[[2]string]bool{}
	for _, t := range ts {
		id := t.ID
		readsVars := false
		for _, r := range refs(t) {
			switch r.Kind {
			case params.KindTile:
				if to, ok := bySlug[r.Slug]; ok && !hosted[to.ID] && to.ID != t.ID && !joined[[2]string{id, to.ID}] {
					v.Edges = append(v.Edges, Edge{EdgeRef, id, to.ID})
					joined[[2]string{id, to.ID}] = true
				}
			case params.KindStack, params.KindOrg:
				g := ghost(v, "ref:"+string(r.Kind)+"."+r.Slug, r.Slug, string(r.Kind)+" tile")
				if !joined[[2]string{id, g}] {
					v.Edges = append(v.Edges, Edge{EdgeShared, id, g})
					joined[[2]string{id, g}] = true
				}
			case params.KindParam:
				readsVars = true
			}
		}
		if readsVars && vars.shown() {
			v.Edges = append(v.Edges, Edge{EdgeShared, vars.ID, id})
		}
	}
	for _, t := range ts {
		for _, l := range tile.Lines(t.DependsOn) {
			dep, _, err := tile.ParseDep(l)
			to, ok := bySlug[dep]
			if err != nil || !ok || hosted[to.ID] {
				continue
			}
			a, b := t.ID, to.ID
			if !joined[[2]string{a, b}] && !joined[[2]string{b, a}] {
				v.Edges = append(v.Edges, Edge{EdgeStartup, a, b})
			}
		}
	}

	// Detached volumes: declared here, mounted by nothing.
	for _, vol := range vs {
		if vol.InstanceID != nil || mounted[vol.Slug] {
			continue
		}
		n := card(vol.ID, KindVolume, vol.Slug)
		n.Slug, n.H, n.Detail = vol.Slug, ShortH, "detached"
		if vol.OrphanedAt != nil {
			n.Detail = "orphaned"
		}
		v.Nodes = append(v.Nodes, n)
	}

	// The system column: the proxy when anything is proxied, the internet
	// when a tile talked to it in the last sample.
	egress := map[string]bool{}
	for _, l := range in.Traffic {
		if t, ok := byID[l.From]; ok && l.To == ltraffic.Internet && !hosted[t.ID] && !egress[t.Slug] {
			egress[t.Slug] = true
			v.Edges = append(v.Edges, Edge{EdgeEgress, t.ID, KindInternet})
		}
	}
	if proxied {
		v.Nodes = append(v.Nodes, proxyCard())
	}
	if len(egress) > 0 {
		n := card(KindInternet, KindInternet, "Internet")
		n.Detail, n.System = "outbound", true
		v.Nodes = append(v.Nodes, n)
	}
	return nil
}

// proxyCard is the system card in front of whatever has a domain, the
// same card on the org, stack and env canvas (v0 proxyNode).
func proxyCard() Node {
	n := card(KindProxy, KindProxy, "Proxy")
	n.Detail, n.System = "Caddy", true
	n.Status = "running" // ponytail: as v0; this page came through it, a live Caddy probe if that ever lies
	return n
}

// hosts is every domain the tiles answer on, in tile order.
func (f *Flow) hosts(ctx context.Context, ts []store.Tile) ([]string, error) {
	var out []string
	for _, t := range ts {
		ds, err := f.Domains.ListByTile(ctx, t.ID)
		if err != nil {
			return nil, err
		}
		for _, d := range ds {
			out = append(out, d.Host)
		}
	}
	return out, nil
}

// ghost adds a ref card once and returns its id.
func ghost(v *View, id, name, detail string) string {
	if slices.ContainsFunc(v.Nodes, func(n Node) bool { return n.ID == id }) {
		return id
	}
	n := card(id, KindRef, name)
	n.Detail, n.Static = detail, true
	v.Nodes = append(v.Nodes, n)
	return id
}

// tileCard is one tile's card; vols are the env's volumes by slug, so a
// mount's sub-tile carries the volume id (a mount of an undeclared slug
// keeps "volume:<slug>").
func (f *Flow) tileCard(ctx context.Context, t store.Tile, vols map[string]string, withStatus bool) (Node, error) {
	n := card(t.ID, t.Kind, t.Name)
	n.Slug, n.Replicas = t.Slug, max(t.Replicas, 1)
	n.Host = t.Privileged || strings.TrimSpace(t.Devices) != ""
	switch {
	case t.Kind == tile.Cron:
		n.Detail = t.Schedule
	case t.Kind == tile.Function && t.Trigger == "on_deploy":
		n.Detail = "on deploy"
	case t.Kind == tile.Function:
		n.Detail = "manual"
	case t.Kind == tile.Managed:
		if in, err := f.Managed.GetByTile(ctx, t.ID); err == nil {
			n.Detail = in.Engine
		}
	case t.GitURL != "":
		n.Detail = strings.TrimSuffix(t.GitURL[strings.LastIndex(t.GitURL, "/")+1:], ".git")
	default:
		n.Detail = t.ImageRef
	}
	ds, err := f.Domains.ListByTile(ctx, t.ID)
	if err != nil {
		return n, err
	}
	for _, d := range ds {
		n.Domains = append(n.Domains, d.Host)
	}
	for _, l := range tile.Lines(t.Volumes) {
		sl, path, _ := strings.Cut(l, ":")
		n.Volumes = append(n.Volumes, sl)
		id, ok := vols[sl]
		if !ok {
			id = "volume:" + sl
		}
		n.Subs = append(n.Subs, Sub{
			ID:     id,
			Kind:   KindVolume,
			Name:   sl,
			Detail: path,
		})
	}
	if t.Kind == tile.Managed {
		if in, err := f.Managed.GetByTile(ctx, t.ID); err == nil {
			vs, err := f.Volumes.List(ctx, volume.Scope{Kind: "env", ID: t.EnvironmentID})
			if err != nil {
				return n, err
			}
			for _, vol := range vs {
				if vol.InstanceID != nil && *vol.InstanceID == in.ID {
					n.Subs = append(n.Subs, Sub{ID: vol.ID, Kind: KindVolume, Name: vol.Slug})
				}
			}
		}
	}
	if withStatus {
		st, err := f.status(ctx, t)
		if err != nil {
			return n, err
		}
		n.Status, n.LastRun, n.Waiting = st.word, st.lastRun, st.waiting
		n.Running = 0
		for _, c := range st.state.Replicas {
			if c.State == "running" {
				n.Running++
			}
		}
		n.Subs = append(n.Subs, st.replicas...)
		if t.Kind == tile.Cron && !t.Paused {
			if at, err := lrun.Next(t.Schedule, time.Now()); err == nil {
				n.NextRun = &at
			}
		}
		if t.ImageRef != "" {
			i, err := f.Images.GetByRef(ctx, t.ImageRef)
			if err != nil && !errors.Is(err, errs.ErrNotFound) {
				return n, err
			}
			if tile.Pulls(t) {
				e, err := f.Envs.Get(ctx, t.EnvironmentID)
				if err != nil {
					return n, err
				}
				if i.Digest, err = f.Releases.Digest(ctx, e.ReleaseID, t.Slug); err != nil {
					return n, err
				}
			}
			n.NewVersion = i.Newer()
		}
	}
	return n, nil
}

// filter drops what the query params turned off.
func filter(v *View, s Show) {
	drop := map[string]bool{}
	if !s.System {
		for _, n := range v.Nodes {
			if n.System {
				drop[n.ID] = true
			}
		}
	}
	if !s.Traffic {
		drop[KindInternet] = true
	}
	nodes := v.Nodes[:0]
	for _, n := range v.Nodes {
		if !drop[n.ID] {
			nodes = append(nodes, n)
		}
	}
	v.Nodes = nodes
	edges := v.Edges[:0]
	for _, e := range v.Edges {
		off := drop[e.From] || drop[e.To] || (!s.Refs && e.Kind == EdgeRef) ||
			(!s.Startup && e.Kind == EdgeStartup) || (!s.Traffic && e.Kind == EdgeEgress)
		if !off {
			edges = append(edges, e)
		}
	}
	v.Edges = edges
}

// Place saves one card's drop. The first drop on a canvas with unsaved
// cards saves every card where it stands, or the arranged neighbours would
// all jump next to the one that moved (graph-ref §6). A note moves its row.
func (f *Flow) Place(ctx context.Context, s canvas.Scope, in In, nodeID string, p canvas.Point) error {
	if id, ok := strings.CutPrefix(nodeID, "note:"); ok {
		return f.Canvas.Move(ctx, s, id, p)
	}
	in.Status = false
	v, err := f.Build(ctx, s, in)
	if err != nil {
		return err
	}
	var target *Node
	for i := range v.Nodes {
		if v.Nodes[i].ID == nodeID {
			target = &v.Nodes[i]
		}
	}
	if target == nil {
		return errs.ErrNotFound
	}
	if target.Static {
		return errs.Invalidf("node_id", "this card does not move")
	}
	for _, n := range v.Nodes {
		if !n.Saved && n.ID != nodeID {
			if err := f.Canvas.Save(ctx, s, n.ID, canvas.Point{X: n.X, Y: n.Y}); err != nil {
				return err
			}
		}
	}
	return f.Canvas.Save(ctx, s, nodeID, p)
}
