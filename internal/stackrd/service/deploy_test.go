package service

import (
	"context"
	"errors"
	"testing"

	"github.com/FyrmForge/stackr/internal/deploystate"
	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

type deployStore struct {
	repo.Store
	envs  []repo.Environment
	tile  *repo.Tile
	plans []repo.ConfigPlan
	deps  map[string][]repo.Deployment
	tiles map[string][]repo.Tile
}

func (d *deployStore) ListEnvironmentsByStack(context.Context, string) ([]repo.Environment, error) {
	return d.envs, nil
}
func (d *deployStore) GetTile(context.Context, string) (*repo.Tile, error) { return d.tile, nil }
func (d *deployStore) ListTilesByEnv(_ context.Context, envID string) ([]repo.Tile, error) {
	return d.tiles[envID], nil
}
func (d *deployStore) ListDeploymentsByTile(_ context.Context, tileID string, _ int) ([]repo.Deployment, error) {
	return d.deps[tileID], nil
}

// The refusals were spelled in two places and the cron one only on the panel,
// so `stackr apps deploy` on a cron tile queued a deploy for a tile whose
// container is created per run.
func TestDeployableRefusals(t *testing.T) {
	ctx := context.Background()
	// One static env, so nothing here is an upper env.
	st := &deployStore{envs: []repo.Environment{{ID: "e1", Type: "static"}}}
	svc := NewDeployService(st, nil)

	// A deployable tile with no engine is "this server cannot", not "you may
	// not"; a tile the rules refuse answers with the rule, whatever the
	// server can do.
	if err := svc.deployable(ctx, &repo.Tile{Kind: "service", EnvironmentID: "e1"}); !errors.Is(err, svcerr.ErrUnavailable) {
		t.Fatalf("no engine should be unavailable, got %v", err)
	}

	for _, tc := range []struct {
		what string
		tile *repo.Tile
	}{
		{"a volume", &repo.Tile{Kind: "volume", EnvironmentID: "e1"}},
		{"a cron tile", &repo.Tile{Kind: "cron", EnvironmentID: "e1"}},
	} {
		if err := svc.deployable(ctx, tc.tile); err == nil {
			t.Errorf("%s was accepted for deploy", tc.what)
		}
	}
	if err := svc.deployable(ctx, nil); !errors.Is(err, svcerr.ErrNotFound) {
		t.Fatal("a missing tile should be not-found")
	}
}

// An upper environment receives promotions and nothing else. Both surfaces
// refused this already, in two spellings; the rule is one sentence now.
func TestDeployableRefusesAnUpperEnv(t *testing.T) {
	st := &deployStore{envs: []repo.Environment{{ID: "e1", Type: "static"}, {ID: "e2", Type: "static"}}}
	svc := NewDeployService(st, nil)
	err := svc.deployable(context.Background(), &repo.Tile{Kind: "service", EnvironmentID: "e2"})
	var conflict svcerr.Conflict
	if !errors.As(err, &conflict) {
		t.Fatalf("an upper env should be a conflict, got %v", err)
	}
}

type fakeQueue struct {
	applied  []string
	promoted []string
}

func (f *fakeQueue) Apply(_ context.Context, s *repo.Stack, cp *repo.ConfigPlan, force bool, env, commit string) (string, error) {
	f.applied = append(f.applied, cp.ID+"/"+env+"/"+commit)
	return "job-apply", nil
}

func (f *fakeQueue) Promote(_ context.Context, s *repo.Stack, env, commit string) (string, error) {
	f.promoted = append(f.promoted, env+"/"+commit)
	return "job-promote", nil
}

// The first static environment builds on push; it is not a rung. One copy of
// that rule, where the API and the panel each had their own.
func TestTargetRefusesTheFirstRung(t *testing.T) {
	st := &deployStore{envs: []repo.Environment{
		{ID: "e1", Slug: "production", Type: "static"},
		{ID: "e2", Slug: "staging", Type: "static"},
	}}
	svc := NewReleaseService(st, &fakeQueue{})
	stack := &repo.Stack{ID: "s1"}
	if _, err := svc.Target(context.Background(), stack, "production"); err == nil {
		t.Fatal("the first rung was accepted as a promote target")
	}
	env, err := svc.Target(context.Background(), stack, "staging")
	if err != nil || env.ID != "e2" {
		t.Fatalf("staging: %v %+v", err, env)
	}
	if _, err := svc.Target(context.Background(), stack, "nope"); !errors.Is(err, svcerr.ErrNotFound) {
		t.Fatal("an unknown env should be not-found")
	}
}

// A promote moves an image that exists. Both surfaces accepted any string and
// enqueued deploys for a tag nothing had ever built, so the promote reported
// success and every tile failed one at a time with a registry error.
func TestPromoteNeedsABuiltCommit(t *testing.T) {
	ctx := context.Background()
	st := &deployStore{
		envs:  []repo.Environment{{ID: "e1", Slug: "production", Type: "static"}, {ID: "e2", Slug: "staging", Type: "static"}},
		tiles: map[string][]repo.Tile{"e1": {{ID: "t1"}}},
		deps: map[string][]repo.Deployment{"t1": {
			{Status: deploystate.Done, CommitSHA: "abc1234def"},
			{Status: deploystate.Error, CommitSHA: "badbadbad"},
		}},
	}
	q := &fakeQueue{}
	svc := NewReleaseService(st, q)
	stack, env := &repo.Stack{ID: "s1"}, &repo.Environment{ID: "e2", Slug: "staging"}

	if _, err := svc.Promote(ctx, stack, env, PromoteReq{Commit: ""}); err == nil {
		t.Fatal("an empty commit was accepted")
	}
	if _, err := svc.Promote(ctx, stack, env, PromoteReq{Commit: "badbadbad"}); err == nil {
		t.Fatal("a commit whose only deployment failed was accepted")
	}
	if _, err := svc.Promote(ctx, stack, env, PromoteReq{Commit: "abc1234def"}); err != nil {
		t.Fatalf("a built commit was refused: %v", err)
	}
	// Queued, never run on the request.
	if len(q.promoted) != 1 || q.promoted[0] != "staging/abc1234def" {
		t.Fatalf("promoted %v", q.promoted)
	}
	if len(q.applied) != 0 {
		t.Fatalf("a plain promote should not enqueue an apply: %v", q.applied)
	}
}
