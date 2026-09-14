package envnet

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// The stack's home has no env segment; every env scope keeps its own.
func TestHomeNames(t *testing.T) {
	home := Scope{OrgSlug: "test-org", StackSlug: "stackr-test", EnvSlug: repo.HomeSlug}
	if got := home.namePrefix(); got != "stkr_test-org_stackr-test" {
		t.Fatalf("home name prefix: %s", got)
	}
	if got := home.ContainerName("sharedpg", ""); got != "stkr_test-org_stackr-test_sharedpg" {
		t.Fatalf("home container: %s", got)
	}
	env := Scope{OrgSlug: "test-org", StackSlug: "stackr-test", EnvSlug: "production"}
	if got := env.namePrefix(); got != "stkr_test-org_stackr-test_production" {
		t.Fatalf("env name prefix: %s", got)
	}
}

// Names are built from slugs and never parsed back; this is the one place
// that checks the separator keeps the segments apart, including when every
// slug carries a single dash of its own.
func TestNamesRoundTrip(t *testing.T) {
	sc := Scope{OrgSlug: "test-org", StackSlug: "stackr-test", EnvSlug: "pr-12"}
	tile := "orders-api"

	check := func(name, prefix string, want ...string) {
		t.Helper()
		if !strings.HasPrefix(name, prefix) {
			t.Fatalf("%s: prefix %q missing", name, prefix)
		}
		got := strings.Split(strings.TrimPrefix(name, prefix), sep)
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("%s: segments %v, want %v", name, got, want)
		}
	}
	check(sc.namePrefix(), Prefix, "test-org", "stackr-test", "pr-12")
	check(sc.ContainerName(tile, ""), Prefix, "test-org", "stackr-test", "pr-12", "orders-api")
	check(sc.ImageRepo(tile), ImagePrefix, "test-org", "stackr-test", "orders-api")

	// The deploy suffix joins like any other segment; a slug can never contain
	// the separator, so it cannot be mistaken for a tile name.
	if got := sc.ContainerName(tile, "1a2b3c4d"); !strings.HasSuffix(got, "_orders-api_1a2b3c4d") {
		t.Fatalf("suffix: %s", got)
	}
}

// A stack "a-b" in env "c" and a stack "a" in env "b-c" must not share a name;
// with a single-dash separator they did.
func TestNamesDoNotCollide(t *testing.T) {
	a := Scope{OrgSlug: "o", StackSlug: "a-b", EnvSlug: "c"}
	b := Scope{OrgSlug: "o", StackSlug: "a", EnvSlug: "b-c"}
	if a.namePrefix() == b.namePrefix() {
		t.Fatalf("collision: %s", a.namePrefix())
	}
}

// Every cron run of a deeply scoped tile failed with the daemon's
// "name must be 63 characters or fewer" and nothing else: the tile's own
// service name fit, its runs' did not.
func TestRunNameStaysInsideSwarmsLimit(t *testing.T) {
	sc := Scope{OrgSlug: "test-org", StackSlug: "stackr-test", EnvSlug: "staging"}

	// The tile that broke it, with the run suffix the jobs service adds.
	name := sc.ContainerName("cron-reconcile-stock", "run-7bd3edcb")
	assert.LessOrEqual(t, len(name), maxObjectName)
	assert.True(t, strings.HasSuffix(name, "_run-7bd3edcb"),
		"the run id is what makes the name unique, so it is the part that survives")

	// A name that already fits is not touched.
	short := Scope{OrgSlug: "o", StackSlug: "s", EnvSlug: "e"}
	assert.Equal(t, "stkr_o_s_e_web_run-1a2b3c4d", short.ContainerName("web", "run-1a2b3c4d"))

	// The tile's own service name, which has no suffix, never changes: it is
	// the identity swarm updates in place for the life of the tile.
	assert.Equal(t, "stkr_test-org_stackr-test_staging_cron-reconcile-stock",
		sc.ServiceName("cron-reconcile-stock"))
}
