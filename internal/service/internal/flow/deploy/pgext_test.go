package deploy_test

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/deploy"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/opmember"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// pgWorld is a deploy flow with real operated engines over the docker fake: a
// running postgres instance "pg-db" with extension lists, a slice "api-db"
// listing vector, and the image tile "api" as its consumer.
type pgWorld struct {
	*world
	pg, slice store.Tile
}

func newPGWorld(t *testing.T) *pgWorld {
	w := setup(t)
	w.f.Engines = &mflow.Flow{
		Tiles:     w.f.Tiles,
		Instances: w.f.Operated,
		Volumes:   w.f.Volumes,
		Envs:      w.f.Envs,
		Stacks:    w.f.Stacks,
		Members:   opmember.New(w.st.OperatedMembers),
		Pull:      func(context.Context, string) error { return nil },
		ReadyWait: 20 * time.Millisecond,
		ReadyPoll: time.Millisecond,
	}
	pg, err := w.f.Tiles.Create(ctx, store.Tile{
		StackID: w.tile.StackID, EnvironmentID: w.env.ID, Name: "pg-db", Kind: tile.Operated,
		ImageRef: "postgres:17",
	})
	must(t, err)
	m, err := w.f.Operated.Create(ctx, pg.ID, "postgres", "stackr", "")
	must(t, err)
	m, err = w.f.Operated.SetMajor(ctx, m, 17)
	must(t, err)
	_, err = w.f.Operated.SetExtensions(ctx, m, nil, []string{"vector"})
	must(t, err)
	from := "shop:dev:pg-db"
	sl, err := w.f.Tiles.Create(ctx, store.Tile{
		StackID: w.tile.StackID, EnvironmentID: w.env.ID, Name: "api-db", Kind: tile.Slice,
		ProvisionFrom: &from, Extensions: []string{"vector"},
	})
	must(t, err)
	w.fake.Containers = append(w.fake.Containers,
		docker.Container{ID: "pgc", State: "running", Labels: map[string]string{tile.LabelTile: pg.ID, tile.LabelRole: "replica"}},
		docker.Container{ID: "pgp", State: "running", Labels: map[string]string{tile.LabelTile: pg.ID, tile.LabelRole: "pause"}},
	)
	w.fake.Details["pgc"] = docker.Detail{Running: true, Health: "healthy", State: "running", Networks: map[string]string{"n": "10.0.0.7"}}
	w.fake.Details["pgp"] = docker.Detail{Running: true, State: "running", Networks: map[string]string{"n": "10.0.0.6"}}
	w.fake.RunID = "pool1" // the pooler, made by each provision
	w.fake.Details["pool1"] = docker.Detail{Running: true, Health: "healthy", State: "running"}
	w.fake.ExecFn = func(cmd []string) string {
		j := strings.Join(cmd, " ")
		switch {
		case strings.Contains(j, "get_auth("):
			return "1,0,0"
		case strings.Contains(j, "pg_available_extensions"):
			return "R|0\nE|plpgsql|1.0|1.0|\nE|vector||0.8.1|\n"
		}
		return ""
	}
	return &pgWorld{w, pg, sl}
}

func (p *pgWorld) reads() int {
	n := 0
	for _, c := range p.fake.Calls() {
		if strings.Contains(strings.Join(c.Args, " "), "pg_available_extensions") {
			n++
		}
	}
	return n
}

// A slice's own deploy reconciles its database; a consumer's bind provisions
// the slice again and reconciles nothing (R10).
func TestProvisionReconcilesAndBindDoesNot(t *testing.T) {
	p := newPGWorld(t)
	if _, err := p.f.Run(ctx, p.slice, "", io.Discard, nil); err != nil {
		t.Fatal(err)
	}
	if p.reads() != 1 || !strings.Contains(string(p.fake.ExecIn), `CREATE EXTENSION IF NOT EXISTS "vector" CASCADE`) {
		t.Fatalf("slice deploy: reads %d, session %q", p.reads(), p.fake.ExecIn)
	}

	p.tile.EnvJSON = `{"DATABASE_URL":"${{ tile.api-db.url }}"}`
	before := p.reads()
	if _, err := p.f.Run(ctx, p.tile, "nginx@sha256:aa", io.Discard, nil); err != nil {
		t.Fatal(err)
	}
	if env := p.fake.Specs[len(p.fake.Specs)-1].Env; !slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, "DATABASE_URL=postgres://") }) {
		t.Fatalf("the consumer was not bound: %v", env)
	}
	if n := p.reads(); n != before {
		t.Errorf("a bind read the extension catalog %d times", n-before)
	}
}

// The operated deploy's phases: a failed readiness is "start", a failed
// pooler "pooler"; both stop before the hook. A good start calls the hook once,
// after the pooler, and its failure is "reconcile" with its text unchanged.
func TestOperatedDeployPhases(t *testing.T) {
	run := func(p *pgWorld) error {
		p.fake.RunIDs = []string{"new"}
		p.fake.Details["new"] = docker.Detail{Running: true, Health: "healthy", State: "running", Networks: map[string]string{"n": "10.0.0.8"}}
		_, err := p.f.Run(ctx, p.pg, "postgres:17", io.Discard, nil)
		return err
	}
	hook := func(p *pgWorld, err error) *int {
		n := new(int)
		p.f.PGExtDue = func(context.Context, store.Tile, io.Writer) error {
			*n++
			return err
		}
		return n
	}

	p := newPGWorld(t)
	n := hook(p, nil)
	if err := run(p); err != nil || *n != 1 {
		t.Fatalf("good start: %v, hook calls %d", err, *n)
	}

	p = newPGWorld(t)
	n = hook(p, nil)
	p.fake.Err = map[string]error{"Exec": errors.New("pg_isready: no response")}
	if err := run(p); deploy.PhaseOf(err) != deploy.PhaseStart || *n != 0 {
		t.Errorf("not ready: %v, phase %q, hook calls %d", err, deploy.PhaseOf(err), *n)
	}

	p = newPGWorld(t)
	n = hook(p, nil)
	if _, err := p.f.Run(ctx, p.slice, "", io.Discard, nil); err != nil { // a slice makes the pooler needed
		t.Fatal(err)
	}
	p.f.Engines.Pull = func(context.Context, string) error { return errors.New("registry down") }
	if err := run(p); deploy.PhaseOf(err) != deploy.PhasePooler || *n != 0 {
		t.Errorf("pooler: %v, phase %q, hook calls %d", err, deploy.PhaseOf(err), *n)
	}

	p = newPGWorld(t)
	n = hook(p, errors.New("extension vector was not created in app: boom"))
	err := run(p)
	if deploy.PhaseOf(err) != deploy.PhaseReconcile || *n != 1 || err.Error() != "extension vector was not created in app: boom" {
		t.Errorf("reconcile: %v, phase %q, hook calls %d", err, deploy.PhaseOf(err), *n)
	}
}
