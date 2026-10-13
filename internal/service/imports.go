package service

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/registry"
	"github.com/FyrmForge/stackr/internal/service/internal/regfront"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Import brings an outside image into the cluster registry for an org and
// returns the ref every node pulls it by (localhost:5000/<org>/<host>/<path>:
// <tag>) and its digest, which is the upstream's. auth is the id of the org's
// credential for ref's registry, "" for anonymous; it is read here, on the
// controller, and never leaves it. platforms are the CPU types to hold; nil is
// clusterPlatforms. The registry leaf does the copy and writes the record.
//
// Filled by G2 (the credential, the caching, the callers); the copy is G1's
// imgcopy.Copy and the leaf's Import.
func (o *Orchestrator) Import(ctx context.Context, orgID, ref, auth string, platforms []string) (localRef, digest string, err error) {
	og, err := o.store.Orgs.Get(ctx, orgID)
	if err != nil {
		return "", "", err
	}
	if len(platforms) == 0 {
		platforms = o.clusterPlatforms(ctx)
	}
	// ponytail: auth is not resolved yet; every import is anonymous until G2.
	res, err := o.regs.Import(ctx, registry.ImportRequest{OrgID: orgID, OrgSlug: og.Slug, Ref: ref, Platforms: platforms})
	return res.LocalRef, res.Digest, err
}

// clusterPlatforms are the CPU types the cluster's nodes have, from each
// node's Docker Info Architecture (x86_64 is amd64, aarch64 is arm64), sorted,
// no repeats. A node of a new type shows up here the next time it is asked.
//
// Filled by G2. Until then it names none and Import holds none.
func (o *Orchestrator) clusterPlatforms(ctx context.Context) []string {
	return nil
}

// recordImage writes the registry_images row of a version, via the registry
// leaf (the only writer). Imports record themselves inside the leaf; this is
// for callers that put an image there another way.
func (o *Orchestrator) recordImage(ctx context.Context, img store.RegistryImage) error {
	return o.regs.Record(ctx, img)
}

// registryPushed is regfront's Pushed callback: a builder's or a developer's
// manifest PUT succeeded. The record is best effort, never a reason to fail a
// push.
func (o *Orchestrator) registryPushed(ctx context.Context, p regfront.Pushed) {
	org, _, _ := strings.Cut(p.Repo, "/")
	orgID := ""
	if org != regfront.SystemOrg {
		og, err := o.store.Orgs.GetBySlug(ctx, org)
		if err != nil {
			slog.Warn("registry: push of an unknown org", "repo", p.Repo)
			return
		}
		orgID = og.ID
	}
	err := o.regs.Pushed(ctx, registry.Push{
		OrgID: orgID, Repo: p.Repo, Reference: p.Reference, Digest: p.Digest, By: p.Subject,
	})
	if err != nil && !errors.Is(err, errors.ErrUnsupported) {
		slog.Warn("registry: push not recorded", "repo", p.Repo, "ref", p.Reference, "err", err)
	}
}
