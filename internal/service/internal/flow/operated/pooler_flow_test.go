package operated_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
)

// The first deploy makes the pooler (its role and lookup function first,
// then the container); a later one finds it and only reloads its config.
func TestEnsurePoolerMakesThenReloads(t *testing.T) {
	w := setup(t, "postgres")
	w.fake.RunID = "pool1"
	w.fake.Details = map[string]docker.Detail{"pool1": {Running: true, State: "running"}}
	w.f.Tiles.Gate = tile.Gate{Poll: time.Millisecond, Grace: time.Millisecond, Deadline: time.Second}
	roleThere := false
	w.fake.ExecFn = func(cmd []string) string {
		c := strings.Join(cmd, " ")
		switch {
		case strings.Contains(c, "get_auth('$2')") || strings.Contains(c, "get_auth('stackr_pooler_probe')"):
			return "1,0,0" // the probe is found, the admin and the pooler role are not
		case strings.Contains(c, "FROM pg_roles") && roleThere:
			return "1"
		}
		return "0"
	}
	var pulled []string
	pull := func(_ context.Context, ref string) error { pulled = append(pulled, ref); return nil }
	w.f.Pull = pull
	must(t, w.f.EnsurePooler(ctx, w.db, io.Discard))

	ex := rawExecs(w.fake, 0)
	has(t, "auth", ex, "CREATE ROLE stackr_pooler LOGIN PASSWORD", "CREATE OR REPLACE FUNCTION stackr_pooler.get_auth", "GRANT EXECUTE ON FUNCTION stackr_pooler.get_auth")
	if len(pulled) != 1 || pulled[0] != mflow.PoolerImage {
		t.Errorf("pulled %v", pulled)
	}
	if len(w.fake.Specs) != 1 {
		t.Fatalf("%d containers made", len(w.fake.Specs))
	}
	s := w.fake.Specs[0]
	if s.Labels[tile.LabelTile] != w.db.ID || s.Labels[tile.LabelRole] != mflow.PoolerRole {
		t.Errorf("labels = %v", s.Labels)
	}
	if s.User != "pgbouncer" || s.Restart != "unless-stopped" || len(s.Networks) != 1 || !strings.HasSuffix(s.Networks[0].Aliases[0], "-pooler") {
		t.Errorf("spec = %+v", s)
	}
	m, err := w.f.Instances.Get(ctx, w.instance.ID)
	must(t, err)
	if m.PoolerPassword == "" {
		t.Fatalf("the pooler password was not stored")
	}
	for _, c := range w.fake.Calls() {
		if strings.Contains(strings.Join(c.Args, " "), m.PoolerPassword) {
			t.Errorf("the pooler password is on an argv: %s", c.Method)
		}
	}
	for _, e := range s.Env {
		if strings.HasPrefix(e, "STACKR_PGB_INI=") && strings.Contains(e, m.PoolerPassword) {
			t.Errorf("the ini carries the password")
		}
	}

	// the pooler is there now: a second ensure reloads, never a second container
	w.fake.Containers = append(w.fake.Containers, docker.Container{
		ID: "pool1", State: "running",
		Labels: map[string]string{tile.LabelTile: w.db.ID, tile.LabelRole: mflow.PoolerRole},
	})
	roleThere = true
	n := len(w.fake.Calls())
	must(t, w.f.EnsurePooler(ctx, w.db, io.Discard))
	if len(w.fake.Specs) != 1 {
		t.Errorf("a second pooler was made")
	}
	has(t, "reload", rawExecs(w.fake, n), "ALTER ROLE stackr_pooler LOGIN PASSWORD", "kill -HUP 1")

	// a pooler never counts as a replica of the instance
	st, err := w.f.Tiles.State(ctx, w.db)
	must(t, err)
	if len(st.Replicas) != 1 || st.Replicas[0].ID != "c1" {
		t.Errorf("replicas = %v", st.Replicas)
	}

	// SyncPooler (a slice came) reloads the running one too
	n = len(w.fake.Calls())
	must(t, w.f.SyncPooler(ctx, w.db))
	has(t, "sync", rawExecs(w.fake, n), "kill -HUP 1")
}

