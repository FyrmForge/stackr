package service_test

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// A new instance is made with each major still supported, newest by default,
// pinned to an exact minor of it; an unsupported major, or an image of another
// one than named, is refused.
func TestCreateOperatedPicksMajor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := servicetest.New(t)
	tl := env.Tile(t, env.Org(t, "acme"))
	o := env.Orch
	majors := service.PGMajors()
	if len(majors) == 0 || majors[len(majors)-1] != 18 {
		t.Fatalf("pickable = %v", majors)
	}
	major := func(id string) int {
		m, err := o.PGVersion(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return m.Major
	}
	for _, m := range majors {
		pg, err := o.CreateOperatedTile(ctx, service.Tile{EnvironmentID: tl.Env, Name: "pg" + strconv.Itoa(m)}, "postgres", m)
		if err != nil {
			t.Fatalf("major %d: %v", m, err)
		}
		if !strings.HasPrefix(pg.ImageRef, "postgres:"+strconv.Itoa(m)+".") || major(pg.ID) != m {
			t.Errorf("major %d: image %q, stored %d", m, pg.ImageRef, major(pg.ID))
		}
		if v, _ := o.PGVersion(ctx, pg.ID); v.UpdateTo != "" {
			t.Errorf("a new instance has an update: %+v", v)
		}
	}
	pg, err := o.CreateOperatedTile(ctx, service.Tile{EnvironmentID: tl.Env, Name: "newest"}, "postgres", 0)
	if err != nil || major(pg.ID) != 18 {
		t.Errorf("default: %v", err)
	}
	for name, c := range map[string]struct {
		engine, image string
		major         int
	}{
		"unknown major":  {"postgres", "", 13},
		"future major":   {"postgres", "", 19},
		"image mismatch": {"postgres", "postgres:16.1", 17},
		"eol image":      {"postgres", "postgres:12.1", 0},
		"s3 has none":    {"s3", "", 17},
	} {
		_, err := o.CreateOperatedTile(ctx, service.Tile{EnvironmentID: tl.Env, Name: "x", ImageRef: c.image}, c.engine, c.major)
		if _, ok := errs.IsInvalid(err); !ok {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// An instance on an older minor is offered the table's; applying it keeps the
// major and the instance then has nothing to apply. A major change is refused.
func TestPGVersionUpdate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := servicetest.New(t)
	tl := env.Tile(t, env.Org(t, "acme"))
	o := env.Orch
	pg, err := o.CreateOperatedTile(ctx, service.Tile{EnvironmentID: tl.Env, Name: "pg", ImageRef: "postgres:16.1"}, "postgres", 0)
	if err != nil {
		t.Fatal(err)
	}
	v, err := o.PGVersion(ctx, pg.ID)
	if err != nil || v.Major != 16 || !strings.HasPrefix(v.UpdateTo, "postgres:16.") || v.UpdateTo == "postgres:16.1" || v.EOL {
		t.Fatalf("version = %+v, %v", v, err)
	}
	if v.EOLDate != "2028-11-09" {
		t.Errorf("eol date %q", v.EOLDate)
	}
	got, _, err := o.UpdatePGVersion(ctx, pg.ID)
	if err != nil || got.ImageRef != v.UpdateTo {
		t.Fatalf("apply: %q, %v", got.ImageRef, err)
	}
	if v, _ = o.PGVersion(ctx, pg.ID); v.UpdateTo != "" || v.Image != got.ImageRef {
		t.Errorf("after: %+v", v)
	}
	if _, _, err := o.UpdatePGVersion(ctx, pg.ID); err == nil {
		t.Error("a second update was taken")
	}
	_, _, err = o.UpdateTile(ctx, pg.ID, func(t *service.Tile) error { t.ImageRef = "postgres:17.1"; return nil })
	if err == nil || !strings.Contains(err.Error(), "Major upgrades are not supported") {
		t.Errorf("major change: %v", err)
	}
}
