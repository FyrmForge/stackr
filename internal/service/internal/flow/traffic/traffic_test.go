package traffic_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/traffic"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/managed"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/stack"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	ltraffic "github.com/FyrmForge/stackr/internal/service/internal/leaf/traffic"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/internal/storetest"
	"github.com/FyrmForge/stackr/internal/worker"
)

func labelled(labels map[string]string) map[string]string {
	labels[docker.LabelManaged] = "true"
	return labels
}

// fake docker + fixture conntrack: web dials db's VIP (the pause container),
// the reply comes from db's replica; an exited container and ssh to the
// host are nobody's.
func world() (*traffic.Flow, *dockerfake.Fake, *time.Time) {
	f := dockerfake.New()
	f.Containers = []docker.Container{
		{
			ID:     "w1",
			State:  "running",
			IPs:    []string{"172.20.0.3", "172.21.0.6"},
			Labels: labelled(map[string]string{tile.LabelTile: "web", tile.LabelRole: "replica"}),
		},
		{
			ID:     "dp",
			State:  "running",
			IPs:    []string{"172.20.0.2"},
			Labels: labelled(map[string]string{tile.LabelTile: "db", tile.LabelRole: "pause"}),
		},
		{
			ID:     "d1",
			State:  "running",
			IPs:    []string{"172.20.0.4"},
			Labels: labelled(map[string]string{tile.LabelTile: "db", tile.LabelRole: "replica"}),
		},
		{
			ID:     "old",
			State:  "exited",
			IPs:    []string{"172.20.0.9"},
			Labels: labelled(map[string]string{tile.LabelTile: "gone"}),
		},
	}
	// The proxy is on the default bridge and web's ingress network.
	f.Details = map[string]docker.Detail{
		"stackr-proxy": {Networks: map[string]string{"bridge": "172.17.0.2", "stackr-ingress-web": "172.21.0.5"}},
	}
	f.GatewayIPs = []string{"172.20.0.1", "172.21.0.1"}
	now := time.Unix(1000, 0)
	fl := &traffic.Flow{
		Tiles:   tile.New(nil, f, nil),
		Envs:    environment.New(nil, f),
		Domains: domain.New(nil, f, "stackr-proxy"),
		Traffic: ltraffic.New(),
		Path:    filepath.Join("testdata", "tick1"),
		Now:     func() time.Time { return now },
	}
	return fl, f, &now
}

func TestTick(t *testing.T) {
	ctx := context.Background()
	fl, _, now := world()
	if err := fl.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	fl.Path, *now = filepath.Join("testdata", "tick2"), now.Add(5*time.Second)
	if err := fl.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	got := fl.Traffic.Snapshot()
	want := map[ltraffic.Pair]float64{
		{From: "web", To: "db"}: 1000,
		{From: "db", To: "web"}: 10000,
	}
	if len(got) != len(want) {
		t.Fatalf("snapshot = %v, want %v", got, want)
	}
	for p, v := range want {
		if got[p] != v {
			t.Errorf("%v = %v, want %v", p, got[p], v)
		}
	}
}

// Task 3: the proxy container and a gateway source are the proxy; a
// tile's unknown far end is the internet; outside <-> proxy is nobody's.
func TestTickSystemEnds(t *testing.T) {
	ctx := context.Background()
	fl, _, now := world()
	fl.Path = filepath.Join("testdata", "ingress1")
	if err := fl.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	fl.Path, *now = filepath.Join("testdata", "ingress2"), now.Add(5*time.Second)
	if err := fl.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	got := fl.Traffic.Snapshot()
	want := map[ltraffic.Pair]float64{
		{From: "proxy", To: "web"}:    200,
		{From: "web", To: "proxy"}:    1100,
		{From: "web", To: "internet"}: 200,
		{From: "internet", To: "web"}: 400,
	}
	if len(got) != len(want) {
		t.Fatalf("snapshot = %v, want %v", got, want)
	}
	for p, v := range want {
		if got[p] != v {
			t.Errorf("%v = %v, want %v", p, got[p], v)
		}
	}
}

// No table: nothing sampled and Docker never asked.
func TestTickNoTable(t *testing.T) {
	fl, f, _ := world()
	fl.Path = filepath.Join(t.TempDir(), "none")
	if err := fl.Tick(context.Background()); err != nil || len(f.Calls()) != 0 || fl.Traffic.Seq() != 0 {
		t.Errorf("err %v, calls %v, seq %d", err, f.Calls(), fl.Traffic.Seq())
	}
}

func TestCheck(t *testing.T) {
	for path, want := range map[string]string{
		filepath.Join("testdata", "tick1"):  "",
		filepath.Join("testdata", "noacct"): "nf_conntrack_acct=1",
		filepath.Join("testdata", "none"):   "no conntrack table",
	} {
		if got := traffic.Check(path); (want == "") != (got == "") || !strings.Contains(got, want) {
			t.Errorf("Check(%s) = %q, want %q", path, got, want)
		}
	}
}