// A lookup that does not answer is an error, not a silent "bouncer config error".
func TestEnsurePoolerFailsWithoutLookup(t *testing.T) {
	w := setup(t, "postgres")
	w.fake.ExecFn = func([]string) string { return "0" }
	w.f.Pull = func(context.Context, string) error { return nil }
	err := w.f.EnsurePooler(ctx, w.db, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "auth lookup") {
		t.Errorf("err = %v", err)
	}
	if len(w.fake.Specs) != 0 {
		t.Errorf("a pooler was started on a broken lookup")
	}
}

// S3 has no pooler.
func TestEnsurePoolerSkipsS3(t *testing.T) {
	w := setup(t, "s3")
	w.f.Pull = func(context.Context, string) error { return nil }
	must(t, w.f.EnsurePooler(ctx, w.db, io.Discard))
	if len(w.fake.Specs) != 0 {
		t.Errorf("s3 got a pooler")
	}
}

// has is contains for output that carries a password: a miss never prints it.
func has(t *testing.T, what, got string, want ...string) {
	t.Helper()
	for _, s := range want {
		if !strings.Contains(got, s) {
			t.Errorf("%s: no %q in the execs", what, s)
		}
	}
}

// pgFake answers the engine's queries: the lookup proof, "nothing exists
// yet", and max_connections as postgres runs it (live, "" = not asked).
func pgFake(w *world, live string) {
	w.fake.RunID = "pool1"
	w.fake.Details = map[string]docker.Detail{"pool1": {Running: true, State: "running"}}
	w.f.Tiles.Gate = tile.Gate{Poll: time.Millisecond, Grace: time.Millisecond, Deadline: time.Second}
	w.f.Pull = func(context.Context, string) error { return nil }
	w.fake.ExecFn = func(cmd []string) string {
		c := strings.Join(cmd, " ")
		switch {
		case strings.Contains(c, "get_auth("):
			return "1,0,0"
		case strings.Contains(c, "FROM pg_settings WHERE name IN") && live != "":
			return "max_connections|0|" + live + "|postmaster"
		case strings.Contains(c, "bool_or(pending_restart)"):
			return "0"
		case strings.Contains(c, "|| ','"):
			return "0,0"
		}
		return "0"
	}
}

func poolerIniOf(w *world) string {
	for _, c := range w.fake.Calls() {
		if c.Method == "Exec" && strings.Contains(strings.Join(c.Args, " "), "kill -HUP 1") {
			return strings.Join(c.Args, " ")
		}
	}
	return ""
}

// A new slice makes the pooler when there is none (the pooled outputs it
// publishes must name something), and lists the database in it.
func TestProvisionMakesThePooler(t *testing.T) {
	w := setup(t, "postgres")
	pgFake(w, "")
	if _, err := w.f.Provision(ctx, w.db, w.orders); err != nil {
		t.Fatal(err)
	}
	if len(w.fake.Specs) != 1 || w.fake.Specs[0].Labels[tile.LabelRole] != mflow.PoolerRole {
		t.Fatalf("specs = %d", len(w.fake.Specs))
	}
	var ini string
	for _, e := range w.fake.Specs[0].Env {
		if strings.HasPrefix(e, "STACKR_PGB_INI=") {
			ini = e
		}
	}
	if !strings.Contains(ini, "s_dev_orders = host=") {
		t.Errorf("the new database is not in the pooler's config")
	}
}

// A pooler that cannot be made fails the slice's deploy, so a retry makes it.
func TestProvisionFailsWithThePooler(t *testing.T) {
	w := setup(t, "postgres")
	pgFake(w, "")
	w.f.Pull = func(context.Context, string) error { return errors.New("registry down") }
	if _, err := w.f.Provision(ctx, w.db, w.orders); err == nil {
		t.Fatal("no error")
	}
	w.f.Pull = func(context.Context, string) error { return nil }
	if _, err := w.f.Provision(ctx, w.db, w.orders); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(w.fake.Specs) != 1 {
		t.Errorf("the retry made %d poolers", len(w.fake.Specs))
	}
}

