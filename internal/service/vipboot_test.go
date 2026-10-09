package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/hostgrant"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/internal/storetest"
	"github.com/FyrmForge/stackr/internal/service/internal/vip"
)

// A panel restart gives a fresh vip.Table; without the boot rebuild the first
// route redeclares STACKR-VIP and drops every other tile's jump.
func TestRebuildVIPsKeepsOtherTiles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	fake := dockerfake.New()
	var scripts []string
	table := vip.New() // the restarted panel's empty table
	table.Restore = func(_ context.Context, s string) error { scripts = append(scripts, s); return nil }
	table.Exec = func(_ context.Context, argv ...string) error { // -D finds nothing, like iptables with no jump
		if argv[1] == "-D" {
			return context.Canceled
		}
		return nil
	}
	orch, err := New(
		Config{DataDir: dir, SecretsKey: testKey, Conntrack: dir + "/nf_conntrack"},
		WithDocker(fake),
		WithVIP(table),
		WithProxy(func(context.Context, json.RawMessage) error { return nil }),
	)
	must(t, err)
	t.Cleanup(func() { _ = orch.Close() })
	st := storetest.Open(t, filepath.Join(dir, "stackr.db"))
	org, stack, env := uuid.NewString(), uuid.NewString(), uuid.NewString()
	now := time.Now()
	must(t, st.Orgs.Create(ctx, store.Org{ID: org, Name: "acme", Slug: "acme", EnvColors: "{}", Settings: "{}", CreatedAt: now}))
	must(t, st.Stacks.Create(ctx, store.Stack{ID: stack, OrgID: org, Name: "shop", Slug: "shop", Settings: "{}", CreatedAt: now}))
	must(t, st.Environments.Create(ctx, store.Environment{
		ID: env, StackID: stack, Name: "dev", Slug: "dev", Type: "static", Settings: "{}",
		Network: "n", FromKind: "branch", FromBranch: "main", CreatedAt: now,
	}))
	tiles := map[string]store.Tile{}
	for _, slug := range []string{"a", "b"} {
		tl := bootTile(stack, env, slug)
		must(t, st.Tiles.Create(ctx, tl))
		tiles[slug] = tl
		fake.Containers = append(fake.Containers,
			docker.Container{ID: "p" + slug, State: "running", Labels: map[string]string{tile.LabelTile: tl.ID, tile.LabelRole: "pause"}},
			docker.Container{ID: "r" + slug, State: "running", Labels: map[string]string{tile.LabelTile: tl.ID, tile.LabelRole: "replica"}})
	}
	fake.Details = map[string]docker.Detail{
		"pa": {Running: true, Networks: map[string]string{"n": "10.0.0.2"}},
		"ra": {Running: true, Networks: map[string]string{"n": "10.0.0.3"}},
		"pb": {Running: true, Networks: map[string]string{"n": "10.0.0.4"}},
		"rb": {Running: true, Networks: map[string]string{"n": "10.0.0.5"}},
	}
	// A third tile whose pause container is gone is skipped, not fatal.
	gone := bootTile(stack, env, "gone")
	must(t, st.Tiles.Create(ctx, gone))

	must(t, orch.rebuildVIPs(ctx))
	scripts = nil
	must(t, orch.tiles.Route(ctx, tiles["a"], "n", nil))
	if len(scripts) != 1 {
		t.Fatalf("restore calls = %d, want 1", len(scripts))
	}
	for _, want := range []string{"-d 10.0.0.2/32", "-d 10.0.0.4/32", "--to-destination 10.0.0.5"} {
		if !strings.Contains(scripts[0], want) {
			t.Errorf("script after routing a lacks %q:\n%s", want, scripts[0])
		}
	}
}