// 3b: web and jobs both bind one Postgres instance; each lane lands on the
// consumer's own slice, the instance keeps lanes to an unbound tile, and
// lanes not touching the env are left out.
func TestEdgesSlices(t *testing.T) {
	ctx := context.Background()
	s := storetest.Store(t)
	now := time.Now()
	org, stk, env := uuid.NewString(), uuid.NewString(), uuid.NewString()
	must(t, s.Orgs.Create(ctx, store.Org{
		ID:        org,
		Name:      "acme",
		Slug:      "acme",
		EnvColors: "{}",
		Settings:  "{}",
		CreatedAt: now,
	}))
	must(t, s.Stacks.Create(ctx, store.Stack{
		ID:        stk,
		OrgID:     org,
		Name:      "shop",
		Slug:      "shop",
		Settings:  "{}",
		CreatedAt: now,
	}))
	must(t, s.Environments.Create(ctx, store.Environment{
		ID:         env,
		StackID:    stk,
		Name:       "dev",
		Slug:       "dev",
		Type:       "static",
		Settings:   "{}",
		Network:    "n",
		FromKind:   "branch",
		FromBranch: "main",
		CreatedAt:  now,
	}))
	tiles := tile.New(s.Tiles, dockerfake.New(), nil)
	ids := map[string]string{}
	for _, n := range []string{"web", "jobs", "cron", "pg"} {
		tl, err := tiles.Create(ctx, store.Tile{
			StackID:       stk,
			EnvironmentID: env,
			Name:          n,
			Kind:          tile.Image,
			ImageRef:      n + ":1",
		})
		must(t, err)
		ids[n] = tl.ID
	}
	ml := managed.New(s.ManagedInstances, s.Provisions, s.Bindings)
	m, err := ml.Create(
		ctx,
		ids["pg"],
		"postgres",
		"admin",
		"pg:5432",
	)
	must(t, err)
	slice := map[string]string{} // consumer slug -> the slice tile it binds
	for _, n := range []string{"web", "jobs"} {
		from := "s:dev:pg"
		sl, err := tiles.Create(ctx, store.Tile{
			StackID:       stk,
			EnvironmentID: env,
			Name:          n + "-db",
			Kind:          tile.Slice,
			ProvisionFrom: &from,
		})
		must(t, err)
		p, err := ml.CreateProvision(ctx, m, sl.ID, managed.Slice{
			DBName: n,
			DBUser: n,
		})
		must(t, err)
		_, err = ml.Bind(ctx, p, ids[n], managed.Cred{
			Access: "write",
			User:   n + "_app",
		})
		must(t, err)
		slice[n] = sl.ID
	}
	// far lives in another env of the stack and talks to pg directly.
	prd := uuid.NewString()
	must(t, s.Environments.Create(ctx, store.Environment{
		ID:         prd,
		StackID:    stk,
		Name:       "prd",
		Slug:       "prd",
		Type:       "static",
		Settings:   "{}",
		Network:    "n2",
		FromKind:   "branch",
		FromBranch: "main",
		CreatedAt:  now,
	}))
	far, err := tiles.Create(ctx, store.Tile{
		StackID:       stk,
		EnvironmentID: prd,
		Name:          "far",
		Kind:          tile.Image,
		ImageRef:      "far:1",
	})
	must(t, err)
	fl := &traffic.Flow{
		Tiles:   tiles,
		Envs:    environment.New(s.Environments, nil),
		Stacks:  stack.New(s.Stacks),
		Managed: ml,
		Traffic: ltraffic.New(),
	}
	ipMap := map[string]string{
		"10.0.0.2": ids["web"],
		"10.0.0.3": ids["jobs"],
		"10.0.0.4": ids["cron"],
		"10.0.0.9": ids["pg"],
		"10.1.0.2": "elsewhere",
		"10.1.0.3": "elsewhere-too",
		"10.2.0.2": far.ID,
	}
	dump := func(n int) []byte {
		var b strings.Builder
		for i, src := range []string{"10.0.0.2", "10.0.0.3", "10.0.0.4", "10.1.0.2", "10.2.0.2"} {
			dst := "10.0.0.9"
			if src == "10.1.0.2" {
				dst = "10.1.0.3"
			}
			b.WriteString("ipv4 2 tcp 6 100 ESTABLISHED src=" + src + " dst=" + dst + " sport=" + itoa(1000+i) +
				" dport=5432 bytes=" + itoa(n) + " src=" + dst + " dst=" + src + " sport=5432 dport=" + itoa(1000+i) +
				" bytes=" + itoa(2*n) + "\n")
		}
		return []byte(b.String())
	}
	fl.Traffic.Sample(ipMap, dump(0), now)
	fl.Traffic.Sample(ipMap, dump(500), now.Add(5*time.Second))
	got, err := fl.Edges(ctx, env)
	must(t, err)
	want := map[ltraffic.Pair]float64{
		{From: ids["web"], To: slice["web"]}:   100,
		{From: slice["web"], To: ids["web"]}:   200,
		{From: ids["jobs"], To: slice["jobs"]}: 100,
		{From: slice["jobs"], To: ids["jobs"]}: 200,
		{From: ids["cron"], To: ids["pg"]}:     100,
		{From: ids["pg"], To: ids["cron"]}:     200,
		{From: far.ID, To: ids["pg"]}:          100,
		{From: ids["pg"], To: far.ID}:          200,
	}
	names := map[string]string{
		ids["web"]:    "web",
		ids["jobs"]:   "jobs",
		ids["cron"]:   "cron",
		ids["pg"]:     "pg",
		slice["web"]:  "web-db",
		slice["jobs"]: "jobs-db",
		far.ID:        "shop/prd/far",
	}
	if len(got) != len(want) {
		t.Fatalf("edges = %+v, want %v", got, want)
	}
	for _, e := range got {
		if want[ltraffic.Pair{From: e.From, To: e.To}] != e.BPS {
			t.Errorf("edge %+v not wanted", e)
		}
		if e.FromName != names[e.From] || e.ToName != names[e.To] {
			t.Errorf("edge %+v named %s -> %s, want %s -> %s", e, e.FromName, e.ToName, names[e.From], names[e.To])
		}
	}
}

