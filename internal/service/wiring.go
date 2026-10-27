package service

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/selfmetrics"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/deploy"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/orgconfig"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/promote"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/serverconfig"
	"github.com/FyrmForge/stackr/internal/service/internal/git"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/connector"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domainres"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/panel"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
	"github.com/FyrmForge/stackr/internal/service/internal/regfront"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Glue the flows take as funcs: the proxy config's install facts, the
// stack file and tile builds from git, the panel's own spec and archive.

// proxyConfigs is the Syncer's BuildAll: one Caddy config per ingress node,
// keyed by node id. The controller is the default ingress node.
func (o *Orchestrator) proxyConfigs(ctx context.Context) (map[string]json.RawMessage, error) {
	o.logship.tileStale.Store(true) // the hosts below may be new to the log shipper
	get := func(k string) string {
		v, _ := o.settings.Get(ctx, k)
		return v
	}
	in := domain.Install{
		AdminListen:    ProxyAdmin,
		ACMEEmail:      get("acme_email"),
		Managed:        o.managedDomains(ctx),
		TLSOff:         o.cfg.TLSOff,
		IPv4Only:       !o.ipv6On(ctx),
		PanelHost:      get("panel_domain"),
		PanelUpstream:  o.cfg.PanelUpstream,
		TrustedProxies: splitList(get("trusted_proxies")),
		Custom:         get("proxy_custom"),
	}
	on := func(k string) bool { b, _ := strconv.ParseBool(get(k)); return b }
	in.AccessLog = on("observe_enabled") && on("observe_logs_enabled")
	in.AccessQuery, in.AccessIP, in.AccessHeaders = on("observe_access_query"), on("observe_access_ip"), on("observe_access_headers")
	rs, err := o.domainres.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range rs {
		if r.ACMEEmail != "" {
			in.Accounts = append(in.Accounts, domain.Account{Host: r.Host, Email: r.ACMEEmail})
		}
	}
	di, err := o.DefaultIngress(ctx)
	if err != nil {
		return nil, err
	}
	in.DefaultIngress = di
	ctl, err := o.controllerID(ctx)
	if err != nil {
		return nil, err
	}
	in.Probe, in.Controller = probePath(ctl), ctl
	in.IngressNodes = o.ingressNodes(ctx)
	routes, err := o.externalRoutes(ctx)
	if err != nil {
		return nil, err
	}
	// ponytail: external routes carry no ingress pick, so they follow the
	// default ingress node; tiles use IngressFor (ingress_picks).
	ext := make([]domain.NodeRoute, len(routes))
	for i, r := range routes {
		ext[i] = domain.NodeRoute{Route: r}
	}
	return o.deploy.ProxyConfigs(ctx, in, ext, o.placement(ctx, di))
}

// placement answers deploy.Placement: the tile's env node and, unless the
// controller both runs and serves it, the tile's VIP (its pause container's
// address on the env network) so another node's Caddy can reach it. A tile
// whose VIP cannot be read keeps "" and is served by container name, as before.
func (o *Orchestrator) placement(ctx context.Context, defIngress string) deploy.Placement {
	return func(tileID string) (ingress, node, vip string) {
		t, err := o.store.Tiles.Get(ctx, tileID)
		if err != nil {
			return "", "", ""
		}
		node, err = o.envNode(ctx, t.EnvironmentID)
		if err != nil {
			return "", "", ""
		}
		ingress, _ = o.IngressFor(ctx, tileID)
		if ctl, err := o.controllerID(ctx); err != nil || !needsVIP(node, ctl, cmp.Or(ingress, defIngress)) {
			return ingress, node, ""
		}
		if v, ok := o.tileVIP(ctx, node, t.ID); ok {
			vip = v.String()
		}
		return ingress, node, vip
	}
}

// needsVIP: only the controller's own Caddy shares a network with the
// controller's replicas; every other ingress dials the tile's VIP over the mesh.
func needsVIP(node, ctl, ingress string) bool {
	return node != ctl || ingress != ctl
}

// caddyFor is a worker's Caddy admin API, nil while the node has no connection.
func (o *Orchestrator) caddyFor(nodeID string) caddyNode {
	c, err := o.nodeExtras(nodeID)
	if err != nil || c == nil {
		return nil
	}
	return c
}

