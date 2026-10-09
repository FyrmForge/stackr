package promote

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/hostgrant"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

func prEnv(t *testing.T, w *world) store.Environment {
	t.Helper()
	base := w.dev.ID
	e := store.Environment{
		ID: uuid.NewString(), StackID: w.st.ID, Name: "pr-1", Slug: "pr-1", Type: environment.Ephemeral,
		Settings: "{}", Network: "n", FromKind: "branch", FromBranch: "feat", BaseEnvID: &base,
		CreatedAt: time.Now(),
	}
	must(t, w.s.Environments.Create(ctx, e))
	return e
}

// A push to a branch only a PR env builds does nothing: the PR event, which
// the job runs only while PR envs are on, is what builds it.
func TestPushSkipsPREnvWithoutPREvent(t *testing.T) {
	w := setup(t)
	prEnv(t, w)
	var log bytes.Buffer
	r, _, err := w.f.Push(ctx, w.st.ID, Event{Repo: "acme/shop", Branch: "feat", Commit: "c1"}, &log)
	must(t, err)
	if r.ID != "" || !strings.Contains(log.String(), "pr-1 is a PR env") {
		t.Fatalf("release %q, log %q", r.ID, log.String())
	}
}

// A fork PR whose head ref is main must not build into dev: a PR event
// targets only its own pr-<n> env.
func TestPREventNeverBuildsStaticEnv(t *testing.T) {
	w := setup(t)
	prEnv(t, w)
	var log bytes.Buffer
	r, auto, err := w.f.Push(ctx, w.st.ID, Event{Repo: "acme/shop", Branch: "main", Commit: "c1", PR: true, PRNumber: 1}, &log)
	must(t, err)
	if r.ID != "" || len(auto) != 0 {
		t.Fatalf("release %q auto %v, log %q", r.ID, auto, log.String())
	}
}

// A PR env never gets elevated access: the tile that needs it is left out of
// the plan with a note, and nothing parks on an admin.
func TestPREnvSkipsGrantTiles(t *testing.T) {
	w := setup(t)
	w.f.D.HostGrants = hostgrant.New(w.s.HostGrants)
	e := prEnv(t, w)
	w.files["c1"] = sockFile
	r := w.release(t, "c1")
	var log bytes.Buffer
	p, err := w.f.Plan(ctx, e.ID, r.ID, &log)
	must(t, err)
	if len(p.Blockers) != 0 || !strings.Contains(log.String(), "tile mon needs elevated host access") {
		t.Fatalf("blockers %q, log %q", p.Blockers, log.String())
	}
	for _, c := range p.Changes {
		if c.Tile == "mon" {
			t.Fatalf("mon still planned: %+v", c)
		}
	}
}

// pr_envs is the main file's alone: an included one is refused, not dropped.
func TestIncludeRefusesPREnvs(t *testing.T) {
	main := []byte("version: 1\nstack: shop\ninclude: [x.yml]\n")
	fetch := func(string) ([]byte, error) {
		return []byte("version: 1\nstack: shop\npr_envs:\n  enabled: true\n"), nil
	}
	_, err := Load(main, fetch, "acme")
	if err == nil || !strings.Contains(err.Error(), "pr_envs belongs in the main stack file") {
		t.Fatalf("err = %v", err)
	}
}

// A static env that happens to be named pr-1 is not a PR env: a PR event
// never builds it.
func TestPREventNeverBuildsStaticEnvNamedLikePR(t *testing.T) {
	w := setup(t)
	e := prEnv(t, w)
	e.Type = environment.Static
	e.FromBranch = "main"
	must(t, w.s.Environments.Update(ctx, e))
	var log bytes.Buffer
	r, _, err := w.f.Push(ctx, w.st.ID, Event{Repo: "acme/shop", Branch: "main", Commit: "c1", PR: true, PRNumber: 1}, &log)
	must(t, err)
	if r.ID != "" {
		t.Fatalf("release %q, log %q", r.ID, log.String())
	}
}