func bootTile(stackID, envID, slug string) store.Tile {
	return store.Tile{
		ID: uuid.NewString(), StackID: stackID, EnvironmentID: envID, Name: slug, Slug: slug,
		Kind: tile.Image, ImageRef: "nginx:1", EnvJSON: "{}", BuildArgs: "{}", Replicas: 1,
		UpdatePolicy: "manual", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
}

// bootRig is a panel with a fresh vip.Table and two routed tiles, a and b.
type bootRig struct {
	orch    *Orchestrator
	fake    *dockerfake.Fake
	table   *vip.Table
	scripts *[]string
	st      *store.Store
	stack   string
	env     string
	tiles   map[string]store.Tile
}

func newBootRig(t *testing.T) bootRig {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	fake := dockerfake.New()
	scripts := &[]string{}
	table := vip.New()
	table.Restore = func(ctx context.Context, s string) error {
		*scripts = append(*scripts, s)
		return ctx.Err()
	}
	table.Exec = func(_ context.Context, argv ...string) error { // -D finds nothing, like iptables with no jump
		if argv[1] == "-D" {
			return context.Canceled
		}
		return nil
	}
	orch, err := New(
		Config{DataDir: dir, SecretsKey: testKey, Conntrack: dir + "/nf_conntrack"},
		WithDocker(fake),
		WithVIP(table),
		WithProxy(func(context.Context, json.RawMessage) error { return nil }),
	)
	must(t, err)
	t.Cleanup(func() { _ = orch.Close() })
	st := storetest.Open(t, filepath.Join(dir, "stackr.db"))
	org, stack, env := uuid.NewString(), uuid.NewString(), uuid.NewString()
	now := time.Now()
	must(t, st.Orgs.Create(ctx, store.Org{ID: org, Name: "acme", Slug: "acme", EnvColors: "{}", Settings: "{}", CreatedAt: now}))
	must(t, st.Stacks.Create(ctx, store.Stack{ID: stack, OrgID: org, Name: "shop", Slug: "shop", Settings: "{}", CreatedAt: now}))
	must(t, st.Environments.Create(ctx, store.Environment{
		ID: env, StackID: stack, Name: "dev", Slug: "dev", Type: "static", Settings: "{}",
		Network: "n", FromKind: "branch", FromBranch: "main", CreatedAt: now,
	}))
	r := bootRig{orch: orch, fake: fake, table: table, scripts: scripts, st: st, stack: stack, env: env, tiles: map[string]store.Tile{}}
	fake.Details = map[string]docker.Detail{}
	fake.Err = map[string]error{}
	for i, slug := range []string{"a", "b"} {
		r.addTile(t, slug, "10.0.0."+string(rune('2'+2*i)), "10.0.0."+string(rune('3'+2*i)))
	}
	*scripts = nil // New's own boot rebuild saw an empty Docker
	return r
}

// addTile creates a tile row, its pause container and one replica.
func (r bootRig) addTile(t *testing.T, slug, pauseIP, replicaIP string) {
	t.Helper()
	tl := bootTile(r.stack, r.env, slug)
	must(t, r.st.Tiles.Create(context.Background(), tl))
	r.tiles[slug] = tl
	r.fake.Containers = append(r.fake.Containers,
		docker.Container{ID: "p" + slug, State: "running", Labels: map[string]string{tile.LabelTile: tl.ID, tile.LabelRole: "pause"}},
		docker.Container{ID: "r" + slug, State: "running", Labels: map[string]string{tile.LabelTile: tl.ID, tile.LabelRole: "replica"}})
	r.fake.Details["p"+slug] = docker.Detail{Running: true, Networks: map[string]string{"n": pauseIP}}
	r.fake.Details["r"+slug] = docker.Detail{Running: true, Networks: map[string]string{"n": replicaIP}}
}

// A slow Docker spends the boot context; the rebuild has its own, so a boot
// context that is already done must not fail it.
func TestRebuildVIPsOwnContext(t *testing.T) {
	r := newBootRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	must(t, r.orch.rebuildVIPs(ctx))
	if len(*r.scripts) != 1 || !strings.Contains((*r.scripts)[0], "-d 10.0.0.2/32") {
		t.Fatalf("rebuild under a done boot ctx applied %v", *r.scripts)
	}
}

// A tile skipped for a transient read error must not lose its kernel rules:
// the table keeps its previous entries, nothing is removed.
func TestRebuildVIPsTransientErrorKeepsRules(t *testing.T) {
	ctx := context.Background()
	r := newBootRig(t)
	must(t, r.orch.rebuildVIPs(ctx))
	*r.scripts = nil
	r.fake.Err["Inspect"] = errors.New("docker busy")
	_ = r.orch.rebuildVIPs(ctx)
	for _, s := range *r.scripts {
		if strings.Contains(s, "-X ") || !strings.Contains(s, "-d 10.0.0.2/32") || !strings.Contains(s, "-d 10.0.0.4/32") {
			t.Fatalf("rebuild after a transient error dropped rules:\n%s", s)
		}
	}
}

// One tile whose pause container has no address on its env network is skipped
// with its previous rules kept; the other tiles are still rebuilt.
func TestRebuildVIPsUnaddressedPauseSkipsOnlyThatTile(t *testing.T) {
	ctx := context.Background()
	r := newBootRig(t)
	must(t, r.orch.rebuildVIPs(ctx))
	*r.scripts = nil
	r.fake.Details["pa"] = docker.Detail{Running: true, Networks: map[string]string{}}
	r.fake.Details["rb"] = docker.Detail{Running: true, Networks: map[string]string{"n": "10.0.0.7"}}
	_ = r.orch.rebuildVIPs(ctx)
	if len(*r.scripts) != 1 {
		t.Fatalf("restore calls = %d, want 1: %v", len(*r.scripts), *r.scripts)
	}
	s := (*r.scripts)[0]
	for _, want := range []string{"-d 10.0.0.2/32", "--to-destination 10.0.0.3", "-d 10.0.0.4/32", "--to-destination 10.0.0.7"} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q (a kept or a rebuilt rule):\n%s", want, s)
		}
	}
	if strings.Contains(s, "-X ") {
		t.Errorf("script removed a chain:\n%s", s)
	}
}