// caddyNode is one worker's Caddy admin API through its extras client
// (workerclient.Client satisfies it).
type caddyNode interface {
	Caddy(ctx context.Context, method, path string, body []byte) (*http.Response, error)
}

// proxyPush is the Syncer's PushNode. The controller's config goes to the
// local admin socket (push); a worker's goes through worker(id), which
// returns nil for a node with no connection.
func proxyPush(controller func(context.Context) (string, error), push func(context.Context, json.RawMessage) error, worker func(nodeID string) caddyNode, down func(ctx context.Context, nodeID string) error) func(context.Context, string, json.RawMessage) error {
	return func(ctx context.Context, node string, cfg json.RawMessage) error {
		ctl, err := controller(ctx)
		if err != nil {
			return err
		}
		if node == ctl {
			return push(ctx, cfg)
		}
		w := worker(node)
		if w == nil {
			return down(ctx, node)
		}
		resp, err := w.Caddy(ctx, http.MethodPost, "/load", cfg)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
			return fmt.Errorf("caddy on node %s: %s: %s", node, resp.Status, strings.TrimSpace(string(b)))
		}
		return nil
	}
}

// publicBase is a managed tile's public URL: its auto name under the
// nearest resource visible to its stack (REWRITE.md "Domain resources"),
// once a domain row routes that name to the tile. "" otherwise: an
// unrouted name as S3_ENDPOINT would send every consumer to a host nothing
// serves.
// ponytail: a domain resource rename re-publishes the bindings; other moves
// of the visible resources (a new nearer resource) do not.
func (o *Orchestrator) publicBase(ctx context.Context, t store.Tile) string {
	host, _, err := o.autoHost(ctx, t)
	if err != nil {
		return ""
	}
	ds, err := o.domains.ListByTile(ctx, t.ID)
	if err != nil {
		return ""
	}
	for _, d := range ds {
		if d.Host != host || d.RedirectTo != "" {
			continue
		}
		if d.HTTPS {
			return "https://" + host
		}
		return "http://" + host
	}
	return ""
}

// autoHost is the tile's generated name under the nearest domain resource
// visible to its stack, and that resource: what an auto domain attaches and
// what publicBase looks for.
func (o *Orchestrator) autoHost(ctx context.Context, t store.Tile) (string, DomainResource, error) {
	e, err := o.envs.Get(ctx, t.EnvironmentID)
	if err != nil {
		return "", DomainResource{}, err
	}
	st, err := o.stacks.Get(ctx, t.StackID)
	if err != nil {
		return "", DomainResource{}, err
	}
	og, err := o.orgs.Get(ctx, st.OrgID)
	if err != nil {
		return "", DomainResource{}, err
	}
	vis, err := o.domainres.Visible(ctx, st.ID, st.OrgID)
	if err != nil {
		return "", DomainResource{}, err
	}
	if len(vis) == 0 {
		return "", DomainResource{}, errs.Conflictf(
			"No domain resource to nest under. Add one in the server, organization or stack domain settings first.",
		)
	}
	def, err := o.envs.IsDefault(ctx, e)
	if err != nil {
		return "", DomainResource{}, err
	}
	host := domainres.AutoHost(vis[0], og.Slug, st.Slug, e.Slug, t.Slug, def)
	return host, vis[0], nil
}

// TileAutoHost is the generated name the tile's auto button attaches, ""
// when no domain resource is visible to it.
func (o *Orchestrator) TileAutoHost(ctx context.Context, tileID string) (string, error) {
	t, err := o.tiles.Get(ctx, tileID)
	if err != nil {
		return "", err
	}
	host, _, err := o.autoHost(ctx, t)
	if _, ok := errs.IsConflict(err); ok {
		return "", nil
	}
	return host, err
}