// ctLine is one conntrack entry: original src->dst, the reply back.
func ctLine(src, dst string, sport, orig, reply int) string {
	return "ipv4 2 tcp 6 86399 ESTABLISHED src=" + src + " dst=" + dst + " sport=" + itoa(sport) +
		" dport=80 packets=3 bytes=" + itoa(orig) + " src=" + dst + " dst=" + src + " sport=80 dport=" +
		itoa(sport) + " packets=3 bytes=" + itoa(reply) + " mark=0 use=1\n"
}

// A worker's tiles join the lanes: its app calls the controller's db (that
// connection sits in both nodes' tables and counts once), its own Caddy
// (a gateway) serves it, and a connection it only forwards is nobody's. A
// missed read is a gap, and the next answer counts from where it left off,
// not the connection's whole life.
func TestTickWorkers(t *testing.T) {
	ctx := context.Background()
	fl, f, now := world()
	f.GatewayIPs = append(f.GatewayIPs, "172.30.0.1")
	local := filepath.Join(t.TempDir(), "ct")
	fl.Path = local
	app := map[string]string{"10.210.1.5": "app"}
	var workerLines string
	up := true
	fl.Remote = func(context.Context) map[string]worker.Traffic {
		if !up {
			return nil
		}
		return map[string]worker.Traffic{"w1": {Tiles: app, Lines: workerLines, Gateways: []string{"10.210.1.1"}}}
	}
	tick := func(appDB, proxyApp int) {
		t.Helper()
		cross := ctLine("10.210.1.5", "172.20.0.2", 7000, appDB, 2*appDB)
		// the controller's copy of the same connection, its counters off a
		// little: counted here too, it would skew the lane
		must(t, os.WriteFile(local, []byte(ctLine("10.210.1.5", "172.20.0.2", 7000, appDB+7, 2*appDB+7)), 0o644))
		// the controller's gateway, the worker's own (its host-network Caddy)
		// and another node's Caddy over the mesh (its transit address): all
		// the proxy
		workerLines = cross + ctLine("172.30.0.1", "10.210.1.5", 7001, proxyApp, proxyApp) +
			ctLine("10.210.1.1", "10.210.1.5", 7003, proxyApp, proxyApp) +
			ctLine("10.207.255.6", "10.210.1.5", 7004, proxyApp, proxyApp) +
			ctLine("10.212.0.3", "10.211.0.4", 7002, 99999, 99999) // forwarded for others
		must(t, fl.Tick(ctx))
		*now = now.Add(5 * time.Second)
	}
	tick(1000, 100)
	tick(6000, 600)
	got := fl.Traffic.Snapshot()
	want := map[ltraffic.Pair]float64{
		{From: "app", To: "db"}:    1000, // once, not twice
		{From: "db", To: "app"}:    2000,
		{From: "proxy", To: "app"}: 300,
		{From: "app", To: "proxy"}: 300,
	}
	if len(got) != len(want) {
		t.Fatalf("snapshot = %v, want %v", got, want)
	}
	for p, v := range want {
		if got[p] != v {
			t.Errorf("%v = %v, want %v", p, got[p], v)
		}
	}

	// The worker misses a tick: its lanes go quiet, no spike after.
	up = false
	tick(6000, 600)
	if v := fl.Traffic.Snapshot()[ltraffic.Pair{From: "proxy", To: "app"}]; v != 0 {
		t.Errorf("missed read: proxy->app = %v, want a gap", v)
	}
	up = true
	tick(11000, 1100)
	if v := fl.Traffic.Snapshot()[ltraffic.Pair{From: "proxy", To: "app"}]; v != 300 {
		t.Errorf("after the gap: proxy->app = %v, want 300 (not its whole life)", v)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