// One tile with a non-IPv4 address must not empty the table for the rest.
func TestRebuildVIPsBadAddressSkipsOnlyThatTile(t *testing.T) {
	ctx := context.Background()
	r := newBootRig(t)
	r.addTile(t, "v6", "fd00::2", "fd00::3")
	must(t, r.orch.rebuildVIPs(ctx))
	if len(*r.scripts) != 1 {
		t.Fatalf("restore calls = %d, want 1 (the v6 tile must not fail the rebuild)", len(*r.scripts))
	}
	for _, want := range []string{"-d 10.0.0.2/32", "-d 10.0.0.4/32"} {
		if !strings.Contains((*r.scripts)[0], want) {
			t.Errorf("script lacks %q:\n%s", want, (*r.scripts)[0])
		}
	}
}

// A pause container whose tile row is gone is skipped; the others still apply.
func TestRebuildVIPsPauseWithoutTileRow(t *testing.T) {
	ctx := context.Background()
	r := newBootRig(t)
	r.fake.Containers = append(r.fake.Containers,
		docker.Container{ID: "pz", State: "running", Labels: map[string]string{tile.LabelTile: uuid.NewString(), tile.LabelRole: "pause"}})
	r.fake.Details["pz"] = docker.Detail{Running: true, Networks: map[string]string{"n": "10.0.0.9"}}
	must(t, r.orch.rebuildVIPs(ctx))
	if len(*r.scripts) != 1 || !strings.Contains((*r.scripts)[0], "-d 10.0.0.4/32") {
		t.Fatalf("rebuild with an orphan pause applied %v", *r.scripts)
	}
}

// After a host reboot the boot rebuild may run before containers are up;
// the minute tick re-routes from Docker once they are.
func TestWatchTickRebuildsVIPs(t *testing.T) {
	r := newBootRig(t)
	must(t, r.orch.watchTick(context.Background()))
	if len(*r.scripts) == 0 || !strings.Contains((*r.scripts)[len(*r.scripts)-1], "-d 10.0.0.4/32") {
		t.Fatalf("watchTick did not rebuild the VIP table: %v", *r.scripts)
	}
}