// repoLock serialises git work on one clone dir.
func (o *Orchestrator) repoLock(dir string) func() {
	m, _ := o.repoLocks.LoadOrStore(dir, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// clone checks out url at commit ("" = the branch's head) into dir
// through the org's connector: the one connectorID names when set, the one
// for url's host otherwise. It returns the commit checked out.
func (o *Orchestrator) clone(
	ctx context.Context,
	orgID, connectorID, url, branch, dir, commit string,
	log io.Writer,
) (git.Repo, string, error) {
	env, err := o.cloneEnv(ctx, orgID, connectorID, url)
	if err != nil {
		return git.Repo{}, "", err
	}
	r := git.Repo{
		Dir:    dir,
		Root:   filepath.Join(o.cfg.DataDir, "repos"),
		URL:    url,
		Branch: branch,
		Auth:   func(context.Context) []string { return env },
	}
	sha, err := r.Checkout(ctx, commit, log)
	return r, sha, err
}

// configHead is the tip of the stack's config branch, or of the repo's
// default branch when it names none: the branch and its commit.
func (o *Orchestrator) configHead(ctx context.Context, st store.Stack) (branch, sha string, err error) {
	env, err := o.cloneEnv(ctx, st.OrgID, st.ConfigConnectorID, st.ConfigRepo)
	if err != nil {
		return "", "", err
	}
	r := git.Repo{
		URL:    st.ConfigRepo,
		Branch: st.ConfigBranch,
		Auth:   func(context.Context) []string { return env },
	}
	return r.Head(ctx)
}

// cloneEnv is the connector's auth env lines for url, or WithGit's.
func (o *Orchestrator) cloneEnv(ctx context.Context, orgID, connectorID, url string) ([]string, error) {
	if o.gitEnv != nil {
		return o.gitEnv, nil
	}
	var c store.Connector
	var err error
	if connectorID != "" {
		// a read: the org's own connector or a server one shared with it
		c, err = o.conns.Usable(ctx, orgID, connectorID)
	} else {
		c, err = o.conns.For(ctx, orgID, url)
	}
	if err != nil {
		return nil, err
	}
	return o.conns.CloneEnv(ctx, c, url)
}

// tileConnector is the connector a tile's clone names: a tile has no
// connector field, so when several shared server connectors serve its
// host, its stack's config connector breaks the tie if it serves the host
// too. "" lets clone resolve by host.
func (o *Orchestrator) tileConnector(ctx context.Context, st store.Stack, url string) string {
	if o.gitEnv != nil || st.ConfigConnectorID == "" {
		return ""
	}
	if _, err := o.conns.For(ctx, st.OrgID, url); !errors.As(err, &connector.Ambiguous{}) {
		return ""
	}
	c, err := o.conns.Usable(ctx, st.OrgID, st.ConfigConnectorID)
	if err != nil || !connector.Connected(c) || c.Host != connector.Host(url) {
		return ""
	}
	return c.ID
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
	r, _, err := o.clone(ctx, st.OrgID, st.ConfigConnectorID, st.ConfigRepo, st.ConfigBranch, dir, commit, log)
	if err != nil {
		return nil, nil, err
	}
	return clonedStackFile(ctx, r, st, commit)
}

// clonedStackFile is stackFile read from the local clone r, no fetch.
func clonedStackFile(ctx context.Context, r git.Repo, st store.Stack, commit string) ([]byte, promote.Fetcher, error) {
	path := st.ConfigPath
	if path == "" {
		path = promote.DefaultPath
	}
	data, err := r.ReadFile(ctx, commit, path)
	fetch := func(p string) ([]byte, error) {
		if strings.HasSuffix(p, "/") {
			// ponytail: a trailing slash asks for the folder's files, NUL
			// joined, so Config keeps one Fetcher type; a typed lister
			// when a second caller needs one.
			names, err := r.ListTree(ctx, commit, p)
			return []byte(strings.Join(names, "\x00")), err
		}
		return r.ReadFile(ctx, commit, p)
	}
	return data, fetch, err
}

// TileOutsideStackFile: t's stack runs from a stack file and the file its
// env took last does not declare t, so the next promote removes it (the
// same test as the plan's deletes). It reads the local clone only, never
// fetches: anything it cannot read (no release yet, no clone, a bad file)
// answers false.
func (o *Orchestrator) TileOutsideStackFile(ctx context.Context, t Tile) bool {
	st, err := o.stacks.Get(ctx, t.StackID)
	if err != nil || st.ConfigRepo == "" {
		return false
	}
	e, err := o.envs.Get(ctx, t.EnvironmentID)
	if err != nil || e.ReleaseID == nil {
		return false
	}
	pins, err := o.releases.Pins(ctx, *e.ReleaseID)
	commit := pins[release.ConfigSlug].CommitSHA
	r := git.Repo{Dir: filepath.Join(o.cfg.DataDir, "repos", "config-"+st.ID)}
	if err != nil || commit == "" || !r.Has(ctx, commit) {
		return false
	}
	og, err := o.orgs.Get(ctx, st.OrgID)
	if err != nil {
		return false
	}
	data, fetch, err := clonedStackFile(ctx, r, st, commit)
	if err != nil {
		return false
	}
	f, err := promote.Load(data, fetch, og.Slug)
	if err != nil {
		return false
	}
	base := ""
	if e.BaseEnvID != nil {
		if b, err := o.envs.Get(ctx, *e.BaseEnvID); err == nil {
			base = b.Slug
		}
	}
	re, ok := f.EnvFor(e.Slug, base)
	_, declared := re.Tiles[t.Slug]
	return ok && !declared
}

// orgFile is the org's config file at commit ("" = the bound branch's
// head) and the commit it was read at; "" when the clone itself failed.
func (o *Orchestrator) orgFile(ctx context.Context, og store.Org, commit string, log io.Writer) ([]byte, string, error) {
	dir := filepath.Join(o.cfg.DataDir, "repos", "orgconfig-"+og.ID)
	unlock := o.repoLock(dir)
	defer unlock()
	r, sha, err := o.clone(ctx, og.ID, og.ConfigConnectorID, og.ConfigRepo, og.ConfigBranch, dir, commit, log)
	if err != nil {
		return nil, "", err
	}
	data, err := r.ReadFile(ctx, sha, cmp.Or(og.ConfigPath, orgconfig.DefaultPath))
	return data, sha, err
}

// serverFile is the server config file at commit ("" = the bound branch's
// head) and the commit it was read at; "" when the clone itself failed. The
// binding's connector is a server connector, looked up as one: the org-side
// resolution never runs here.
func (o *Orchestrator) serverFile(ctx context.Context, commit string, log io.Writer) ([]byte, string, error) {
	b, err := o.ServerConfigBinding(ctx)
	if err != nil {
		return nil, "", err
	}
	if b.Repo == "" {
		return nil, "", errs.Conflictf("The server has no config repo; bind one first.")
	}
	env := o.gitEnv
	if env == nil {
		if b.ConnectorID == "" {
			return nil, "", errs.Conflictf("The server's config repo names no connector; bind one.")
		}
		c, err := o.conns.Server(ctx, b.ConnectorID)
		if err != nil {
			return nil, "", err
		}
		if env, err = o.conns.CloneEnv(ctx, c, b.Repo); err != nil {
			return nil, "", err
		}
	}
	dir := filepath.Join(o.cfg.DataDir, "repos", "serverconfig")
	unlock := o.repoLock(dir)
	defer unlock()
	r := git.Repo{
		Dir:    dir,
		Root:   filepath.Join(o.cfg.DataDir, "repos"),
		URL:    b.Repo,
		Branch: b.Branch,
		Auth:   func(context.Context) []string { return env },
	}
	sha, err := r.Checkout(ctx, commit, log)
	if err != nil {
		return nil, "", err
	}
	data, err := r.ReadFile(ctx, sha, cmp.Or(b.Path, serverconfig.DefaultPath))
	return data, sha, err
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
	return o.buildInto(ctx, st, t, commit, "", log)
}

// buildInto is buildTile into ref, an image row a release already pins
// (deploy's Build: the row and its id stay); "" names a new one.
func (o *Orchestrator) buildInto(
	ctx context.Context,
	st store.Stack,
	t store.Tile,
	commit, ref string,
	log io.Writer,
) (_ string, err error) {
	defer func(start time.Time) { selfmetrics.Build(err, time.Since(start)) }(time.Now())
	og, err := o.orgs.Get(ctx, st.OrgID)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(o.cfg.DataDir, "repos", tileCheckout(st, t))
	unlock := o.repoLock(dir)
	defer unlock()
	r, _, err := o.clone(ctx, st.OrgID, o.tileConnector(ctx, st, t.GitURL), t.GitURL, t.GitBranch, dir, commit, log)
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
	// The checkout stays on the controller; a builder-tagged worker gets the
	// build context streamed over the mesh (the docker CLI through its
	// proxied Docker). The image goes into the cluster registry: the env's
	// node, and any node a move takes it to, gets it loaded from there.
	ctl, err := o.controllerID(ctx)
	if err != nil {
		return "", err
	}
	repo := regfront.Repo(og.Slug, st.Slug, t.Slug)
	if ref == "" {
		if ref, err = o.images.Name(ctx, "localhost:"+installspec.RegistryPort+"/"+repo, commit, uuid.NewString()); err != nil {
			return "", err
		}
	}
	return buildFallback(o.builderNode(ctx, ctl), ctl,
		func(n string) (string, error) { return o.buildOn(ctx, n, repo, ref, bctx, df, args, log) },
		func(n string) bool { return o.nodeAnswers(ctx, n) }, log)
}

// tileCheckout names a tile's build clone. Never the tile's id: a push
// builds a tile from its stack-file row before any row is saved, so the id
// is "". The stack and slug key it (one build per slug per push); the repo
// hash keeps an env whose tile points at another repo out of its clone,
// since a clone keeps fetching from the remote it was made from.
func tileCheckout(st store.Stack, t store.Tile) string {
	h := sha256.Sum256([]byte(promote.NormalizeRepo(t.GitURL)))
	return "tile-" + st.ID + "-" + t.Slug + "-" + hex.EncodeToString(h[:4])
}

// buildOn builds ref on nodeID and copies it into the cluster registry
// (pushLocal): the node's Docker saves it, the controller writes it.
func (o *Orchestrator) buildOn(
	ctx context.Context,
	nodeID, repo, ref, dir, dockerfile string,
	args map[string]string,
	log io.Writer,
) (string, error) {
	slog.Info("build: placed", "node", nodeID, "ref", ref)
	ctx = docker.WithNode(ctx, nodeID)
	img, err := o.images.Build(ctx, dir, dockerfile, ref, args, log)
	if err != nil {
		return "", err
	}
	if o.cfg.FakeHost == "" { // a fake host has no registry
		leave, err := o.regs.Hold(ctx)
		if err != nil {
			return "", err
		}
		_, err = o.pushLocal(ctx, ref)
		leave()
		if err != nil {
			return "", err
		}
	}
	// The write went past the front, so the row is recorded here.
	_, tag, _ := strings.Cut(strings.TrimPrefix(ref, "localhost:"+installspec.RegistryPort+"/"+repo), ":")
	o.registryPushed(ctx, regfront.Pushed{Repo: repo, Reference: tag, Kind: regfront.KindNode, Subject: nodeID})
	return img.ID, nil
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
	r, err := o.panelBackup(ctx, log, "upgrade", "")
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

// keyLogCapVersion is the build whose boot last ran controllerLogCap.
const keyLogCapVersion = "controller_logcap_version"

// How long controllerLogCap waits for the upgrade helper, and how often it looks.
var logCapWait, logCapPoll = 5 * time.Minute, 2 * time.Second

// controllerLogCap sets Docker's log cap on the controller once per build,
// as the installer and a worker upgrade do; the panel's own Update reaches
// it this way. It runs at boot before the system images are published, and
// a worker rollout starts only after that publish (rolloutAfterBoot), so
// when daemon.json changes the Docker restart bounces this panel before any
// rollout exists; the next boot finds the build recorded and goes on. The
// upgrade helper is waited for first: a Docker restart mid-swap would leave
// the old panel behind. The build is recorded before the run, so a Docker
// that refuses the cap is not restarted at every boot. It only warns.
func (o *Orchestrator) controllerLogCap(ctx context.Context) {
	// A swap copy left beside the database means a helper died mid-swap.
	// Kept: it may be the only database the old panel can open.
	// While the helper still runs, the copy belongs to the swap that booted this panel.
	// Only a running helper counts: a dead one stays as an exited container.
	busy := false
	if cctx, err := o.withController(ctx); err == nil {
		cs, _ := o.docker.List(cctx, map[string]string{panel.LabelRole: "upgrader"})
		for _, c := range cs {
			busy = busy || c.State == "running"
		}
	}
	for _, f := range panel.Leftovers(o.cfg.DBPath) {
		if busy {
			break
		}
		slog.Warn("upgrade: database copy left from an interrupted swap, kept", "file", f)
	}
	if o.cfg.PanelSpec == nil || o.cfg.FakeHost != "" || o.cfg.Version == "" {
		return // not an installed panel: no host to touch
	}
	if v, _, _ := o.store.Settings.Get(ctx, keyLogCapVersion); v == o.cfg.Version {
		return
	}
	ctx, err := o.withController(ctx)
	if err == nil {
		var image string
		if image, err = o.runningPanelImage(ctx); err == nil {
			if err = o.store.Settings.Set(ctx, keyLogCapVersion, o.cfg.Version); err == nil {
				err = o.runLogCap(ctx, image)
			}
		}
	}
	if err != nil {
		slog.Warn("docker log cap: not set", "err", err)
	}
}

// runLogCap runs `stackrd host-run docker-logcap` from image on this host and
// waits for it: the helper waits for the cap's unit, Docker's restart included.
func (o *Orchestrator) runLogCap(ctx context.Context, image string) error {
	_ = o.docker.StopRemove(ctx, hostHelperName) // a leftover of an earlier run
	id, err := o.docker.Run(ctx, hostHelperSpec(image, "host-run", "docker-logcap"))
	if err != nil {
		return err
	}
	defer func() { _ = o.docker.StopRemove(context.WithoutCancel(ctx), id) }()
	wctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	code, err := o.docker.Wait(wctx, id)
	if err == nil && code != 0 {
		out, _ := o.docker.Logs(ctx, id, 20)
		err = fmt.Errorf("host helper exited %d: %s", code, out)
	}
	return err
}

// runningPanelImage is this panel's own image, once the upgrade helper that
// started it is done (the proxy's image may still be the old build's).
func (o *Orchestrator) runningPanelImage(ctx context.Context) (string, error) {
	if _, err := o.upgraderDone(ctx); err != nil {
		return "", err
	}
	ps, err := o.docker.List(ctx, map[string]string{installspec.LabelRole: "panel"})
	ps = slices.DeleteFunc(ps, func(c docker.Container) bool { return c.State != "running" })
	if err != nil {
		return "", err
	}
	if len(ps) != 1 {
		return "", fmt.Errorf("%d running panel containers, want 1", len(ps))
	}
	return ps[0].Image, nil
}

// upgraderDone waits until no upgrade helper runs and returns the helpers
// left behind.
func (o *Orchestrator) upgraderDone(ctx context.Context) ([]docker.Container, error) {
	for deadline := time.Now().Add(logCapWait); ; {
		cs, err := o.docker.List(ctx, map[string]string{installspec.LabelRole: "upgrader"})
		if err != nil {
			return nil, err
		}
		if !slices.ContainsFunc(cs, func(c docker.Container) bool { return c.State == "running" }) {
			return cs, nil
		}
		if time.Now().After(deadline) {
			return nil, errors.New("the upgrade helper is still running")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(logCapPoll):
		}
	}
}

// clearUpgrader removes the panel Update's helper once it is done: one that
// exited 0 swapped this panel in. A failed one stays for its logs; the next
// Update's Launch removes it.
func (o *Orchestrator) clearUpgrader(ctx context.Context) {
	ctx, err := o.withController(ctx)
	if err != nil {
		slog.Warn("upgrade helper: not removed", "err", err)
		return
	}
	cs, err := o.upgraderDone(ctx)
	for _, c := range cs {
		if d, ierr := o.docker.Inspect(ctx, c.ID); ierr == nil && d.State == "exited" && d.ExitCode == 0 {
			err = errors.Join(err, o.docker.StopRemove(ctx, c.ID))
		}
	}
	if err != nil {
		slog.Warn("upgrade helper: not removed", "err", err)
	}
}
