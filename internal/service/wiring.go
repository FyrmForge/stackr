package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/promote"
	"github.com/FyrmForge/stackr/internal/service/internal/git"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/panel"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/stack"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Glue the flows take as funcs: the proxy config's install facts, the
// stack file and tile builds from git, the panel's own spec and archive.

// proxyConfig is the Syncer's Build: install facts from settings, stack
// ACME accounts, then every domain row through flow/deploy.
func (o *Orchestrator) proxyConfig(ctx context.Context) (json.RawMessage, error) {
	get := func(k string) string {
		v, _ := o.settings.Get(ctx, k)
		return v
	}
	in := domain.Install{
		AdminListen:    ProxyAdmin,
		ACMEEmail:      get("acme_email"),
		DNSProvider:    get("dns_provider"),
		TLSOff:         o.cfg.TLSOff,
		PanelHost:      get("panel_domain"),
		PanelUpstream:  o.cfg.PanelUpstream,
		TrustedProxies: splitList(get("trusted_proxies")),
	}
	orgs, err := o.orgs.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	for _, og := range orgs {
		sts, err := o.stacks.List(ctx, og.ID)
		if err != nil {
			return nil, err
		}
		for _, st := range sts {
			rs, _ := stack.Reservations(st)
			for _, r := range rs {
				if r.ACMEEmail != "" {
					in.Accounts = append(in.Accounts, domain.Account{Host: r.Host, Email: r.ACMEEmail})
				}
			}
		}
	}
	return o.deploy.ProxyConfig(ctx, in)
}

// repoLock serialises git work on one clone dir.
func (o *Orchestrator) repoLock(dir string) func() {
	m, _ := o.repoLocks.LoadOrStore(dir, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// clone checks out url at commit into dir through the org's connector.
func (o *Orchestrator) clone(
	ctx context.Context,
	orgID, url, branch, dir, commit string,
	log io.Writer,
) (git.Repo, error) {
	c, err := o.conns.For(ctx, orgID, url)
	if err != nil {
		return git.Repo{}, err
	}
	env, err := o.conns.CloneEnv(ctx, c, url)
	if err != nil {
		return git.Repo{}, err
	}
	r := git.Repo{
		Dir:    dir,
		URL:    url,
		Branch: branch,
		Auth:   func(context.Context) []string { return env },
	}
	_, err = r.Checkout(ctx, commit, log)
	return r, err
}

// stackFile is promote's Config: the stack file at commit of the config repo,
// and a fetcher for its includes at the same commit.
func (o *Orchestrator) stackFile(
	ctx context.Context,
	st store.Stack,
	commit string,
	log io.Writer,
) ([]byte, promote.Fetcher, error) {
	if st.ConfigRepo == "" {
		return nil, nil, errors.New("this stack has no config repo")
	}
	dir := filepath.Join(o.cfg.DataDir, "repos", "config-"+st.ID)
	unlock := o.repoLock(dir)
	defer unlock()
	r, err := o.clone(ctx, st.OrgID, st.ConfigRepo, st.ConfigBranch, dir, commit, log)
	if err != nil {
		return nil, nil, err
	}
	path := st.ConfigPath
	if path == "" {
		path = promote.DefaultPath
	}
	data, err := r.ReadFile(ctx, commit, path)
	fetch := func(p string) ([]byte, error) { return r.ReadFile(ctx, commit, p) }
	return data, fetch, err
}

// buildTile is promote's Build: clone the tile's repo at commit, build its
// Dockerfile, return the image row id.
func (o *Orchestrator) buildTile(
	ctx context.Context,
	st store.Stack,
	t store.Tile,
	commit string,
	log io.Writer,
) (string, error) {
	og, err := o.orgs.Get(ctx, st.OrgID)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(o.cfg.DataDir, "repos", t.ID)
	unlock := o.repoLock(dir)
	defer unlock()
	r, err := o.clone(ctx, st.OrgID, t.GitURL, t.GitBranch, dir, commit, log)
	if err != nil {
		return "", err
	}
	bctx, err := r.Under(t.BuildContext)
	if err != nil {
		return "", err
	}
	dockerfile := t.DockerfilePath
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	df, err := r.Under(dockerfile)
	if err != nil {
		return "", err
	}
	var args map[string]string
	if strings.TrimSpace(t.BuildArgs) != "" {
		// ponytail: build args are literals; refs in them are not expanded.
		if err := json.Unmarshal([]byte(t.BuildArgs), &args); err != nil {
			return "", err
		}
	}
	ref, err := o.images.Name(ctx, "stkr/"+og.Slug+"_"+st.Slug+"_"+t.Slug, commit, uuid.NewString())
	if err != nil {
		return "", err
	}
	img, err := o.images.Build(ctx, bctx, df, ref, args, log)
	return img.ID, err
}

// panelSpec is the upgrade's container spec; the installer supplies it.
func (o *Orchestrator) panelSpec(image string) docker.ContainerSpec {
	if o.cfg.PanelSpec == nil {
		return docker.ContainerSpec{}
	}
	return o.cfg.PanelSpec(image)
}

// upgradeArchive is the pre-upgrade panel archive; its object key is the
// path the upgrade reports.
func (o *Orchestrator) upgradeArchive(ctx context.Context, log io.Writer) (string, error) {
	if o.cfg.PanelSpec == nil {
		return "", errors.New("no panel spec: this install cannot upgrade itself")
	}
	r, err := o.panelBackup(ctx, log)
	return r.ObjectKey, err
}

// RunPanelSwap is `stackrd upgrade-swap`, run inside the one-shot helper
// container: swap the panel for the spec in STACKR_SWAP_SPEC.
func RunPanelSwap(ctx context.Context) error {
	spec, err := panel.SpecFromEnv()
	if err != nil {
		return err
	}
	d, err := docker.New()
	if err != nil {
		return err
	}
	return panel.New(d).Swap(ctx, spec)
}
