package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// EnsureAutoDomain gives a service tile its generated hostname under the
// nearest domain resource visible to its stack. Fired on request (config
// `auto: true`, panel/CLI ask, clone of a tile that had one), never
// automatically for every tile. Idempotent; a stale generated row (the
// naming pattern or resource changed) is replaced. HTTPS: per-host Let's
// Encrypt http-challenge, works at any dot depth, no wildcard cert.
// AddAuto is EnsureAuto asked for by a person, so it carries the guards the
// two handlers used to each write out: it is a structural write (the file
// owns which tiles have hostnames), and a tile with no endpoint has nothing
// to route to. EnsureAuto itself stays silent on those, because a clone
// inheriting the intent must not fail when the tile cannot take it.
func (s *DomainService) AddAuto(ctx context.Context, env *repo.Environment, t *repo.Tile, by Actor) error {
	if t == nil || env == nil {
		return svcerr.ErrNotFound
	}
	// The gate's staging answer is ignored on purpose. An auto row is
	// generated, not authored: it carries no choice a reviewer could approve,
	// it is recomputed from the resource and the slugs every time, and a
	// config apply regenerates it anyway. What the gate is being asked here
	// is only "may this stack be written to at all".
	if _, err := s.gate.Gate(ctx, t.StackID, GateStructural, by.Surface()); err != nil {
		return err
	}
	if t.IsManaged() || t.Kind != "service" || t.ContainerPort == 0 {
		return invalid("", "auto domains need a service with a container port")
	}
	return s.EnsureAuto(ctx, env, t)
}

func (s *DomainService) EnsureAuto(ctx context.Context, env *repo.Environment, t *repo.Tile) error {
	if t.IsManaged() || t.Kind != "service" || t.ContainerPort == 0 {
		return nil
	}
	stack, err := s.store.GetStack(ctx, t.StackID)
	if err != nil || stack == nil {
		return err
	}
	org, err := s.store.GetOrg(ctx, stack.OrgID)
	if err != nil || org == nil {
		return err
	}
	all, err := s.store.ListDomainResources(ctx)
	if err != nil {
		return err
	}
	visible := VisibleDomainResources(all, stack.ID, stack.OrgID)
	if len(visible) == 0 {
		return nil // nothing to nest under
	}
	def, err := s.defaultEnvID(ctx, env.StackID)
	if err != nil {
		return err
	}
	host := AutoHost(visible[0], org.Slug, stack.Slug, env.Slug, t.Slug, def == env.ID)
	existing, err := s.store.ListDomainsByTile(ctx, t.ID)
	if err != nil {
		return err
	}
	kept := existing[:0]
	for _, d := range existing {
		if d.Host == host {
			return nil
		}
		if d.Auto {
			// A generated row under an old pattern; the replacement below
			// carries the current one.
			if err := s.store.DeleteDomain(ctx, d.ID); err != nil {
				return err
			}
			continue
		}
		kept = append(kept, d)
	}
	d := &repo.Domain{
		ID:            uuid.New().String(),
		TileID:        t.ID,
		Host:          host,
		Path:          "/",
		ContainerPort: t.ContainerPort,
		HTTPS:         true,
		// Same default as the API and as a config file with no force_https:
		// serving TLS implies the bounce. Left false the row also diffs against
		// its own serialization for ever, so every later plan carried a change
		// that changed nothing.
		ForceHTTPS: true,
		Auto:       true,
		Position:   len(kept),
		CreatedAt:  time.Now().UTC(),
	}
	if err := s.store.CreateDomain(ctx, d); err != nil {
		return err
	}
	return s.px.SyncTile(ctx, t)
}

// defaultEnvID is the stack's default environment: the bottom rung of the
// declared ladder, which is position 0. Its tiles get the bare generated
// hostname with no env segment in it (AutoHost).
//
// It used to be the store's oldest row, which is a different thing and was a
// bug: whichever environment happened to be created first took the bare host,
// even when the config file declares another one as the bottom rung. On a
// stack where production was applied before staging existed, production took
// site.<stack>.<org>.<domain>, and every later plan then failed with "already
// routes to another service" with no way to fix it from the file.
//
// Position is written from the file's environment order on every apply, so it
// is the file's answer. On a stack nobody manages from a file every position
// is 0 and the oldest row wins the tie, which is the old behaviour.
func (s *DomainService) defaultEnvID(ctx context.Context, stackID string) (string, error) {
	envs, err := s.store.ListEnvironmentsByStack(ctx, stackID)
	if err != nil || len(envs) == 0 {
		return "", err
	}
	best := -1
	for i := range envs {
		// The home env holds stack-scoped instances, never routed tiles, and
		// it is not on the ladder. It must never be the one that wins.
		if envs[i].Type != "static" || envs[i].Slug == repo.HomeSlug {
			continue
		}
		if best < 0 || envs[i].Position < envs[best].Position {
			best = i
		}
	}
	if best < 0 {
		return envs[0].ID, nil
	}
	return envs[best].ID, nil
}
