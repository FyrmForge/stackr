package service

import (
	"context"
	"strings"

	"github.com/FyrmForge/stackr/internal/deploystate"
	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// ApplyQueue is the work queue as this package needs it: one way to start a
// config apply, one to start a promote, both keyed so that two people pressing
// the same button make one job. An interface because the implementation
// (config/stackconf) imports this package.
type ApplyQueue interface {
	// Apply enqueues plan cp, optionally promoting commit onto promoteEnv once
	// the config has landed. The dedupe key is the plan.
	Apply(ctx context.Context, stack *repo.Stack, cp *repo.ConfigPlan, force bool, promoteEnv, promoteCommit string) (string, error)
	// Promote enqueues a promote with no plan behind it. The dedupe key is the
	// environment.
	Promote(ctx context.Context, stack *repo.Stack, envSlug, commit string) (string, error)
}

// ReleaseService owns the promotion ladder: which environments may be promoted
// to, and what a promote is allowed to do.
//
// Three things were wrong here, all of them from the two surfaces having their
// own copy. Both hand-built the apply job under the stack id, which threw away
// the plan-id dedupe that exists because one push makes one plan per
// branch-bound environment. Both ran a plain promote inline on the request, so
// a walk of every tile in every upper rung happened inside the request budget
// and its failures had nowhere to be written. And the panel hard-coded
// force: true, which meant its button silently overrode a refusal the API and
// the CLI respect — an override that is always on is not an override.
type ReleaseService struct {
	store repo.Store
	queue ApplyQueue
}

func NewReleaseService(store repo.Store, queue ApplyQueue) *ReleaseService {
	return &ReleaseService{store: store, queue: queue}
}

// Target resolves the environment named by slug and refuses the ones that are
// not a rung. The first static environment builds on push and is not promoted
// to; an ephemeral one has no ladder at all. One copy, where the API and the
// panel each had their own.
func (s *ReleaseService) Target(ctx context.Context, stack *repo.Stack, slug string) (*repo.Environment, error) {
	if stack == nil {
		return nil, svcerr.ErrNotFound
	}
	envs, err := s.store.ListEnvironmentsByStack(ctx, stack.ID)
	if err != nil {
		return nil, err
	}
	rung := 0
	for i := range envs {
		if envs[i].Type != "static" {
			continue
		}
		if envs[i].Slug == slug {
			if rung == 0 {
				return nil, svcerr.Invalidf("", "the first environment builds on push; it is not promoted to")
			}
			return &envs[i], nil
		}
		rung++
	}
	return nil, svcerr.ErrNotFound
}

// PromoteReq is a promote as a caller asks for it.
type PromoteReq struct {
	// Commit is the full sha to move. Resolving "head", "latest" or a short
	// sha is the CLI's job; by the time it reaches here it is a commit.
	Commit string
	// PlanID makes it an apply-then-promote: the config lands whole first,
	// and both halves share one job so a restart between them cannot leave
	// the promote orphaned.
	PlanID string
	// Force overrides the per-environment apply policy. It defaults to false
	// on every surface now, the panel included: its confirmation dialogue is
	// not the same thing as an override.
	Force bool
}

// Promote queues the promote. It returns the work-queue job id, which is the
// only handle a caller gets — nothing about a promote happens on the request.
func (s *ReleaseService) Promote(ctx context.Context, stack *repo.Stack, env *repo.Environment, in PromoteReq) (string, error) {
	if stack == nil || env == nil {
		return "", svcerr.ErrNotFound
	}
	if s.queue == nil {
		return "", svcerr.ErrUnavailable
	}
	commit := strings.TrimSpace(in.Commit)
	if commit == "" {
		return "", invalid("commit", "required")
	}
	// Checked, because nothing downstream does. Both surfaces accepted any
	// string and enqueued deploys for an image tag that had never been built,
	// so the promote "succeeded" and every tile's deploy failed one by one
	// with a registry error.
	if err := s.built(ctx, stack, commit); err != nil {
		return "", err
	}
	if in.PlanID == "" {
		return s.queue.Promote(ctx, stack, env.Slug, commit)
	}
	cp, err := s.store.GetConfigPlan(ctx, in.PlanID)
	if err != nil {
		return "", err
	}
	if cp == nil || cp.StackID != stack.ID {
		return "", svcerr.ErrNotFound
	}
	return s.queue.Apply(ctx, stack, cp, in.Force, env.Slug, commit)
}

// built reports whether any tile in the stack has a finished deployment of
// this commit. A promote moves an image that exists; if nothing built it there
// is nothing to move.
func (s *ReleaseService) built(ctx context.Context, stack *repo.Stack, commit string) error {
	envs, err := s.store.ListEnvironmentsByStack(ctx, stack.ID)
	if err != nil {
		return err
	}
	for i := range envs {
		tiles, err := s.store.ListTilesByEnv(ctx, envs[i].ID)
		if err != nil {
			continue
		}
		for j := range tiles {
			ds, err := s.store.ListDeploymentsByTile(ctx, tiles[j].ID, 50)
			if err != nil {
				continue
			}
			for k := range ds {
				if ds[k].Status == deploystate.Done && ds[k].CommitSHA == commit {
					return nil
				}
			}
		}
	}
	return svcerr.Invalidf("commit", "%s has not been built on any environment yet", short(commit))
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