// Docker dials a loopback resolver from the host, so only the others need a
// firewall exception.
func TestResolvers(t *testing.T) {
	conf := "# x\nnameserver 127.0.0.53\nnameserver 192.168.1.1\nnameserver fe80::1\nsearch lan\nnameserver 1.1.1.1\n"
	if got := resolvers(conf); len(got) != 2 || got[0] != "192.168.1.1" || got[1] != "1.1.1.1" {
		t.Fatalf("got %v", got)
	}
}

// lanRig gives tile a a lan ask and tile b another; only what the stack's
// grant holds may reach the VIP table.
func lanRig(t *testing.T) bootRig {
	t.Helper()
	r := newBootRig(t)
	for slug, lan := range map[string]string{"a": "192.168.1.50:445", "b": "192.168.1.60:80"} {
		tl := r.tiles[slug]
		tl.Lan = lan
		must(t, r.st.Tiles.Update(context.Background(), tl))
		r.tiles[slug] = tl
	}
	now := time.Now()
	must(t, r.st.Users.Create(context.Background(), store.User{ID: "adm", Email: "a@b.c", Name: "A", Role: "admin", Active: true, CreatedAt: now, UpdatedAt: now}))
	return r
}

func lastScript(r bootRig) string { return (*r.scripts)[len(*r.scripts)-1] }

// A granted lan line reaches the rules of that tile's replica only; an
// ungranted one never does; revoke rebuilds without it at once.
func TestLanGrantReachesRulesAndRevokeCutsIt(t *testing.T) {
	ctx := context.Background()
	r := lanRig(t)
	grant := hostgrant.Set{Lines: []string{"a lan:192.168.1.50:445"}}
	if _, err := r.orch.hostgrant.Approve(ctx, r.stack, "adm", grant); err != nil {
		t.Fatal(err)
	}
	must(t, r.orch.rebuildVIPs(ctx))
	s := lastScript(r)
	if !strings.Contains(s, "-s 10.0.0.3/32 -d 192.168.1.50") {
		t.Fatalf("granted lan line missing for a's replica:\n%s", s)
	}
	if strings.Contains(s, "192.168.1.60") || strings.Contains(s, "-s 10.0.0.5/32 -d 192.168.1") {
		t.Fatalf("ungranted lan line reached the rules:\n%s", s)
	}
	*r.scripts = nil
	must(t, r.orch.RevokeHostGrant(ctx, r.stack, "a"))
	r.orch.vipAsync.Wait()
	if len(*r.scripts) != 1 || strings.Contains(lastScript(r), "192.168.1.50") {
		t.Fatalf("revoke did not rebuild without the lan rule: %v", *r.scripts)
	}
}

// Approve applies the lan rules at once, without a redeploy.
func TestApproveHostGrantRebuildsRules(t *testing.T) {
	ctx := context.Background()
	r := lanRig(t)
	must(t, r.orch.rebuildVIPs(ctx))
	*r.scripts = nil
	if _, err := r.orch.hostgrant.Approve(ctx, r.stack, "adm", hostgrant.Set{Lines: []string{"a lan:192.168.1.50:445"}}); err != nil {
		t.Fatal(err)
	}
	r.orch.rerouteVIPs(ctx) // what ApproveHostGrant calls after it grants
	if !strings.Contains(lastScript(r), "-s 10.0.0.3/32 -d 192.168.1.50") {
		t.Fatalf("approval not applied:\n%s", lastScript(r))
	}
}

// The minute tick applies the iptables script once: VIPs and filter base together.
func TestWatchTickAppliesOnce(t *testing.T) {
	r := newBootRig(t)
	must(t, r.orch.watchTick(context.Background()))
	if len(*r.scripts) != 1 {
		t.Fatalf("watchTick applied %d scripts, want 1", len(*r.scripts))
	}
}

