package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/clusterca"
	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/mesh"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/regfront"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/worker"
)

// RegistryHostPrefix: the registry answers at registry.<panel domain>.
const RegistryHostPrefix = "registry."

// registryKey derives the token signing key from the master key.
func (o *Orchestrator) registryKey() []byte {
	m := hmac.New(sha256.New, []byte(o.cfg.SecretsKey))
	m.Write([]byte("stackr registry token v1"))
	return m.Sum(nil)
}

// RegistryHandler is the registry front: /token and /v2/, in front of the
// local registry. Realm follows the request's host, so a node dialing the
// panel directly and a developer through Caddy each get a token URL they can
// reach.
func (o *Orchestrator) RegistryHandler() http.Handler {
	backend, _ := url.Parse("http://127.0.0.1:" + installspec.RegistryPort)
	key := o.registryKey()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme := "https"
		if r.TLS == nil && o.cfg.TLSOff {
			scheme = "http"
		}
		(&regfront.Server{
			Key: key, Realm: scheme + "://" + r.Host, Backend: backend,
			Authenticate: o.registryFacts, Audit: o.registryAudit, Pushed: o.registryPushed,
			Gate: o.regs.Gate(),
		}).ServeHTTP(w, r)
	})
}

// serveRegistryWG serves the registry front to workers over the mesh: the
// controller's transit address, the panel port (the one STACKR-IN lets in
// from stackr0), worker certs only. The address exists once a second node
// brings stackr0 up, so the bind is retried until then.
func (o *Orchestrator) serveRegistryWG(ctx context.Context) {
	addr, ok := o.registryWGAddr()
	if !ok {
		return
	}
	srv := &http.Server{
		Handler:           o.meshHandler(),
		TLSConfig:         clusterca.WorkerServerConfig(o.cluster.ca.Cert, o.cluster.cert, o.cluster.revoked),
		ReadHeaderTimeout: 10 * time.Second,
	}
	for {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			go func() { <-ctx.Done(); _ = srv.Close() }()
			if err := srv.ServeTLS(ln, "", ""); !errors.Is(err, http.ErrServerClosed) {
				slog.Warn("registry: mesh listener", "addr", addr, "err", err)
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
}

// meshHandler is the controller's mesh listener: node cert renewal, and the
// registry front for everything else.
func (o *Orchestrator) meshHandler() http.Handler {
	reg := o.RegistryHandler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == worker.RenewTunnelPath {
			o.renewTunnel(w, r)
			return
		}
		reg.ServeHTTP(w, r)
	})
}

// registryWGAddr is the mesh listener's host:port.
func (o *Orchestrator) registryWGAddr() (string, bool) {
	if o.cluster == nil || o.cfg.PanelPort == 0 {
		return "", false
	}
	return net.JoinHostPort(mesh.ControllerTransit.String(), strconv.Itoa(o.cfg.PanelPort)), true
}

// meshRegistry is the registry a worker's mesh spec names; nil without a
// panel domain (the front answers to registry.<panel domain> only).
func (o *Orchestrator) meshRegistry(ctx context.Context) *worker.RegistrySpec {
	addr, ok := o.registryWGAddr()
	host := o.RegistryHost(ctx)
	if !ok || host == "" {
		return nil
	}
	return &worker.RegistrySpec{URL: "https://" + addr, Host: host}
}

// RegistryMiddleware is an echo Pre middleware: requests for
// registry.<panel domain> go to the registry front, everything else on.
func (o *Orchestrator) RegistryMiddleware() echo.MiddlewareFunc {
	h := o.RegistryHandler()
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if r := c.Request(); o.isRegistryHost(r) {
				h.ServeHTTP(c.Response(), r)
				return nil
			}
			return next(c)
		}
	}
}

