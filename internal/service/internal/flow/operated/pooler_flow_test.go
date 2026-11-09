package operated_test

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
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
		case strings.Contains(c, "get_auth('$2')"):
			return "1" // the lookup answers
		case strings.Contains(c, "FROM pg_roles") && roleThere:
			return "1"
		}
		return "0"
	}
	var pulled []string
	pull := func(_ context.Context, ref string) error { pulled = append(pulled, ref); return nil }
	must(t, w.f.EnsurePooler(ctx, w.db, pull, io.Discard))

	ex := execs(w.fake, 0)
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
	must(t, w.f.EnsurePooler(ctx, w.db, pull, io.Discard))
	if len(w.fake.Specs) != 1 {
		t.Errorf("a second pooler was made")
	}
	has(t, "reload", execs(w.fake, n), "ALTER ROLE stackr_pooler LOGIN PASSWORD", "kill -HUP 1")

	// a pooler never counts as a replica of the instance
	st, err := w.f.Tiles.State(ctx, w.db)
	must(t, err)
	if len(st.Replicas) != 1 || st.Replicas[0].ID != "c1" {
		t.Errorf("replicas = %v", st.Replicas)
	}

	// SyncPooler (a slice came) reloads the running one too
	n = len(w.fake.Calls())
	must(t, w.f.SyncPooler(ctx, w.db))
	has(t, "sync", execs(w.fake, n), "kill -HUP 1")
}

// SyncPooler makes nothing: an instance with no pooler stays without one.
func TestSyncPoolerNeverCreates(t *testing.T) {
	w := setup(t, "postgres")
	must(t, w.f.SyncPooler(ctx, w.db))
	if len(w.fake.Specs) != 0 || len(w.fake.Calls()) > 3 {
		t.Errorf("calls = %v", w.fake.Calls())
	}
}

// A lookup that does not answer is an error, not a silent "bouncer config error".
func TestEnsurePoolerFailsWithoutLookup(t *testing.T) {
	w := setup(t, "postgres")
	w.fake.ExecFn = func([]string) string { return "0" }
	err := w.f.EnsurePooler(ctx, w.db, func(context.Context, string) error { return nil }, io.Discard)
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
	must(t, w.f.EnsurePooler(ctx, w.db, func(context.Context, string) error { return nil }, io.Discard))
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