// A slice the budget cannot hold is refused, before anything is created.
func TestProvisionRefusedOverBudget(t *testing.T) {
	w := setup(t, "postgres")
	pgFake(w, "")
	m, err := w.f.Instances.SetSettings(ctx, w.instance, map[string]string{"max_connections": "12"})
	must(t, err)
	_ = m
	n := len(w.fake.Calls())
	_, err = w.f.Provision(ctx, w.db, w.orders)
	if err == nil || !strings.Contains(err.Error(), "max_connections") {
		t.Fatalf("err = %v", err)
	}
	if ex := rawExecs(w.fake, n); strings.Contains(ex, "CREATE ROLE") || strings.Contains(ex, "CREATE DATABASE") {
		t.Errorf("the slice was created")
	}
	// the settings check says the same
	if err := w.f.CheckPoolBudget(ctx, w.db, map[string]string{"max_connections": "100"}, 1); err != nil {
		t.Errorf("100 refused: %v", err)
	}
	if err := w.f.CheckPoolBudget(ctx, w.db, map[string]string{"max_connections": "12"}, 1); err == nil {
		t.Errorf("12 accepted for one database")
	}
}

// The pool caps follow the max_connections postgres runs with: a stored
// change waiting for its restart does not move them.
func TestPoolerFollowsLiveMaxConnections(t *testing.T) {
	w := setup(t, "postgres")
	pgFake(w, "100")
	_, err := w.f.Instances.SetSettings(ctx, w.instance, map[string]string{"max_connections": "40"})
	must(t, err)
	must(t, w.f.EnsurePooler(ctx, w.db, io.Discard))
	var ini string
	for _, e := range w.fake.Specs[0].Env {
		if strings.HasPrefix(e, "STACKR_PGB_INI=") {
			ini = e
		}
	}
	if !strings.Contains(ini, "postgres = host=") || !strings.Contains(ini, "max_db_connections=2") {
		t.Fatalf("ini: %s", ini)
	}
	pgFake(w, "40")
	w.fake.Containers = append(w.fake.Containers, docker.Container{
		ID: "pool1", State: "running",
		Labels: map[string]string{tile.LabelTile: w.db.ID, tile.LabelRole: mflow.PoolerRole},
	})
	_, err = w.f.Provision(ctx, w.db, w.orders)
	must(t, err)
	if r := poolerIniOf(w); !strings.Contains(r, "s_dev_orders = host=db-") || !strings.Contains(r, "max_db_connections=28") {
		t.Errorf("reload: %s", r)
	}
}

// A stopped pooler is started and waited for before the reload is sent.
func TestPoolerReloadWaitsForStart(t *testing.T) {
	w := setup(t, "postgres")
	pgFake(w, "")
	w.fake.Containers = append(w.fake.Containers, docker.Container{
		ID: "pool1", State: "running", Health: "healthy",
		Labels: map[string]string{tile.LabelTile: w.db.ID, tile.LabelRole: mflow.PoolerRole},
	})
	w.fake.Containers[1].State = "exited"
	must(t, w.f.SyncPooler(ctx, w.db))
	start, hup := -1, -1
	for i, c := range w.fake.Calls() {
		if c.Method == "Start" && start < 0 {
			start = i
		}
		if c.Method == "Exec" && strings.Contains(strings.Join(c.Args, " "), "kill -HUP 1") {
			hup = i
		}
	}
	if start < 0 || hup < 0 || start > hup {
		t.Fatalf("start at %d, reload at %d", start, hup)
	}
}

// Jobs for one instance's pooler never overlap, so the last write holds
// every database.
func TestPoolerJobsAreSerialised(t *testing.T) {
	w := setup(t, "postgres")
	pgFake(w, "")
	base := w.fake.ExecFn
	var in, worst atomic.Int32
	w.fake.ExecFn = func(cmd []string) string {
		if strings.Contains(strings.Join(cmd, " "), "FROM pg_roles WHERE rolname = 'stackr_pooler')") {
			n := in.Add(1)
			if n > worst.Load() {
				worst.Store(n)
			}
			time.Sleep(5 * time.Millisecond)
			defer in.Add(-1)
		}
		return base(cmd)
	}
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() { defer wg.Done(); _ = w.f.SyncPooler(ctx, w.db) }()
	}
	wg.Wait()
	if worst.Load() != 1 {
		t.Errorf("%d pooler jobs ran at once", worst.Load())
	}
}

// rawExecs is every exec from call n on, the pooler's included.
func rawExecs(f *dockerfake.Fake, n int) string {
	var b strings.Builder
	for _, c := range f.Calls()[n:] {
		if c.Method == "Exec" || c.Method == "ExecStream" {
			b.WriteString(strings.Join(c.Args, " ") + "\n" + c.Stdin)
		}
	}
	return b.String()
}