func (o *Orchestrator) isRegistryHost(r *http.Request) bool {
	panel, _ := o.settings.Get(r.Context(), "panel_domain")
	if panel == "" {
		return false
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.EqualFold(host, RegistryHostPrefix+installspec.CleanHost(panel))
}

// registryAudit writes the token row and the security event in one
// transaction; a failure refuses the token.
func (o *Orchestrator) registryAudit(ctx context.Context, i regfront.Issued) error {
	return o.store.Tx(ctx, func(tx store.Tx) error {
		if err := tx.RegistryTokens.Create(ctx, store.RegistryToken{
			ID: i.ID, Kind: string(i.Kind), SubjectID: i.Subject, Scope: i.Scope,
			CreatedAt: i.CreatedAt, ExpiresAt: i.ExpiresAt,
		}); err != nil {
			return err
		}
		return audit(tx).Append(ctx, securitylog.Event{
			Action: securitylog.RegistryToken, TargetKind: "registry_token", TargetID: i.ID,
			ActorID: i.Subject, Detail: string(i.Kind) + " " + i.Scope,
		})
	})
}

var errRegistryAuth = errors.New("registry: unauthorized")

// registryFacts authenticates a token request: a node by its client
// certificate, otherwise a user by API key (Basic auth password or Bearer).
// Sessions are host-only cookies of the panel host, so they never reach the
// registry host; docker login uses a key.
func (o *Orchestrator) registryFacts(r *http.Request) (regfront.Facts, error) {
	ctx := r.Context()
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		return o.nodeFacts(ctx, r.TLS.PeerCertificates)
	}
	tok := ""
	if _, pw, ok := r.BasicAuth(); ok {
		tok = pw
	} else if b, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		tok = b
	}
	if tok == "" {
		return regfront.Facts{}, errRegistryAuth
	}
	if strings.HasPrefix(tok, registryLoginPrefix) {
		return o.loginFacts(ctx, tok)
	}
	p, err := o.KeyPrincipal(ctx, tok)
	if err != nil || authz.Self(p.Access) != nil {
		return regfront.Facts{}, errRegistryAuth
	}
	return o.userRegistryFacts(ctx, p), nil
}

// userRegistryFacts is what a user may do in the registry: admins anything,
// others push and pull the orgs they write in and pull the ones they read.
func (o *Orchestrator) userRegistryFacts(ctx context.Context, p *Principal) regfront.Facts {
	f := regfront.Facts{Kind: regfront.KindUser, Subject: p.User.ID}
	// A key bound to an org never gets admin, whatever its user is.
	if p.Access.KeyOrg == "" && authz.Can(p.Access, "node.manage", authz.Resource{}) == nil {
		f.Kind = regfront.KindAdmin
		return f
	}
	for orgID := range p.Access.Roles {
		og, err := o.store.Orgs.Get(ctx, orgID)
		if err != nil {
			continue
		}
		res := authz.Resource{OrgID: orgID}
		switch {
		case authz.Can(p.Access, "stack.write", res) == nil:
			f.Orgs = append(f.Orgs, og.Slug)
		case authz.Can(p.Access, "org.read", res) == nil:
			f.PullOrgs = append(f.PullOrgs, og.Slug)
		}
	}
	return f
}

// nodeFacts: the cert must chain to the cluster CA with the worker role and
// not be revoked; the node may pull the images of the envs pinned to it.
// ponytail: no builder facts yet, build jobs do not run on workers.
func (o *Orchestrator) nodeFacts(ctx context.Context, chain []*x509.Certificate) (regfront.Facts, error) {
	ca, err := clusterca.LoadCA(o.cfg.DataDir)
	if err != nil {
		return regfront.Facts{}, errRegistryAuth
	}
	peer, err := clusterca.Verify(ca.Cert, chain, clusterca.RoleWorker, "", time.Now())
	if err != nil {
		return regfront.Facts{}, errRegistryAuth
	}
	if revoked, err := o.store.NodeCerts.IsRevoked(ctx, peer.Serial); err != nil || revoked {
		return regfront.Facts{}, errRegistryAuth
	}
	if _, err := o.store.Nodes.Get(ctx, peer.NodeID); err != nil {
		return regfront.Facts{}, errRegistryAuth
	}
	envs, err := o.store.Environments.ListByNode(ctx, peer.NodeID)
	if err != nil {
		return regfront.Facts{}, err
	}
	f := regfront.Facts{Kind: regfront.KindNode, Subject: peer.NodeID, PushRepos: o.builds.of(peer.NodeID)}
	for _, e := range envs {
		st, err := o.store.Stacks.Get(ctx, e.StackID)
		if err != nil {
			return f, err
		}
		og, err := o.store.Orgs.Get(ctx, st.OrgID)
		if err != nil {
			return f, err
		}
		ts, err := o.store.Tiles.ListByEnv(ctx, e.ID)
		if err != nil {
			return f, err
		}
		for _, t := range ts {
			f.PullRepos = append(f.PullRepos, regfront.Repo(og.Slug, st.Slug, t.Slug))
			for _, r := range o.tileImports(ctx, og.Slug, e, t) {
				if !slices.Contains(f.PullImports, r) {
					f.PullImports = append(f.PullImports, r)
				}
			}
		}
	}
	return f, nil
}
