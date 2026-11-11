package operated

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// The socket settings are owned: a user cannot lock the admin out; every
// admin client names the socket directory itself.
func TestSocketIsOwnedAndNamed(t *testing.T) {
	for _, n := range []string{"unix_socket_directories", "unix_socket_permissions", "unix_socket_group"} {
		if err := CheckSettings(map[string]string{n: "x"}); err == nil || !strings.Contains(err.Error(), "managed by stackr") {
			t.Errorf("%s: %v", n, err)
		}
	}
	i := Instance{AdminUser: "stackr", AdminDB: "postgres", PITRDays: 7}
	s := Slice{Name: "orders", User: "orders"}
	for name, a := range map[string][]string{
		"psql": pg{}.psql(i, "postgres", nil), "dump": pg{}.Backup("dump", i), "base": pg{}.Backup("base", i),
		"restore": pg{}.Restore("dump", "postgres", i), "slicedump": pg{}.SliceDump(i, s),
		"seed custom": pg{}.Seed(DumpCustom, i, s), "seed plain": pg{}.Seed(DumpPlain, i, s),
	} {
		if j := strings.Join(a, " "); !strings.Contains(j, "-h "+pgSocketDir) {
			t.Errorf("%s does not name the socket: %s", name, j)
		}
	}
}

// The layout follows the stored major; an image of another one, or none,
// is refused, never guessed.
func TestMajorIsStoredNotGuessed(t *testing.T) {
	d := pg{}.Definition()
	tl := store.Tile{Slug: "db"}
	ok := store.OperatedInstance{Major: 17}
	if err := checkMajor(d, ok, tl); err != nil {
		t.Errorf("default image on 17: %v", err)
	}
	tl.ImageRef = "postgres:18"
	if err := checkMajor(d, ok, tl); err == nil || !strings.Contains(err.Error(), "stackr operated upgrade") {
		t.Errorf("17 -> 18: %v", err)
	}
	tl.ImageRef = "postgres@sha256:abc"
	if checkMajor(d, ok, tl) == nil {
		t.Error("a digest passed")
	}
	if checkMajor(d, store.OperatedInstance{}, store.Tile{}) == nil {
		t.Error("an instance with no recorded major passed")
	}
	if checkMajor(s3e{}.Definition(), store.OperatedInstance{}, store.Tile{}) != nil {
		t.Error("s3 has no major")
	}
}

// A failure after the file is written puts the old file back and reloads.
func TestApplyUndoesOnFailure(t *testing.T) {
	i := Instance{AdminUser: "stackr", AdminDB: "postgres", Major: 17, Settings: map[string]string{"work_mem": "8MB"}}
	var calls []string
	x := Tools{Exec: func(_ context.Context, a []string) (string, error) {
		j := strings.Join(a, " ")
		calls = append(calls, j)
		switch {
		case strings.Contains(j, "FROM pg_settings"):
			return "work_mem|user", nil
		case strings.Contains(j, `cat "$0"`):
			return "OLDCONF", nil
		case strings.Contains(j, "pg_reload_conf") &&
			!slices.ContainsFunc(calls[:len(calls)-1], func(c string) bool { return strings.Contains(c, "pg_reload_conf") }):
			return "", errors.New("reload failed")
		}
		return "", nil
	}}
	if _, err := pgApply(context.Background(), i, x, []string{"work_mem"}, nil); err == nil {
		t.Fatal("a failed reload was not an error")
	}
	var restored bool
	for _, c := range calls {
		restored = restored || (strings.Contains(c, `"$d/$1.new"`) && strings.Contains(c, "stackr.conf OLDCONF"))
	}
	if !restored || !strings.Contains(calls[len(calls)-1], "pg_reload_conf") {
		t.Errorf("the old file was not put back and reloaded:\n%s", strings.Join(calls, "\n"))
	}
}
