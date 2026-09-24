// Package graph builds one canvas: which cards and edges exist at a level
// of the tree, their worst-of status, ghost refs, and where unsaved cards
// go (arrange.go). It reads many leaves and writes only through
// leaf/canvas. The web layer maps a View to templ; it decides nothing.
package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/canvas"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/connector"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
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

// Card sizes and gaps in world pixels (graph-ref §6).
const (
	CardW   = 220
	CardH   = 96
	ShortH  = 62 // a detached volume
	SubH    = 30 // each sub-tile adds this much
	Grid    = 22
	GapX    = 80
	GapY    = 44
	sysGap  = 40
	Divider = CardW + sysGap // world x of the system wall
)

type View struct {
	Scope   canvas.Scope
	Nodes   []Node
	Edges   []Edge
	Notes   []store.Annotation
	Divider int    // world x of the system wall; 0 = no system column
	Compare []Rung // stack canvas: the env-compare pill, ladder order
}

type Node struct {
	ID     string // org:<id> stack:<id> env:<id> connector:<id> vars tile:<slug> slice:<id> ref:<key> volume:<slug> proxy internet
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
	Replicas int
	Running  int
	Domains  []string
	Volumes  []string
	Host     bool // privileged or devices
	LastRun  *store.Run
	Waiting  string // the param a parked job waits for
	// Vars card counts; never a value.
	Params, Secrets int
}

type Sub struct{ ID, Kind, Name, Status string }

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
var All = Show{System: true, Refs: true, Startup: true, Traffic: true}

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
	filter(&v, in.Show)
	saved, err := f.Canvas.Positions(ctx, s)
	if err != nil {
		return v, err
	}
	arrange(&v, saved)
	v.Notes, err = f.Canvas.Annotations(ctx, s)
	return v, err
}

func card(id, kind, name string) Node {
	return Node{ID: id, Kind: kind, Name: name, W: CardW, H: CardH}
}

func deck(children int) int { return max(0, min(children, 3)-1) }

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
		n := card("org:"+og.ID, KindOrg, og.Name)
		n.Slug, n.Detail, n.Deck = og.Slug, plural(len(sts), "stack"), deck(len(sts))
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
	v.Nodes = append(v.Nodes, vars)
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
		n.Slug, n.Detail, n.Deck = st.Slug, plural(len(es), "env"), deck(len(es))
		if in.Status {
			if n.Status, err = f.worstOf(ctx, "", ts); err != nil {
				return err
			}
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
		if reads(ts, params.KindOrgParam) {
			v.Edges = append(v.Edges, Edge{EdgeShared, vars.ID, n.ID})
		}
	}
	return nil
}

