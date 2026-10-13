package service

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/registry"
	"github.com/FyrmForge/stackr/internal/service/internal/regfront"
)

// RegistryRepo and RegistryVersion are the leaf's shapes, named for the API.
type (
	RegistryRepo     = registry.Repository
	RegistryVersion  = registry.Version
	RegistryOrgUsage = registry.Usage
)

// OrgRepositories lists the org's repositories from the local registry, by
// name. The registry being down is an error the page shows as its text.
func (o *Orchestrator) OrgRepositories(ctx context.Context, orgID string) ([]registry.Repository, error) {
	og, err := o.store.Orgs.Get(ctx, orgID)
	if err != nil {
		return nil, err
	}
	return o.regs.List(ctx, og.Slug)
}

// RegistryHost is the address the registry answers at ("" until the panel
// has a domain).
func (o *Orchestrator) RegistryHost(ctx context.Context) string {
	panel, _ := o.settings.Get(ctx, "panel_domain")
	if panel == "" {
		return ""
	}
	return RegistryHostPrefix + installspec.CleanHost(panel)
}

// RegistryVersions are the versions of one of the org's repos, newest first,
// each with the envs that run it now. A repo of another org is not found.
func (o *Orchestrator) RegistryVersions(ctx context.Context, orgID, repo string) ([]registry.Version, error) {
	og, err := o.store.Orgs.Get(ctx, orgID)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(repo, og.Slug+"/") {
		return nil, errs.ErrNotFound
	}
	vs, err := o.regs.Versions(ctx, repo)
	if err != nil {
		return nil, err
	}
	use, err := o.registryUse(ctx, og.ID, og.Slug)
	if err != nil {
		return nil, err
	}
	for i := range vs {
		vs[i].UsedBy = use.by(repo, vs[i])
	}
	return vs, nil
}

// DeleteRegistryVersion removes the version of repo that tag names, with
// every other tag on its digest. A version an env runs now is refused,
// naming the envs, unless force.
func (o *Orchestrator) DeleteRegistryVersion(ctx context.Context, orgID, repo, tag string, force bool) error {
	vs, err := o.RegistryVersions(ctx, orgID, repo)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(vs, func(v registry.Version) bool { return v.Tag == tag })
	if i < 0 {
		return errs.ErrNotFound
	}
	digest := vs[i].Digest
	var used []string
	for _, v := range vs {
		if v.Digest == digest {
			for _, e := range v.UsedBy {
				if !slices.Contains(used, e) {
					used = append(used, e)
				}
			}
		}
	}
	if len(used) > 0 && !force {
		return errs.Conflictf("%s:%s is in use by %s.", repo, tag, strings.Join(used, ", "))
	}
	// The cleanup's idea of in use also holds pinned releases and rollback
	// targets; deleting one of those would strand a rollback.
	if !force {
		kept, err := o.registryInUse(ctx)
		if err != nil {
			return err
		}
		if kept[digest] {
			return errs.Conflictf("%s:%s is pinned by a release or a rollback target.", repo, tag)
		}
	}
	return o.regs.Delete(ctx, repo, digest)
}

// registryUse is what the envs of an org run now, from each env's current
// release: a built image by its ref (repo:tag), a pulled one by its digest.
type registryUse map[string][]string

func (u registryUse) by(repo string, v registry.Version) []string {
	return slices.Concat(u[repo+":"+v.Tag], u[repo+"@"+v.Digest])
}

func (o *Orchestrator) registryUse(ctx context.Context, orgID, orgSlug string) (registryUse, error) {
	use := registryUse{}
	sts, err := o.store.Stacks.ListByOrg(ctx, orgID)
	if err != nil {
		return nil, err
	}
	for _, st := range sts {
		es, err := o.store.Environments.ListByStack(ctx, st.ID)
		if err != nil {
			return nil, err
		}
		for _, e := range es {
			if e.ReleaseID == nil {
				continue
			}
			pins, err := o.releases.Pins(ctx, *e.ReleaseID)
			if err != nil {
				return nil, err
			}
			who := st.Slug + "/" + e.Slug
			for _, p := range pins {
				key := ""
				switch {
				case p.ImageID != nil:
					im, err := o.store.Images.Get(ctx, *p.ImageID)
					if err != nil {
						continue // an image row gone: nothing to match
					}
					key = strings.TrimPrefix(im.Ref, "localhost:"+installspec.RegistryPort+"/")
				case p.Digest != "" && p.Repo != "":
					key = regfront.ImportRepo(orgSlug, p.Repo) + "@" + p.Digest
				}
				if key != "" && !slices.Contains(use[key], who) {
					use[key] = append(use[key], who)
				}
			}
		}
	}
	return use, nil
}

// RegistryOverview is the admin's view of the registry: what each org holds
// and how the last cleanup went.
type RegistryOverview struct {
	Orgs     []RegistryOrgUsage  `json:"orgs"`
	Versions int                 `json:"versions"`
	Bytes    int64               `json:"bytes"`
	Last     *RegistryCleanState `json:"last_clean"` // nil = never ran
}

// RegistryCleanState is the result of one cleanup, as the sweep leaves it (RegistryClean).
type RegistryCleanState struct {
	At      time.Time `json:"at"`
	Deleted int       `json:"deleted"`
	Freed   int64     `json:"freed"`
	Errors  []string  `json:"errors"`
}

// RegistryOverview sums the registry per org and adds the last cleanup.
func (o *Orchestrator) RegistryOverview(ctx context.Context) (RegistryOverview, error) {
	us, err := o.regs.Usage(ctx)
	if err != nil {
		return RegistryOverview{}, err
	}
	v := RegistryOverview{Orgs: us}
	for _, u := range us {
		v.Versions += u.Versions
		v.Bytes += u.Bytes
	}
	if c, ok := o.LastRegistryClean(); ok {
		v.Last = &RegistryCleanState{At: c.At, Deleted: c.Deleted, Freed: c.Freed, Errors: c.Errors}
	}
	return v, nil
}