// On a systemd-resolved host both files are read: the stub is loopback and
// skipped, the real upstream sits in the second.
func TestVipBaseReadsBothResolvConfs(t *testing.T) {
	dir := t.TempDir()
	a, b := dir+"/a", dir+"/b"
	must(t, os.WriteFile(a, []byte("nameserver 127.0.0.53\nnameserver 192.168.1.1\n"), 0o600))
	must(t, os.WriteFile(b, []byte("nameserver 192.168.1.1\nnameserver 9.9.9.9\n"), 0o600))
	old := resolvConfs
	resolvConfs = []string{a, b, dir + "/missing"}
	t.Cleanup(func() { resolvConfs = old })
	r := newBootRig(t)
	got := r.orch.vipBase(context.Background()).Resolvers
	if !slices.Equal(got, []string{"192.168.1.1", "9.9.9.9"}) {
		t.Fatalf("got %v", got)
	}
}

// The cloudflare keyword reaches the firewall as its IPv4 ranges only.
func TestVipBaseExpandsCloudflare(t *testing.T) {
	r := newBootRig(t)
	must(t, r.orch.SetSetting(context.Background(), "trusted_proxies", "192.168.1.100,cloudflare"))
	got := r.orch.vipBase(context.Background()).Front
	if len(got) != 1+len(domain.CloudflareV4) || got[0] != "192.168.1.100" || !slices.Contains(got, "173.245.48.0/20") {
		t.Fatalf("front = %v", got)
	}
}

// A partial rebuild (one tile unreadable) still drops a deleted tile's VIP
// and the VIP of a tile with no replicas, and keeps the unreadable tile's.
func TestRebuildVIPsPartialDropsStale(t *testing.T) {
	ctx := context.Background()
	r := newBootRig(t)
	r.addTile(t, "c", "10.0.0.6", "10.0.0.7")
	r.addTile(t, "d", "10.0.0.8", "10.0.0.9")
	must(t, r.orch.rebuildVIPs(ctx))
	*r.scripts = nil
	must(t, r.st.Tiles.Delete(ctx, r.tiles["c"].ID))                                                   // c: row gone, pause left
	r.fake.Details["rd"] = docker.Detail{Running: false, Networks: map[string]string{"n": "10.0.0.9"}} // d: no replicas
	r.fake.Details["pa"] = docker.Detail{Running: true, Networks: map[string]string{}}                 // a: unreadable
	_ = r.orch.rebuildVIPs(ctx)
	s := lastScript(r)
	if !strings.Contains(s, "-A STACKR-VIP -d 10.0.0.2/32") || !strings.Contains(s, "-A STACKR-VIP -d 10.0.0.4/32") {
		t.Errorf("kept or healthy VIPs missing:\n%s", s)
	}
	for _, gone := range []string{"10.0.0.6", "10.0.0.8"} {
		if strings.Contains(s, "-A STACKR-VIP -d "+gone+"/32") {
			t.Errorf("stale VIP %s survived a partial rebuild:\n%s", gone, s)
		}
	}
}

// The tick and the async revoke rebuild one at a time.
func TestRebuildVIPsSerialised(t *testing.T) {
	r := newBootRig(t)
	r.orch.vipMu.Lock()
	done := make(chan error)
	go func() { done <- r.orch.rebuildVIPs(context.Background()) }()
	select {
	case <-done:
		t.Fatal("rebuild ran while another held the lock")
	case <-time.After(50 * time.Millisecond):
	}
	r.orch.vipMu.Unlock()
	must(t, <-done)
}

// A starting replica gets its tile's granted lan rules before it is routed,
// and loses them when it goes.
func TestEarlyLanRules(t *testing.T) {
	ctx := context.Background()
	r := lanRig(t)
	if _, err := r.orch.hostgrant.Approve(ctx, r.stack, "adm", hostgrant.Set{Lines: []string{"a lan:192.168.1.50:445"}}); err != nil {
		t.Fatal(err)
	}
	must(t, r.orch.rebuildVIPs(ctx))
	r.fake.Details["rnew"] = docker.Detail{Running: true, Networks: map[string]string{"n": "10.0.0.20"}}
	r.orch.earlyLan(ctx, r.tiles["a"], "rnew", true)
	if !strings.Contains(lastScript(r), "-s 10.0.0.20/32 -d 192.168.1.50") {
		t.Fatalf("early replica has no lan rule:\n%s", lastScript(r))
	}
	r.orch.earlyLan(ctx, r.tiles["a"], "rnew", false)
	if strings.Contains(lastScript(r), "10.0.0.20") {
		t.Fatalf("early rule outlived the replica:\n%s", lastScript(r))
	}
}

