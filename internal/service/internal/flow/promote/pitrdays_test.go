package promote

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// pitr_days on an operated postgres tile: a plan row and a restart when it
// moves, written onto the instance with the hook told, nothing when the
// file leaves it out or already says it.
func TestOperatedPITRDays(t *testing.T) {
	w := newSliceWorld(t)
	var told []int
	w.f.PITRChanged = func(_ context.Context, _ store.Tile, m store.OperatedInstance) { told = append(told, m.PITRDays) }
	rel := func(commit, file string) store.Release {
		w.files[commit] = file
		r, err := w.f.D.Releases.Create(ctx, w.infra.ID, "test", []release.Pin{
			{Slug: release.ConfigSlug, Repo: w.infra.ConfigRepo, CommitSHA: commit},
		})
		must(t, err)
		return r
	}
	staging := w.envs["staging"].ID
	on := infraFile + "      pitr_days: 7\n"

	p, err := w.f.Plan(ctx, staging, rel("p1", on).ID, io.Discard)
	must(t, err)
	if got := kinds(p); !strings.Contains(got, "operated:pg-dbpitr_days7") {
		t.Fatalf("plan = %s", got)
	}
	if !strings.Contains(strings.Join(p.Warnings, "|"), "pg-db restarts to change its point-in-time recovery window") {
		t.Errorf("warnings = %v", p.Warnings)
	}
	if m, _ := w.f.D.Operated.GetByTile(ctx, w.pg["staging"].ID); m.PITRDays != 0 {
		t.Errorf("a plan wrote the window: %d", m.PITRDays)
	}

	// a file that leaves it out does not touch what the panel set
	w.instance(t, "staging", func(m *store.OperatedInstance) { m.PITRDays = 3 })
	p, err = w.f.Plan(ctx, staging, rel("p2", infraFile).ID, io.Discard)
	must(t, err)
	if strings.Contains(kinds(p), "pitr_days") {
		t.Errorf("an omitted pitr_days moved: %s", kinds(p))
	}
	p, err = w.f.Plan(ctx, staging, rel("p3", infraFile+"      pitr_days: 3\n").ID, io.Discard)
	must(t, err)
	if strings.Contains(kinds(p), "pitr_days") {
		t.Errorf("an unchanged pitr_days moved: %s", kinds(p))
	}
	p, err = w.f.Plan(ctx, staging, rel("p4", infraFile+"      pitr_days: 0\n").ID, io.Discard)
	must(t, err)
	if got := kinds(p); !strings.Contains(got, "operated:pg-dbpitr_days0") {
		t.Errorf("off = %s", got)
	}

	// apply writes the row and tells the hook
	// (the restart that follows cannot finish on the fake daemon; the row is
	// written before the rollout starts)
	_, err = w.f.Apply(ctx, staging, rel("p5", on).ID, io.Discard, nil)
	if err != nil && !strings.Contains(err.Error(), "deploy pg-db") {
		t.Fatal(err)
	}
	if m, _ := w.f.D.Operated.GetByTile(ctx, w.pg["staging"].ID); m.PITRDays != 7 {
		t.Errorf("window = %d, want 7", m.PITRDays)
	}
	if len(told) != 1 || told[0] != 7 {
		t.Errorf("hook told %v", told)
	}
}

func TestPITRDaysIsRefusedWhereItMeansNothing(t *testing.T) {
	for name, c := range map[string]struct{ file, want string }{
		"on an image":   {tiles("    api:\n      image: x\n      pitr_days: 7\n"), "tile api: pitr_days: only an operated tile takes it"},
		"on s3":         {tiles("    s:\n      engine: s3\n      pitr_days: 7\n"), "tile s: pitr_days: s3 has no point-in-time recovery"},
		"too many days": {tiles("    db:\n      engine: postgres\n      pitr_days: 400\n"), "tile db: pitr_days: the window is 0 (off) to 365 days"},
		"negative":      {tiles("    db:\n      engine: postgres\n      pitr_days: -1\n"), "tile db: pitr_days: the window is 0 (off) to 365 days"},
	} {
		_, err := Load([]byte(c.file), nil, "testorg")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
	if _, err := Load([]byte(tiles("    db:\n      engine: postgres\n      pitr_days: 7\n")), nil, "testorg"); err != nil {
		t.Errorf("a valid window: %v", err)
	}
}

// The export says the window, 0 too, and the file it writes loads.
func TestExportWritesPITRDays(t *testing.T) {
	w := newSliceWorld(t)
	w.instance(t, "staging", func(m *store.OperatedInstance) { m.PITRDays = 5 })
	out, _, err := w.f.Export(ctx, w.infra.ID, w.envs["staging"].ID)
	must(t, err)
	if !strings.Contains(string(out), "pitr_days: 5") {
		t.Errorf("export lacks the window:\n%s", out)
	}
	w.instance(t, "staging", func(m *store.OperatedInstance) { m.PITRDays = 0 })
	out, _, err = w.f.Export(ctx, w.infra.ID, w.envs["staging"].ID)
	must(t, err)
	if !strings.Contains(string(out), "pitr_days: 0") {
		t.Errorf("export lacks an explicit off:\n%s", out)
	}
	if _, err := Load(out, nil, "testorg"); err != nil {
		t.Errorf("the export does not load: %v\n%s", err, out)
	}
}
