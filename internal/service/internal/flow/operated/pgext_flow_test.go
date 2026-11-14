package operated_test

import (
	"io"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/errs"
)

// sliceWith lays a list on the slice tile and the instance.
func sliceWith(t *testing.T, w *world, instList, sliceList []string) {
	t.Helper()
	cur, err := w.f.Tiles.Get(ctx, w.orders.ID)
	must(t, err)
	next := cur
	next.Extensions = sliceList
	_, _, err = w.f.Tiles.Update(ctx, cur, next)
	must(t, err)
	w.instance, err = w.f.Instances.SetExtensions(ctx, w.instance, nil, instList)
	must(t, err)
	w.orders.Extensions = sliceList
}

// Messages 31 and 3: both are row-only checks ahead of any server work.
func TestProvisionRefusesExtensions(t *testing.T) {
	w := setup(t, "s3")
	sliceWith(t, w, nil, []string{"vector"})
	if _, err := w.f.Provision(ctx, w.db, w.orders); err == nil ||
		!strings.Contains(err.Error(), "slice orders: extensions apply to postgres slices only") {
		t.Errorf("s3: %v", err)
	}

	w = setup(t, "postgres")
	sliceWith(t, w, []string{"cube"}, []string{"vector", "cube", "earthdistance"})
	n := len(w.fake.Calls())
	_, err := w.f.Provision(ctx, w.db, w.orders)
	if _, ok := errs.IsConflict(err); !ok || err.Error() !=
		"slice orders: vector, earthdistance is not enabled on db. Add it to the instance's extensions first." {
		t.Errorf("postgres: %v", err)
	}
	if len(w.fake.Calls()) != n {
		t.Error("a refused provision touched the server")
	}

	sliceWith(t, w, []string{"vector", "cube", "earthdistance"}, []string{"vector"})
	if _, err := w.f.Provision(ctx, w.db, w.orders); err != nil {
		t.Errorf("listed: %v", err)
	}
}

// answers serves the catalog read and the database list.
func answers(w *world, rows string) {
	w.fake.ExecFn = func(cmd []string) string {
		j := strings.Join(cmd, " ")
		switch {
		case strings.Contains(j, "get_auth("):
			return "1,0,0"
		case strings.Contains(j, "FROM pg_database WHERE datallowconn"):
			return "postgres\nshop"
		case strings.Contains(j, "pg_available_extensions"):
			return rows
		}
		return w.fake.ExecOut
	}
}

const wantVector = "R|0\nE|plpgsql|1.0|1.0|\nE|vector||0.8.1|\n"

func TestReconcileSliceCreatesWhatItLists(t *testing.T) {
	w := setup(t, "postgres")
	sliceWith(t, w, []string{"vector"}, []string{"vector"})
	if err := w.f.ReconcileSlice(ctx, w.db, w.orders, io.Discard); err != nil {
		t.Fatalf("unprovisioned: %v", err)
	}
	p, err := w.f.Provision(ctx, w.db, w.orders)
	must(t, err)
	answers(w, wantVector)
	must(t, w.f.ReconcileSlice(ctx, w.db, w.orders, io.Discard))
	if got := string(w.fake.ExecIn); !strings.Contains(got, `CREATE EXTENSION IF NOT EXISTS "vector" CASCADE`) ||
		!strings.HasPrefix(got, "SELECT pg_advisory_lock(") {
		t.Errorf("session = %q", got)
	}
	// the read ran on the slice's database
	var onDB bool
	for _, c := range w.fake.Calls() {
		j := strings.Join(c.Args, " ")
		onDB = onDB || (strings.Contains(j, "pg_available_extensions") && strings.Contains(j, " -d "+p.DBName+" "))
	}
	if !onDB {
		t.Error("the catalog was not read on the slice's database")
	}
	// an image without it: message 4, nothing written
	w.fake.ExecIn = nil
	answers(w, "R|0\nE|plpgsql|1.0|1.0|\n")
	if err := w.f.ReconcileSlice(ctx, w.db, w.orders, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "the image of db has no extension vector.") {
		t.Errorf("missing: %v", err)
	}
	if len(w.fake.ExecIn) != 0 {
		t.Error("something was written")
	}
}