// The panel lock uses the port the panel listens on.
func TestVipBasePanelPort(t *testing.T) {
	r := newBootRig(t)
	r.orch.cfg.PanelPort = 9191
	if got := r.orch.vipBase(context.Background()).PanelPort; got != 9191 {
		t.Fatalf("panel port %d", got)
	}
}

// A run container loses its early rule on exit even though Docker has
// cleared its address by then.
func TestEarlyLanClearedAfterStop(t *testing.T) {
	ctx := context.Background()
	r := lanRig(t)
	if _, err := r.orch.hostgrant.Approve(ctx, r.stack, "adm", hostgrant.Set{Lines: []string{"a lan:192.168.1.50:445"}}); err != nil {
		t.Fatal(err)
	}
	must(t, r.orch.rebuildVIPs(ctx))
	r.fake.Details["rnew"] = docker.Detail{Running: true, Networks: map[string]string{"n": "10.0.0.20"}}
	r.orch.earlyLan(ctx, r.tiles["a"], "rnew", true)
	must(t, r.fake.StopRemove(ctx, "rnew"))
	r.orch.earlyLan(ctx, r.tiles["a"], "rnew", false)
	must(t, r.orch.rebuildVIPs(ctx))
	if strings.Contains(lastScript(r), "10.0.0.20") {
		t.Fatalf("early rule outlived the stopped run:\n%s", lastScript(r))
	}
}

// A revoke while the container still runs cuts its rule at the next rebuild;
// a crashed container's entry is dropped there too.
func TestEarlyLanRevokeAndCrashAtRebuild(t *testing.T) {
	ctx := context.Background()
	r := lanRig(t)
	if _, err := r.orch.hostgrant.Approve(ctx, r.stack, "adm", hostgrant.Set{Lines: []string{"a lan:192.168.1.50:445"}}); err != nil {
		t.Fatal(err)
	}
	r.fake.Details["rnew"] = docker.Detail{Running: true, Networks: map[string]string{"n": "10.0.0.20"}}
	r.fake.Details["rdead"] = docker.Detail{Running: true, Networks: map[string]string{"n": "10.0.0.21"}}
	r.orch.earlyLan(ctx, r.tiles["a"], "rnew", true)
	r.orch.earlyLan(ctx, r.tiles["a"], "rdead", true)
	must(t, r.orch.rebuildVIPs(ctx))
	s := lastScript(r)
	if !strings.Contains(s, "-s 10.0.0.20/32 -d 192.168.1.50") || !strings.Contains(s, "-s 10.0.0.21/32 -d 192.168.1.50") {
		t.Fatalf("early rules missing:\n%s", s)
	}
	must(t, r.fake.Stop(ctx, "rdead")) // crashed, no gate-failure callback
	must(t, r.orch.RevokeHostGrant(ctx, r.stack, "a"))
	r.orch.vipAsync.Wait()
	if s := lastScript(r); strings.Contains(s, "10.0.0.20") || strings.Contains(s, "10.0.0.21") {
		t.Fatalf("early rules survived revoke or crash:\n%s", s)
	}
}

