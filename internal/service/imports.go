package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/imgcopy"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/image"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/registry"
	"github.com/FyrmForge/stackr/internal/service/internal/regfront"
	lregistry "github.com/FyrmForge/stackr/internal/service/internal/registry"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Import brings an outside image into the cluster registry for an org and
// returns the ref every node pulls it by (localhost:5000/<org>/<host>/<path>:
// <tag>) and its digest, which is the upstream's. auth is the id of the org's
// credential for ref's registry, "" for anonymous; it is read here, on the
// controller, and never leaves it. platforms are the CPU types to hold; nil is
// clusterPlatforms. The registry leaf does the copy and writes the record.
func (o *Orchestrator) Import(ctx context.Context, orgID, ref, auth string, platforms []string) (localRef, digest string, err error) {
	var c store.Credential
	if auth != "" {
		if c, err = o.creds.Get(ctx, orgID, auth); err != nil {
			return "", "", err
		}
	}
	return o.importLogged(ctx, orgID, ref, c, platforms, nil)
}

// importLogged is Import with the credential itself (zero = anonymous) and
// a log: the warning for an unreachable upstream answered from our own copy
// goes there (the deploy log). A refusal comes back as one line naming the
// fix (deniedFix).
func (o *Orchestrator) importLogged(ctx context.Context, orgID, ref string, c store.Credential, platforms []string, log io.Writer) (localRef, digest string, err error) {
	og, err := o.store.Orgs.Get(ctx, orgID)
	if err != nil {
		return "", "", err
	}
	if len(platforms) == 0 {
		platforms = o.clusterPlatforms(ctx)
	}
	req := registry.ImportRequest{OrgID: orgID, OrgSlug: og.Slug, Ref: ref, Platforms: platforms, Log: log}
	if c.Password != "" {
		req.Auth = authn.FromConfig(authn.AuthConfig{Username: c.Username, Password: c.Password})
	}
	res, err := o.regs.Import(ctx, req)
	if no := (*imgcopy.DeniedError)(nil); errors.As(err, &no) {
		err = fmt.Errorf("%w: %s", err, deniedFix(ref, c))
	}
	return res.LocalRef, res.Digest, err
}

// pullCred is the credential every pull and registry check of ref uses
// (deploy, import, the image watch): the org's own for ref's host; else, for
// ghcr.io, the installation token of the org's GitHub connector installed on
// the image's owner (no ID: it is no row); else none. ok=false = anonymous.
func (o *Orchestrator) pullCred(ctx context.Context, orgID, ref string) (store.Credential, bool, error) {
	if c, ok, err := o.creds.For(ctx, orgID, ref); err != nil || ok {
		return c, ok, err
	}
	owner, ok := ghcrOwner(ref)
	if !ok {
		return store.Credential{}, false, nil
	}
	tok, ok, err := o.conns.PackageToken(ctx, orgID, owner)
	if err != nil || !ok {
		return store.Credential{}, false, err
	}
	return store.Credential{OrgID: orgID, URL: ghcrHost, Username: "x-access-token", Password: tok}, true, nil
}

const ghcrHost = "ghcr.io"

// ghcrOwner is the owner (first path segment) of a ghcr.io ref, pinned or
// not; ok=false for any other host.
func ghcrOwner(ref string) (string, bool) {
	name, _, _ := strings.Cut(ref, "@")
	host, path, _, ok := lregistry.ParseRef(name)
	if !ok || !strings.EqualFold(host, ghcrHost) {
		return "", false
	}
	owner, _, _ := strings.Cut(path, "/")
	return owner, true
}

// deniedFix is what to do about a registry that refused ref, given the
// credential that was offered (zero = none; no ID = a GitHub connector's).
func deniedFix(ref string, c store.Credential) string {
	name, _, _ := strings.Cut(ref, "@")
	host, _, _, _ := lregistry.ParseRef(name)
	owner, ghcr := ghcrOwner(ref)
	switch {
	case c.Password != "" && c.ID == "":
		return "the GitHub connector for " + owner + " cannot read it; give its app access to the package or add a ghcr.io credential to the org"
	case c.Password != "":
		return "the org's credential for " + host + " cannot read it"
	case ghcr:
		return "connect GitHub for " + owner + " or add a ghcr.io credential to the org"
	}
	return "add a credential for " + host + " to the org"
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

// ownClusterRef refuses a ref into the cluster registry (localhost:5000/...
// or registry.<panel domain>/...) under another org's name: a node that
// already holds that image would run it without asking the registry, so the
// pass check at pull time is not enough. stackr's own images are anyone's.
func (o *Orchestrator) ownClusterRef(ctx context.Context, orgID, ref string) error {
	path, ok := strings.CutPrefix(ref, image.ClusterRegistry)
	if own := o.RegistryHost(ctx); !ok && own != "" {
		path, ok = strings.CutPrefix(ref, own+"/")
	}
	if !ok {
		return nil
	}
	owner, _, _ := strings.Cut(path, "/")
	if owner == regfront.SystemOrg {
		return nil
	}
	og, err := o.store.Orgs.Get(ctx, orgID)
	if err != nil {
		return err
	}
	if owner != og.Slug {
		return errs.Refusedf("%s is not an image of this org", ref)
	}
	return nil
}

// importForDeploy is flow/deploy's Import hook: an outside ref is imported on
// the controller before its node pulls, with c (pullCred's; zero anonymous). imported=false: the ref is not outside, or the install does
// not route through the registry (Config.RegistryImports), so the node pulls
// it as it is. run is the ref the node pulls and the container runs: the
// org's own copy at its digest, never the outside name, which every org on a
// node shares (two orgs on one tag would each retag it for the other).
func (o *Orchestrator) importForDeploy(ctx context.Context, orgID, ref string, c store.Credential, log io.Writer) (run string, imported bool, err error) {
	if err := o.ownClusterRef(ctx, orgID, ref); err != nil {
		return "", false, err
	}
	if !o.cfg.RegistryImports || !o.outsideRef(ctx, ref) {
		return ref, false, nil
	}
	local, digest, err := o.importLogged(ctx, orgID, ref, c, nil, log)
	if err != nil {
		return "", false, err
	}
	return local[:strings.LastIndex(local, ":")] + "@" + digest, true, nil
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
			} else if a := docker.Arch(hi.Arch); a != "" {
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
