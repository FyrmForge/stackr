package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/image"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/registry"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/regfront"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Import brings an outside image into the cluster registry for an org and
// returns the ref every node pulls it by (localhost:5000/<org>/<host>/<path>:
// <tag>) and its digest, which is the upstream's. auth is the id of the org's
// credential for ref's registry, "" for anonymous; it is read here, on the
// controller, and never leaves it. platforms are the CPU types to hold; nil is
// clusterPlatforms. The registry leaf does the copy and writes the record.
func (o *Orchestrator) Import(ctx context.Context, orgID, ref, auth string, platforms []string) (localRef, digest string, err error) {
	return o.importLogged(ctx, orgID, ref, auth, platforms, nil)
}

// importLogged is Import with a log: the warning for an unreachable upstream
// answered from our own copy goes there (the deploy log).
func (o *Orchestrator) importLogged(ctx context.Context, orgID, ref, auth string, platforms []string, log io.Writer) (localRef, digest string, err error) {
	og, err := o.store.Orgs.Get(ctx, orgID)
	if err != nil {
		return "", "", err
	}
	if len(platforms) == 0 {
		platforms = o.clusterPlatforms(ctx)
	}
	req := registry.ImportRequest{OrgID: orgID, OrgSlug: og.Slug, Ref: ref, Platforms: platforms, Log: log}
	if auth != "" {
		c, err := o.creds.Get(ctx, orgID, auth)
		if err != nil {
			return "", "", err
		}
		req.Auth = authn.FromConfig(authn.AuthConfig{Username: c.Username, Password: c.Password})
	}
	res, err := o.regs.Import(ctx, req)
	return res.LocalRef, res.Digest, err
}

// outsideRef reports an image ref that does not come from stackr: not a
// build or an import in the cluster registry (localhost:5000/...), not
// stackr's own host (registry.<panel domain>/...), not stackr.system.
func (o *Orchestrator) outsideRef(ctx context.Context, ref string) bool {
	if ref == "" || strings.HasPrefix(ref, image.ClusterRegistry) || strings.HasPrefix(ref, regfront.SystemOrg+"/") {
		return false
	}
	if own := o.RegistryHost(ctx); own != "" && strings.HasPrefix(ref, own+"/") {
		return false
	}
	return true
}

// importForDeploy is flow/deploy's Import hook: an outside ref is imported on
// the controller before its node pulls, with the org's credential credID
// ("" anonymous). imported=false: the ref is not outside, or the install does
// not route through the registry (Config.RegistryImports), so the node pulls
// it as it is. run is the ref the container must be started with: a
// digest-pinned ref cannot be tagged by Docker, so it runs under its
// localhost name; a tag ref keeps the name the user gave (the router tags the
// pulled copy with it).
func (o *Orchestrator) importForDeploy(ctx context.Context, orgID, ref, credID string, log io.Writer) (run string, imported bool, err error) {
	if !o.cfg.RegistryImports || !o.outsideRef(ctx, ref) {
		return ref, false, nil
	}
	local, digest, err := o.importLogged(ctx, orgID, ref, credID, nil, log)
	if err != nil {
		return "", false, err
	}
	if strings.Contains(ref, "@") {
		return local[:strings.LastIndex(local, ":")] + "@" + digest, true, nil
	}
	return ref, true, nil
}

// platformTTL is how long a node's CPU type is trusted before Docker is asked
// again.
const platformTTL = time.Minute

// platformCache remembers each node's CPU type for platformTTL.
type platformCache struct {
	mu sync.Mutex
	at map[string]platformEnt
}

type platformEnt struct {
	arch string
	at   time.Time
}

// dockerArch is Docker's Architecture as the platform name images use.
func dockerArch(a string) string {
	switch a = strings.ToLower(a); a {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	case "armv7l", "armhf", "arm":
		return "arm"
	case "i386", "i686", "x86":
		return "386"
	case "":
		return ""
	}
	return a // ppc64le, s390x, riscv64 are named the same in both
}

// clusterPlatforms are the CPU types the cluster's nodes have, from each
// node's Docker Info Architecture (x86_64 is amd64, aarch64 is arm64), sorted,
// no repeats. A node of a new type shows up here the next time it is asked
// after the cache (a minute) lapses; a node that does not answer keeps its
// last answer. Nothing known at all means the controller's own type.
// ponytail: serial, one Info call per node a minute at most; fan out if a
// slow node ever holds a deploy up.
func (o *Orchestrator) clusterPlatforms(ctx context.Context) []string {
	ns, err := o.nodeLeaf().List(ctx)
	if err != nil {
		slog.Warn("platforms: nodes not listed", "err", err)
	}
	var out []string
	o.platforms.mu.Lock()
	defer o.platforms.mu.Unlock()
	if o.platforms.at == nil {
		o.platforms.at = map[string]platformEnt{}
	}
	for _, n := range ns {
		if n.Status == node.StatusPending || n.Status == node.StatusJoined {
			continue // never reachable yet, runs nothing
		}
		e := o.platforms.at[n.ID]
		if time.Since(e.at) > platformTTL {
			hi, err := o.docker.HostInfo(docker.WithNode(ctx, n.ID))
			if err != nil {
				slog.Warn("platforms: node not asked", "node", n.Name, "err", err)
			} else if a := dockerArch(hi.Arch); a != "" {
				e = platformEnt{arch: a, at: time.Now()}
				o.platforms.at[n.ID] = e
			}
		}
		if e.arch != "" {
			out = append(out, e.arch)
		}
	}
	if len(out) == 0 {
		out = append(out, runtime.GOARCH)
	}
	slices.Sort(out)
	return slices.Compact(out)
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

// tileImports are the import repos (regfront.ImportRepo) of the outside
// images tile t of env e can run: what the env's release pins, its tag, and a
// managed engine's image. The node that hosts the env may pull exactly these
// (regfront Facts.PullImports); the set is the one the deploy imports.
func (o *Orchestrator) tileImports(ctx context.Context, orgSlug string, e store.Environment, t store.Tile) []string {
	var refs []string
	if cur, _, err := o.deploy.Current(ctx, t, e); err == nil {
		refs = append(refs, cur)
	}
	if tile.Pulls(t) {
		refs = append(refs, t.ImageRef)
	}
	if t.Kind == tile.Managed {
		if c, err := o.engines.Container(ctx, t); err == nil {
			refs = append(refs, c.Image)
		}
	}
	var out []string
	for _, ref := range refs {
		if o.outsideRef(ctx, ref) {
			out = append(out, regfront.ImportRepo(orgSlug, ref))
		}
	}
	return out
}