func (f *Flow) stack(ctx context.Context, v *View, stackID string, in In) error {
	es, err := f.Envs.Ladder(ctx, stackID)
	if err != nil {
		return err
	}
	all, err := f.Envs.List(ctx, stackID)
	if err != nil {
		return err
	}
	es = append(es, rest(all, es)...)
	vars, err := f.vars(ctx, params.Scope{Kind: "stack", ID: stackID})
	if err != nil {
		return err
	}
	v.Nodes = append(v.Nodes, vars)
	prev := 0
	for i, e := range es {
		ts, err := f.Tiles.List(ctx, e.ID)
		if err != nil {
			return err
		}
		n := card("env:"+e.ID, KindEnv, e.Name)
		n.Slug, n.Color, n.Detail, n.Deck = e.Slug, e.Color, from(e), deck(len(ts))
		if in.Status {
			if n.Status, err = f.worstOf(ctx, "", ts); err != nil {
				return err
			}
		}
		v.Nodes = append(v.Nodes, n)
		if reads(ts, params.KindParam) {
			v.Edges = append(v.Edges, Edge{EdgeShared, vars.ID, n.ID})
		}
		r := Rung{EnvID: e.ID, Name: e.Name, Slug: e.Slug, Color: e.Color}
		if e.ReleaseID != nil {
			rel, err := f.Releases.Get(ctx, *e.ReleaseID)
			if err != nil {
				return err
			}
			r.Release = rel.Number
		}
		r.Behind = i > 0 && prev > r.Release
		prev = r.Release
		v.Compare = append(v.Compare, r)
	}
	return nil
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

func from(e store.Environment) string {
	switch {
	case e.FromKind == "branch" && e.Auto:
		return e.FromBranch + " · auto"
	case e.FromKind == "branch":
		return e.FromBranch
	case e.FromKind != "":
		return "promoted"
	}
	return e.Type
}

func (f *Flow) vars(ctx context.Context, s params.Scope) (Node, error) {
	ps, err := f.Params.List(ctx, s, true)
	n := card(KindVars, KindVars, "Variables")
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
	for _, t := range ts {
		if t.GitURL != "" && connector.Host(t.GitURL) == host {
			return true
		}
	}
	return false
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
	for _, k := range sortedKeys(env) {
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

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// ---- status ---------------------------------------------------------------

// rank orders the words for the worst-of roll-up (graph-ref §1): error >
// building/queued > unhealthy > the rest.
var rank = map[string]int{"error": 7, "building": 6, "queued": 6, "waiting": 6, "unhealthy": 5,
	"degraded": 4, "stopped": 3, "running": 2, "done": 1, "none": 0, "": -1}

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
			s.word = map[string]string{"queued": "queued", "running": "running", "ok": "done", "failed": "error"}[r.Status]
		}
	} else {
		if s.state, err = f.Tiles.State(ctx, t); err != nil {
			return s, err
		}
		s.word = s.state.Word
		for i, c := range s.state.Replicas {
			if i >= 3 {
				break
			}
			s.replicas = append(s.replicas, Sub{ID: fmt.Sprintf("replica:%s:%d", t.Slug, i+1), Kind: KindReplica,
				Name: c.Name, Status: c.State})
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
	if _, err := f.Envs.Get(ctx, envID); err != nil {
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
	v.Nodes = append(v.Nodes, vars)

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
			n := card("slice:"+p.ID, KindSlice, p.Slug)
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
				n.Subs = append(n.Subs, Sub{ID: "tile:" + host.Slug, Kind: tile.Managed, Name: host.Name})
			} else {
				g := ghost(v, "ref:"+host.ID, host.Name, inst.ScopeKind+" · "+inst.Engine)
				v.Edges = append(v.Edges, Edge{EdgeShared, n.ID, g})
			}
			v.Edges = append(v.Edges, Edge{EdgeRef, "tile:" + t.Slug, n.ID})
			slices = append(slices, n)
		}
	}

	mounted := map[string]bool{}
	proxied := false
	for _, t := range ts {
		if hosted[t.ID] {
			continue
		}
		n, err := f.tileCard(ctx, t, in.Status)
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
		id := "tile:" + t.Slug
		readsVars := false
		for _, r := range refs(t) {
			switch r.Kind {
			case params.KindTile:
				if to, ok := bySlug[r.Slug]; ok && !hosted[to.ID] && to.ID != t.ID && !joined[[2]string{id, "tile:" + r.Slug}] {
					v.Edges = append(v.Edges, Edge{EdgeRef, id, "tile:" + r.Slug})
					joined[[2]string{id, "tile:" + r.Slug}] = true
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
		if readsVars {
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
			a, b := "tile:"+t.Slug, "tile:"+dep
			if !joined[[2]string{a, b}] && !joined[[2]string{b, a}] {
				v.Edges = append(v.Edges, Edge{EdgeStartup, a, b})
			}
		}
	}

	// Detached volumes: declared here, mounted by nothing.
	vs, err := f.Volumes.List(ctx, volume.Scope{Kind: "env", ID: envID})
	if err != nil {
		return err
	}
	for _, vol := range vs {
		if vol.InstanceID != nil || mounted[vol.Slug] {
			continue
		}
		n := card("volume:"+vol.Slug, KindVolume, vol.Name)
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
			v.Edges = append(v.Edges, Edge{EdgeEgress, "tile:" + t.Slug, KindInternet})
		}
	}
	for _, sys := range []struct {
		on         bool
		id, detail string
	}{{proxied, KindProxy, "Caddy"}, {len(egress) > 0, KindInternet, "outbound"}} {
		if sys.on {
			n := card(sys.id, sys.id, strings.ToUpper(sys.id[:1])+sys.id[1:])
			n.Detail, n.System = sys.detail, true
			v.Nodes = append(v.Nodes, n)
			v.Divider = Divider
		}
	}
	return nil
}

// ghost adds a ref card once and returns its id.
func ghost(v *View, id, name, detail string) string {
	for _, n := range v.Nodes {
		if n.ID == id {
			return id
		}
	}
	n := card(id, KindRef, name)
	n.Detail, n.Static = detail, true
	v.Nodes = append(v.Nodes, n)
	return id
}

func (f *Flow) tileCard(ctx context.Context, t store.Tile, withStatus bool) (Node, error) {
	n := card("tile:"+t.Slug, t.Kind, t.Name)
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
		sl, _, _ := strings.Cut(l, ":")
		n.Volumes = append(n.Volumes, sl)
		n.Subs = append(n.Subs, Sub{ID: "volume:" + sl, Kind: KindVolume, Name: sl})
	}
	if t.Kind == tile.Managed {
		if in, err := f.Managed.GetByTile(ctx, t.ID); err == nil {
			vs, err := f.Volumes.List(ctx, volume.Scope{Kind: in.ScopeKind, ID: in.ScopeID})
			if err != nil {
				return n, err
			}
			for _, vol := range vs {
				if vol.InstanceID != nil && *vol.InstanceID == in.ID {
					n.Subs = append(n.Subs, Sub{ID: "volume:" + vol.Slug, Kind: KindVolume, Name: vol.Name})
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
		if len(st.replicas) > 1 {
			n.Subs = append(n.Subs, st.replicas...)
		}
	}
	n.H += SubH * len(n.Subs)
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
		v.Divider = 0
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
	if v.Divider != 0 {
		for _, n := range v.Nodes {
			if n.System {
				return
			}
		}
		v.Divider = 0
	}
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