// A recreated proxy starts on no ingress network: the tick that sees its new
// start time joins it to each domain tile's network before the push.
func TestWatchTickRejoinsIngressOnProxyRestart(t *testing.T) {
	ctx := context.Background()
	r := newBootRig(t)
	tl := r.tiles["a"]
	must(t, r.st.Domains.Create(ctx, domainRow(tl.ID, "a.acme.io", nil)))
	r.fake.Details[ProxyContainer] = docker.Detail{Running: true, Started: "t1"}
	must(t, r.orch.watchTick(ctx)) // first sight counts as a start
	r.fake.Details[ProxyContainer] = docker.Detail{Running: true, Started: "t2"}
	n := len(r.fake.Calls())
	must(t, r.orch.watchTick(ctx))
	want := "Connect(" + domain.Ingress(tl.ID) + ", " + ProxyContainer + ", )"
	if !slices.ContainsFunc(r.fake.Calls()[n:], func(c dockerfake.Call) bool { return c.String() == want }) {
		t.Fatalf("proxy not rejoined: %v", r.fake.Calls()[n:])
	}
	n = len(r.fake.Calls())
	must(t, r.orch.watchTick(ctx)) // same start time: no rejoin
	for _, c := range r.fake.Calls()[n:] {
		if c.Method == "Connect" {
			t.Fatalf("rejoined without a restart: %v", c)
		}
	}
}

// inspectHook runs fn on every Inspect, to land another call in the gap
// between a read and the Allow that follows it.
type inspectHook struct {
	Docker
	fn func(id string)
}

func (h inspectHook) Inspect(ctx context.Context, id string) (docker.Detail, error) {
	d, err := h.Docker.Inspect(ctx, id)
	if h.fn != nil {
		h.fn(id)
	}
	return d, err
}

func earlyRig(t *testing.T) bootRig {
	t.Helper()
	r := lanRig(t)
	if _, err := r.orch.hostgrant.Approve(context.Background(), r.stack, "adm", hostgrant.Set{Lines: []string{"a lan:192.168.1.50:445"}}); err != nil {
		t.Fatal(err)
	}
	must(t, r.orch.rebuildVIPs(context.Background()))
	r.fake.Details["rnew"] = docker.Detail{Running: true, Networks: map[string]string{"n": "10.0.0.20"}}
	return r
}

// The off path lands between the sweep's snapshot and its Allow: the sweep
// must not put the cleared rule back.
func TestSweepEarlyOffPathInterleaved(t *testing.T) {
	ctx := context.Background()
	r := earlyRig(t)
	r.orch.earlyLan(ctx, r.tiles["a"], "rnew", true)
	h := inspectHook{Docker: r.orch.docker}
	h.fn = func(id string) {
		if id == "rnew" {
			h.fn = nil
			r.orch.earlyLan(ctx, r.tiles["a"], "rnew", false)
		}
	}
	r.orch.docker = &h
	r.orch.sweepEarly(ctx)
	if strings.Contains(lastScript(r), "10.0.0.20") {
		t.Fatalf("sweep re-added a cleared early rule:\n%s", lastScript(r))
	}
	if len(r.orch.earlyIPs) != 0 {
		t.Fatalf("early entries left: %v", r.orch.earlyIPs)
	}
}

// A revoke lands after the on path's first reads: the rule must not be added.
func TestEarlyLanOnPathRevokedMeanwhile(t *testing.T) {
	ctx := context.Background()
	r := earlyRig(t)
	h := inspectHook{Docker: r.orch.docker}
	h.fn = func(string) { must(t, r.orch.hostgrant.Revoke(ctx, r.stack, "a")) }
	r.orch.docker = &h
	r.orch.earlyLan(ctx, r.tiles["a"], "rnew", true)
	if strings.Contains(lastScript(r), "10.0.0.20") || len(r.orch.earlyIPs) != 0 {
		t.Fatalf("stale early rule added after revoke:\n%s", lastScript(r))
	}
}

// Once a VIP routes the replica its early entry is forgotten.
func TestEarlyEntryDroppedWhenRouted(t *testing.T) {
	ctx := context.Background()
	r := earlyRig(t)
	r.fake.Details["rnew"] = docker.Detail{Running: true, Networks: map[string]string{"n": "10.0.0.3"}} // a's routed replica
	r.orch.earlyLan(ctx, r.tiles["a"], "rnew", true)
	if len(r.orch.earlyIPs) != 1 {
		t.Fatalf("early entry missing: %v", r.orch.earlyIPs)
	}
	must(t, r.orch.rebuildVIPs(ctx))
	if len(r.orch.earlyIPs) != 0 {
		t.Fatalf("routed replica still tracked: %v", r.orch.earlyIPs)
	}
}
