package service_test

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// world is the fixture tree every level test reads:
//
//	acme (owner) ── connector gh ──config/source──> shop
//	shop: dev (release 2), prod (release 1), stack param
//	dev: api ─ref→ worker, api ─ref→ slice main (pg hosted here),
//	     api ─shared→ ghost stack.cache, vars ─shared→ api,
//	     web ─startup→ api, api mounts uploads, domain on api,
//	     volume old detached, api's last job failed
//	beta: a second org, empty
type world struct {
	e                               *servicetest.Env
	user, acme, beta, conn          string
	shop, dev, prod                 string
	api, worker, web, pg, provision string
	vols                            map[string]string // slug -> id
}

func tileRow(stackID, envID, slug, kind string, edit func(*store.Tile)) store.Tile {
	t := store.Tile{ID: uuid.NewString(), StackID: stackID, EnvironmentID: envID, Name: slug, Slug: slug, Kind: kind,
		ImageRef: "nginx:1", EnvJSON: "{}", BuildArgs: "{}", Replicas: 1, UpdatePolicy: "manual", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if edit != nil {
		edit(&t)
	}
	return t
}

func seedWorld(t *testing.T) world {
	t.Helper()
	ctx := context.Background()
	e := servicetest.New(t)
	w := world{e: e}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	w.user = e.User(t, "o@x.io", false)
	w.acme, w.beta = e.Org(t, "acme"), e.Org(t, "beta")
	e.Member(t, w.acme, w.user, "owner")
	e.Member(t, w.beta, w.user, "owner")
	w.conn = e.Connector(t, w.acme, "s")

	st, err := e.O.CreateStack(ctx, w.acme, "shop", "")
	must(err)
	w.shop = st.ID
	st.ConfigConnectorID, st.ConfigRepo = w.conn, "https://github.com/acme/config.git"
	must(e.Store.Stacks.Update(ctx, st))
	dev, err := e.O.CreateEnv(ctx, w.shop, "dev", service.EnvSpec{Type: "static", FromKind: "branch", FromBranch: "main", Color: "#0a0"})
	must(err)
	prod, err := e.O.CreateEnv(ctx, w.shop, "prod", service.EnvSpec{Type: "static", FromKind: "branch", FromBranch: "main"})
	must(err)
	w.dev, w.prod = dev.ID, prod.ID
	for n, env := range map[int]string{2: w.dev, 1: w.prod} {
		rid := uuid.NewString()
		must(e.Store.Releases.Create(ctx, store.Release{ID: rid, StackID: w.shop, Number: n, CreatedAt: time.Now()}))
		row, err := e.Store.Environments.Get(ctx, env)
		must(err)
		row.ReleaseID = &rid
		must(e.Store.Environments.Update(ctx, row))
	}
	must(e.O.SetParams(ctx, service.ParamScope{Kind: "env", ID: w.dev}, []service.ParamEntry{
		{Collection: "app", Name: "key", Kind: "param", Value: "v"}, {Collection: "app", Name: "pw", Kind: "secret", Value: "s"}}))
	must(e.O.SetParams(ctx, service.ParamScope{Kind: "stack", ID: w.shop}, []service.ParamEntry{
		{Collection: "app", Name: "region", Kind: "param", Value: "eu"}}))

	api := tileRow(w.shop, w.dev, "api", "service", func(t *store.Tile) {
		t.GitURL = "https://github.com/acme/api.git"
		t.EnvJSON = `{"W":"${{ tile.worker.url }}","K":"${{ params.app.key }}","C":"${{ stack.cache.url }}"}`
		t.Volumes = "uploads:/data"
		t.DependsOn = "worker"
	})
	worker := tileRow(w.shop, w.dev, "worker", "image", nil)
	web := tileRow(w.shop, w.dev, "web", "image", func(t *store.Tile) { t.DependsOn = "api:healthy" })
	pg := tileRow(w.shop, w.dev, "pg", "managed", nil)
	for _, tl := range []store.Tile{api, worker, web, pg} {
		must(e.Store.Tiles.Create(ctx, tl))
	}
	w.api, w.worker, w.web, w.pg = api.ID, worker.ID, web.ID, pg.ID
	inst := store.ManagedInstance{ID: uuid.NewString(), TileID: pg.ID, Engine: "postgres", ScopeKind: "env", ScopeID: w.dev,
		AdminUser: "a", AdminPassword: "p", CreatedAt: time.Now()}
	must(e.Store.ManagedInstances.Create(ctx, inst))
	w.provision = uuid.NewString()
	must(e.Store.Provisions.Create(ctx, store.Provision{ID: w.provision, InstanceID: inst.ID, ConsumerTileID: &w.api,
		Slug: "main", DBName: "main", Outputs: "{}", OnRemove: "keep", CreatedAt: time.Now()}))
	w.vols = map[string]string{}
	for _, sl := range []string{"uploads", "old"} {
		w.vols[sl] = uuid.NewString()
		must(e.Store.Volumes.Create(ctx, store.Volume{ID: w.vols[sl], ScopeKind: "env", ScopeID: w.dev, Slug: sl,
			Name: sl, CreatedAt: time.Now()}))
	}
	must(e.Store.Domains.Create(ctx, store.Domain{ID: uuid.NewString(), TileID: w.api, Host: "api.acme.io", HTTPS: true,
		ProxyJSON: "{}", CreatedAt: time.Now()}))
	fin := time.Now()
	must(e.Store.Jobs.Create(ctx, store.Job{ID: uuid.NewString(), Kind: "deploy", State: "failed", LockSet: store.StringList{w.api},
		Payload: "{}", CreatedAt: time.Now(), FinishedAt: &fin}))
	return w
}

func nodes(v service.GraphView) map[string]service.GraphNode {
	m := map[string]service.GraphNode{}
	for _, n := range v.Nodes {
		m[n.ID] = n
	}
	return m
}

func edges(v service.GraphView) []string {
	var out []string
	for _, e := range v.Edges {
		out = append(out, e.Kind+" "+e.From+" "+e.To)
	}
	sort.Strings(out)
	return out
}

func ids(v service.GraphView) string {
	var out []string
	for _, n := range v.Nodes {
		out = append(out, n.ID)
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

func TestCanvasHome(t *testing.T) {
	w := seedWorld(t)
	v, err := w.e.O.Canvas(context.Background(), service.CanvasScope{Kind: service.CanvasHome, ID: w.user}, service.ShowAll)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ids(v), "org:"+min(w.acme, w.beta)+" org:"+max(w.acme, w.beta); got != want {
		t.Fatalf("home cards = %s, want %s", got, want)
	}
	ns := nodes(v)
	if a := ns["org:"+w.acme]; a.Status != "error" || a.Slug != "acme" || a.Detail != "1 stack" {
		t.Errorf("acme = %+v, want worst-of error, slug acme, 1 stack", a)
	}
	if b := ns["org:"+w.beta]; b.Status != "" || b.X == ns["org:"+w.acme].X && b.Y == ns["org:"+w.acme].Y {
		t.Errorf("beta = %+v: no tiles, no status, and its own grid cell", b)
	}
	if len(v.Edges) != 0 {
		t.Errorf("home edges = %v, want none", edges(v))
	}
}

func TestCanvasOrg(t *testing.T) {
	w := seedWorld(t)
	v, err := w.e.O.Canvas(context.Background(), service.CanvasScope{Kind: service.CanvasOrg, ID: w.acme}, service.ShowAll)
	if err != nil {
		t.Fatal(err)
	}
	ns := nodes(v)
	st := ns["stack:"+w.shop]
	if st.Status != "error" || st.Detail != "2 envs" || st.Deck != 1 {
		t.Errorf("shop = %+v, want error, 2 envs, deck 1", st)
	}
	if c := ns["connector:"+w.conn]; c.Kind != "connector" || c.X >= st.X {
		t.Errorf("connector = %+v, want a card left of the stacks", c)
	}
	want := []string{"config connector:" + w.conn + " stack:" + w.shop, "source connector:" + w.conn + " stack:" + w.shop}
	if got := edges(v); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("org edges = %v, want %v", got, want)
	}
}

func TestCanvasStack(t *testing.T) {
	w := seedWorld(t)
	v, err := w.e.O.Canvas(context.Background(), service.CanvasScope{Kind: service.CanvasStack, ID: w.shop}, service.ShowAll)
	if err != nil {
		t.Fatal(err)
	}
	ns := nodes(v)
	if d := ns["env:"+w.dev]; d.Color != "#0a0" || d.Status != "error" || d.Slug != "dev" {
		t.Errorf("dev = %+v", d)
	}
	if p := ns["env:"+w.prod]; p.Status != "" {
		t.Errorf("prod has no tiles, status = %q", p.Status)
	}
	if vars := ns["vars"]; vars.Params != 1 || vars.Secrets != 0 {
		t.Errorf("stack vars = %+v, want 1 param", vars)
	}
	if got := edges(v); len(got) != 1 || got[0] != "shared vars env:"+w.dev {
		t.Errorf("stack edges = %v, want vars -> dev only (prod reads nothing)", got)
	}
	if len(v.Compare) != 2 || v.Compare[0].Release != 2 || v.Compare[1].Release != 1 || !v.Compare[1].Behind || v.Compare[0].Behind {
		t.Errorf("compare = %+v, want dev #2 then prod #1, prod behind dev", v.Compare)
	}
}

func TestCanvasEnv(t *testing.T) {
	w := seedWorld(t)
	ctx := context.Background()
	s := service.CanvasScope{Kind: service.CanvasEnv, ID: w.dev}
	v, err := w.e.O.Canvas(ctx, s, service.ShowAll)
	if err != nil {
		t.Fatal(err)
	}
	// Env node ids are row ids (the Traffic verb's lane ends); name them back.
	name := strings.NewReplacer(w.api, "api", w.worker, "worker", w.web, "web", w.pg, "pg", w.provision, "slice",
		w.vols["uploads"], "uploads", w.vols["old"], "old")
	named := strings.Fields(name.Replace(ids(v)))
	sort.Strings(named)
	if got, want := strings.Join(named, " "), "api old proxy ref:stack.cache slice vars web worker"; got != want {
		t.Fatalf("env cards = %s\nwant       %s (pg rides under its slice)", got, want)
	}
	want := []string{
		"ingress proxy api",
		"ref api slice",
		"ref api worker",
		"shared api ref:stack.cache",
		"shared vars api",
		"startup web api",
	}
	got := edges(v)
	for i := range got {
		got[i] = name.Replace(got[i])
	}
	sort.Strings(got)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("env edges =\n%v\nwant\n%v (api->worker startup is covered by the ref)", got, want)
	}
	ns := nodes(v)
	api := ns[w.api]
	if api.Status != "error" || len(api.Domains) != 1 || len(api.Subs) != 1 || api.Subs[0].ID != w.vols["uploads"] ||
		api.H != 96+30 || api.Detail != "api" {
		t.Errorf("api = %+v", api)
	}
	if sl := ns[w.provision]; len(sl.Subs) != 1 || sl.Subs[0].ID != w.pg {
		t.Errorf("slice = %+v, want pg as its sub-tile", sl)
	}
	if g := ns["ref:stack.cache"]; !g.Static || g.Kind != "ref" {
		t.Errorf("ghost = %+v, want a static ref card", g)
	}
	if p := ns["proxy"]; !p.System || v.Divider == 0 || p.X+p.W > v.Divider {
		t.Errorf("proxy = %+v divider %d, want a system card behind the wall", p, v.Divider)
	}
	for _, n := range v.Nodes {
		if !n.System && n.X < v.Divider {
			t.Errorf("%s at x %d, inside the system column (divider %d)", n.ID, n.X, v.Divider)
		}
		if n.X%22 != 0 || n.Y%22 != 0 {
			t.Errorf("%s at %d,%d, off the 22 px grid", n.ID, n.X, n.Y)
		}
	}
	if api.X >= ns[w.worker].X {
		t.Errorf("api (%d) should sit left of worker (%d), which it uses", api.X, ns[w.worker].X)
	}

	// The query params: no system column, no startup edges.
	v2, err := w.e.O.Canvas(ctx, s, service.GraphShow{Refs: true, Traffic: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := nodes(v2)["proxy"]; ok || v2.Divider != 0 {
		t.Errorf("system off: proxy still drawn, divider %d", v2.Divider)
	}
	for _, e := range edges(v2) {
		if strings.HasPrefix(e, "startup") || strings.HasPrefix(e, "ingress") {
			t.Errorf("edge %s survived the filter", e)
		}
	}
}

func TestCanvasPositions(t *testing.T) {
	w := seedWorld(t)
	ctx := context.Background()
	o := w.e.O
	s := service.CanvasScope{Kind: service.CanvasEnv, ID: w.dev}
	before, err := o.Canvas(ctx, s, service.ShowAll)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.SetPosition(ctx, s, w.web, service.Point{X: 1100, Y: 880}); err != nil {
		t.Fatal(err)
	}
	after, err := o.Canvas(ctx, s, service.ShowAll)
	if err != nil {
		t.Fatal(err)
	}
	b, a := nodes(before), nodes(after)
	for id, n := range a {
		switch {
		case id == w.web && (n.X != 1100 || n.Y != 880 || !n.Saved):
			t.Errorf("web = %d,%d saved %v, want the drop", n.X, n.Y, n.Saved)
		case id != w.web && (n.X != b[id].X || n.Y != b[id].Y):
			t.Errorf("%s jumped from %d,%d to %d,%d: the first drop saves every card", id, b[id].X, b[id].Y, n.X, n.Y)
		case id != w.web && !n.Saved:
			t.Errorf("%s not saved by the first drop", id)
		}
	}
	for _, c := range []struct {
		id   string
		want error
	}{{"nope", errs.ErrNotFound}, {"ref:stack.cache", nil}} {
		err := o.SetPosition(ctx, s, c.id, service.Point{X: 1, Y: 1})
		if c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.id, err, c.want)
		}
		if c.want == nil {
			if _, ok := errs.IsInvalid(err); !ok {
				t.Errorf("%s: %v, want invalid (a ghost does not move)", c.id, err)
			}
		}
	}
	// Another canvas keeps its own rows; reset forgets only this one.
	if err := o.SetPosition(ctx, service.CanvasScope{Kind: service.CanvasStack, ID: w.shop}, "env:"+w.dev, service.Point{X: 0, Y: 0}); err != nil {
		t.Fatal(err)
	}
	if err := o.ResetPositions(ctx, s); err != nil {
		t.Fatal(err)
	}
	again, _ := o.Canvas(ctx, s, service.ShowAll)
	if n := nodes(again)[w.web]; n.Saved || n.X != b[w.web].X {
		t.Errorf("after reset web = %+v, want arranged again", n)
	}
	st, _ := o.Canvas(ctx, service.CanvasScope{Kind: service.CanvasStack, ID: w.shop}, service.ShowAll)
	if !nodes(st)["env:"+w.dev].Saved {
		t.Error("reset of dev forgot the stack canvas's rows")
	}
}

func TestCanvasAnnotations(t *testing.T) {
	w := seedWorld(t)
	ctx := context.Background()
	o := w.e.O
	s := service.CanvasScope{Kind: service.CanvasOrg, ID: w.acme}
	other := service.CanvasScope{Kind: service.CanvasOrg, ID: w.beta}
	n, _, err := o.SetAnnotation(ctx, s, service.Annotation{Kind: "note", Text: "hello", X: 10, Y: 20, W: 160, H: 60})
	if err != nil || n.ID == "" {
		t.Fatalf("create note: %+v %v", n, err)
	}
	if _, _, err := o.SetAnnotation(ctx, s, service.Annotation{Kind: "box", W: 10, H: 10}); err == nil {
		t.Error("a 10x10 box was accepted")
	}
	if err := o.SetPosition(ctx, s, "note:"+n.ID, service.Point{X: 300, Y: 400}); err != nil {
		t.Fatal(err)
	}
	if err := o.SetPosition(ctx, other, "note:"+n.ID, service.Point{X: 1, Y: 1}); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("moving acme's note from beta's canvas: %v, want not found", err)
	}
	v, _ := o.Canvas(ctx, s, service.ShowAll)
	if len(v.Notes) != 1 || v.Notes[0].X != 300 || v.Notes[0].Text != "hello" {
		t.Errorf("notes = %+v", v.Notes)
	}
	n.Text = "  "
	if _, deleted, err := o.SetAnnotation(ctx, s, n); err != nil || !deleted {
		t.Errorf("emptied note: deleted %v, %v", deleted, err)
	}
	if as, _ := o.Annotations(ctx, s); len(as) != 0 {
		t.Errorf("annotations left = %+v", as)
	}
	if err := o.DeleteAnnotation(ctx, s, n.ID); err != nil {
		t.Errorf("deleting a gone note: %v", err)
	}
}
