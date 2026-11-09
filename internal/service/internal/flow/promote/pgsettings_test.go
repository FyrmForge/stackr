package promote

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// settings: on an operated postgres tile: a plan row and a restart when they
// move, written onto the instance on apply, nothing when the file leaves
// them out or already says it.
func TestOperatedSettings(t *testing.T) {
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
	on := infraFile + "      settings:\n        work_mem: 8MB\n"

	p, err := w.f.Plan(ctx, staging, rel("s1", on).ID, io.Discard)
	must(t, err)
	if got := kinds(p); !strings.Contains(got, "operated:pg-dbsettings+work_mem=8MB") {
		t.Fatalf("plan = %s", got)
	}
	if !strings.Contains(strings.Join(p.Warnings, "|"), "pg-db restarts to change its settings") {
		t.Errorf("warnings = %v", p.Warnings)
	}

	// a file that leaves them out does not touch what the panel set
	w.instance(t, "staging", func(m *store.OperatedInstance) { m.Settings = store.StringMap{"work_mem": "8MB"} })
	p, err = w.f.Plan(ctx, staging, rel("s2", infraFile).ID, io.Discard)
	must(t, err)
	if strings.Contains(kinds(p), "settings") {
		t.Errorf("omitted settings moved: %s", kinds(p))
	}
	p, err = w.f.Plan(ctx, staging, rel("s3", on).ID, io.Discard)
	must(t, err)
	if strings.Contains(kinds(p), "settings") {
		t.Errorf("unchanged settings moved: %s", kinds(p))
	}
	p, err = w.f.Plan(ctx, staging, rel("s4", infraFile+"      settings: {}\n").ID, io.Discard)
	must(t, err)
	if got := kinds(p); !strings.Contains(got, "operated:pg-dbsettings") {
		t.Errorf("cleared = %s", got)
	}
}

// An existing instance is asked first: the hook's refusal fails the apply and
// nothing is stored; a hook that stored them (applied) leaves the row to it.
func TestFileSettingsAreCheckedAgainstTheInstance(t *testing.T) {
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
	on := infraFile + "      settings:\n        nonsense: 1\n"
	w.f.PGSettings = func(context.Context, store.Tile, store.OperatedInstance, map[string]string) (bool, error) {
		return false, errors.New("nonsense is not a setting this postgres knows")
	}
	_, err := w.f.Apply(ctx, staging, rel("c1", on).ID, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "settings: nonsense is not a setting") {
		t.Fatalf("apply = %v", err)
	}
	if m, _ := w.f.D.Operated.GetByTile(ctx, w.pg["staging"].ID); len(m.Settings) != 0 {
		t.Errorf("a refused setting was stored: %v", m.Settings)
	}
}

func TestSettingsAreRefusedWhereTheyMeanNothing(t *testing.T) {
	for name, c := range map[string]struct{ file, want string }{
		"on an image": {tiles("    api:\n      image: x\n      settings:\n        work_mem: 8MB\n"), "tile api: settings: only an operated tile takes it"},
		"on s3":       {tiles("    s:\n      engine: s3\n      settings:\n        work_mem: 8MB\n"), "tile s: settings: s3 has no settings"},
		"an owned one": {
			tiles("    db:\n      engine: postgres\n      settings:\n        wal_level: logical\n"),
			"tile db: settings: wal_level is managed by stackr and cannot be set",
		},
		"an owned prefix": {
			tiles("    db:\n      engine: postgres\n      settings:\n        archive_command: x\n"),
			"archive_command is managed by stackr",
		},
		"empty": {tiles("    db:\n      engine: postgres\n      settings:\n        work_mem: \"\"\n"), "work_mem: the value is empty"},
	} {
		_, err := Load([]byte(c.file), nil, "testorg")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
	if _, err := Load([]byte(tiles("    db:\n      engine: postgres\n      settings:\n        work_mem: 8MB\n")), nil, "testorg"); err != nil {
		t.Errorf("valid settings: %v", err)
	}
}