// Reconcile visits every database and gives a provision its slice's list.
func TestReconcileEveryDatabase(t *testing.T) {
	w := setup(t, "postgres")
	sliceWith(t, w, []string{"vector"}, []string{"vector"})
	p, err := w.f.Provision(ctx, w.db, w.orders)
	must(t, err)
	// the fake answers the list with the admin DB and "shop"; name the slice's
	// database in the list instead
	w.fake.ExecFn = func(cmd []string) string {
		j := strings.Join(cmd, " ")
		switch {
		case strings.Contains(j, "FROM pg_database WHERE datallowconn"):
			return "postgres\n" + p.DBName
		case strings.Contains(j, "pg_available_extensions"):
			return wantVector
		}
		return ""
	}
	must(t, w.f.Reconcile(ctx, w.db, io.Discard))
	var reads int
	for _, c := range w.fake.Calls() {
		if strings.Contains(strings.Join(c.Args, " "), "pg_available_extensions") {
			reads++
		}
	}
	if reads != 2 {
		t.Errorf("catalog reads = %d, want 2 (one per database)", reads)
	}
	if got := string(w.fake.ExecIn); strings.Count(got, "CREATE EXTENSION") != 1 {
		t.Errorf("session = %q: only the slice's database lists vector", got)
	}
	// an s3 instance has nothing to reconcile
	if s3 := setup(t, "s3"); s3.f.Reconcile(ctx, s3.db, io.Discard) != nil {
		t.Error("s3 was reconciled")
	}
}

// After a good load the slice's list is reconciled (seed and branch).
func TestSeedReconciles(t *testing.T) {
	w := setup(t, "postgres")
	sliceWith(t, w, []string{"vector"}, []string{"vector"})
	_, err := w.f.Provision(ctx, w.db, w.orders)
	must(t, err)
	answers(w, wantVector)
	must(t, w.f.Seed(ctx, w.orders, "custom", strings.NewReader("PGDMP"), io.Discard))
	// ExecIn holds the last stdin: the reconcile session, after the dump
	if got := string(w.fake.ExecIn); !strings.Contains(got, `CREATE EXTENSION IF NOT EXISTS "vector"`) {
		t.Errorf("last session = %q", got)
	}
}

// Extensions and SliceExtensions read the catalog for the drawers.
func TestExtensionReads(t *testing.T) {
	w := setup(t, "postgres")
	sliceWith(t, w, []string{"earthdistance"}, []string{"earthdistance"})
	p, err := w.f.Provision(ctx, w.db, w.orders)
	must(t, err)
	_, err = w.f.Instances.SetExtensions(ctx, w.instance, []string{"vchord"}, []string{"earthdistance"})
	must(t, err)
	rows := "R|0\nE|plpgsql|1.0|1.0|\nE|cube|1.5|1.5|earthdistance\nE|earthdistance|1.2|1.2|\nE|vchord||1.1.1|\n"
	w.fake.ExecFn = func(cmd []string) string {
		j := strings.Join(cmd, " ")
		switch {
		case strings.Contains(j, "FROM pg_database WHERE datallowconn"):
			return "postgres\n" + p.DBName
		case strings.Contains(j, "pg_available_extensions"):
			return rows
		}
		return ""
	}
	got, err := w.f.Extensions(ctx, w.db)
	must(t, err)
	// listed and startup first, then the rest the image offers, unmarked
	if len(got) != 4 || got[2].Name != "cube" || got[2].Listed || got[2].Startup || got[2].Available != "1.5" || got[3].Name != "plpgsql" ||
		got[0].Name != "earthdistance" || !got[0].Listed || got[0].Installed != "1.2" ||
		got[1].Name != "vchord" || !got[1].Startup || got[1].Listed || got[1].Available != "1.1.1" || got[1].Installed != "" {
		t.Errorf("instance = %+v", got)
	}
	sl, err := w.f.SliceExtensions(ctx, w.orders)
	must(t, err)
	if len(sl) != 2 || sl[0].Name != "earthdistance" || !sl[0].Listed ||
		sl[1].Name != "cube" || sl[1].Listed || sl[1].AddedWith != "earthdistance" || sl[1].Installed != "1.5" {
		t.Errorf("slice = %+v", sl)
	}
}
