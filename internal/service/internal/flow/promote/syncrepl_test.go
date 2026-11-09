package promote

import (
	"io"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// sync, sync_limit_ms and sync_required on an operated postgres tile: plan
// rows when they move, written onto the instance on apply, nothing when the
// file leaves them out or says what is there, and the export says them.
func TestOperatedSync(t *testing.T) {
	w := newSliceWorld(t)
	rel := func(commit, file string) store.Release {
		w.files[commit] = file
		r, err := w.f.D.Releases.Create(ctx, w.infra.ID, "test", []release.Pin{
			{Slug: release.ConfigSlug, Repo: w.infra.ConfigRepo, CommitSHA: commit},
		})
		must(t, err)
		return r
	}
	staging := w.envs["staging"].ID
	on := infraFile + "      sync: true\n      sync_limit_ms: 25\n      sync_required: true\n"

	p, err := w.f.Plan(ctx, staging, rel("s1", on).ID, io.Discard)
	must(t, err)
	got := kinds(p)
	for _, want := range []string{"operated:pg-dbsynctrue", "operated:pg-dbsync_requiredtrue", "operated:pg-dbsync_limit_ms25"} {
		if !strings.Contains(got, want) {
			t.Errorf("plan lacks %s: %s", want, got)
		}
	}
	if strings.Contains(strings.Join(p.Warnings, "|"), "restarts") {
		t.Errorf("sync is live, it needs no restart: %v", p.Warnings)
	}
	if m, _ := w.f.D.Operated.GetByTile(ctx, w.pg["staging"].ID); m.PreferSync {
		t.Error("a plan wrote the choice")
	}

	// a file that says nothing leaves what the panel set
	w.instance(t, "staging", func(m *store.OperatedInstance) { m.PreferSync, m.SyncLimitMS = true, 5 })
	p, err = w.f.Plan(ctx, staging, rel("s2", infraFile).ID, io.Discard)
	must(t, err)
	if strings.Contains(kinds(p), "sync") {
		t.Errorf("an omitted sync moved: %s", kinds(p))
	}
	p, err = w.f.Plan(ctx, staging, rel("s3", infraFile+"      sync: true\n      sync_limit_ms: 5\n").ID, io.Discard)
	must(t, err)
	if strings.Contains(kinds(p), "sync") {
		t.Errorf("an unchanged sync moved: %s", kinds(p))
	}
	p, err = w.f.Plan(ctx, staging, rel("s4", infraFile+"      sync: false\n").ID, io.Discard)
	must(t, err)
	if !strings.Contains(kinds(p), "operated:pg-dbsyncfalse") {
		t.Errorf("off = %s", kinds(p))
	}

	w.instance(t, "staging", func(m *store.OperatedInstance) { m.PreferSync, m.SyncLimitMS = false, 10 })
	_, err = w.f.Apply(ctx, staging, rel("s5", on).ID, io.Discard, nil)
	if err != nil && !strings.Contains(err.Error(), "deploy pg-db") {
		t.Fatal(err)
	}
	m, _ := w.f.D.Operated.GetByTile(ctx, w.pg["staging"].ID)
	if !m.PreferSync || !m.SyncRequired || m.SyncLimitMS != 25 {
		t.Errorf("instance = prefer %v required %v limit %d", m.PreferSync, m.SyncRequired, m.SyncLimitMS)
	}

	out, _, err := w.f.Export(ctx, w.infra.ID, staging)
	must(t, err)
	for _, want := range []string{"sync: true", "sync_required: true", "sync_limit_ms: 25"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("export lacks %q:\n%s", want, out)
		}
	}
	if _, err := Load(out, nil, "testorg"); err != nil {
		t.Errorf("the export does not load: %v\n%s", err, out)
	}
}

func TestSyncIsRefusedWhereItMeansNothing(t *testing.T) {
	for name, c := range map[string]struct{ file, want string }{
		"on an image": {tiles("    api:\n      image: x\n      sync: true\n"), "tile api: sync: only an operated tile takes it"},
		"on s3":       {tiles("    s:\n      engine: s3\n      sync: true\n"), "tile s: sync: only postgres has synchronous replication"},
		"zero limit":  {tiles("    db:\n      engine: postgres\n      sync_limit_ms: 0\n"), "tile db: sync_limit_ms: the limit is 1 to 10000 ms"},
	} {
		_, err := Load([]byte(c.file), nil, "testorg")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
	if _, err := Load([]byte(tiles("    db:\n      engine: postgres\n      sync: true\n")), nil, "testorg"); err != nil {
		t.Errorf("a valid file: %v", err)
	}
}
