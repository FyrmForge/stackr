package promote

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type builtCalls struct {
	plans   [][]string
	applied []string
	stale   bool
}

// builtWorld is the pgext world with a recipe-built instance and fake plan and
// apply hooks.
func builtWorld(t *testing.T) (*pgExtWorld, *builtCalls) {
	w := newPGExtWorld(t)
	c := &builtCalls{}
	w.instance(t, "staging", func(m *store.OperatedInstance) {
		m.Extensions = store.NameList{"vector"}
		m.Preload = store.NameList{"vchord"}
		m.ExtBuild = `{"base":"postgres:17.5","ref":"stackr-pg:abc"}`
	})
	w.setImage(t, "stackr-pg:abc")
	w.f.PGExtPlan = func(_ context.Context, _ store.Tile, _ store.OperatedInstance, exts []string, _ io.Writer) ([]string, string, error) {
		c.plans = append(c.plans, exts)
		return []string{"builds an image", "restarts"}, "plan-1", nil
	}
	w.f.PGExtApply = func(_ context.Context, _ store.Tile, id string, _ io.Writer) error {
		if c.stale {
			return errors.New("this plan is out of date; a new one is being made. Review it and apply again.")
		}
		c.applied = append(c.applied, id)
		return nil
	}
	return w, c
}

func TestBuiltImageAndPreloadBlocked(t *testing.T) {
	w, c := builtWorld(t)
	if p := w.plan(t, "b1", infraFile+"      image: postgres:17.5\n"); !blocked(p, "is built from its extensions; its image is not set by hand.") {
		t.Errorf("image: %v", p.Blockers)
	}
	if p := w.plan(t, "b2", infraFile+"      preload: [pg_cron]\n"); !blocked(p, "come from its recipes; remove preload, or set a custom image.") {
		t.Errorf("preload: %v", p.Blockers)
	}
	if len(c.plans) != 0 || len(w.calls) != 0 {
		t.Errorf("hooks called: %v %v", c.plans, w.calls)
	}
}

func TestBuiltExtensionsPlanAndApply(t *testing.T) {
	w, c := builtWorld(t)
	file := infraFile + "      extensions: [vector, postgis]\n"
	p := w.plan(t, "b3", file)
	if p.Blocked() || !strings.Contains(kinds(p), "operated:pg-dbextensions+postgis") || !slices.Contains(p.Warnings, "pg-db: restarts") {
		t.Fatalf("plan = %s %v %v", kinds(p), p.Blockers, p.Warnings)
	}
	if len(c.applied) != 0 {
		t.Fatal("plan applied")
	}
	_, err := w.f.Apply(ctx, w.envs["staging"].ID, w.rel(t, "b3", file).ID, io.Discard, nil)
	must(t, err)
	if !slices.Equal(c.applied, []string{"plan-1"}) || len(w.calls) != 0 {
		t.Fatalf("applied %v, PGExt calls %v", c.applied, w.calls)
	}
	c.plans = nil
	if p := w.plan(t, "b4", infraFile+"      extensions: [vector]\n"); strings.Contains(kinds(p), "pg-db") || len(c.plans) != 0 {
		t.Errorf("no-op planned: %s %v", kinds(p), c.plans)
	}
}

func TestBuiltStalePlanFailsApply(t *testing.T) {
	w, c := builtWorld(t)
	file := infraFile + "      extensions: [vector, postgis]\n"
	rel := w.rel(t, "b5", file)
	c.stale = true
	_, err := w.f.Apply(ctx, w.envs["staging"].ID, rel.ID, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "this plan is out of date") {
		t.Fatalf("err = %v", err)
	}
}

func TestBuiltExportOmitsImageAndPreload(t *testing.T) {
	w, _ := builtWorld(t)
	out, _, err := w.f.Export(ctx, w.infra.ID, w.envs["staging"].ID)
	must(t, err)
	s := string(out)
	if strings.Contains(s, "stackr-pg") || strings.Contains(s, "preload") || !strings.Contains(s, "vector") || !strings.Contains(s, "major: 17") {
		t.Errorf("export:\n%s", s)
	}
}
