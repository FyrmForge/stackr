// Package connector owns git connectors: one GitHub App per org and host.
// It runs the connect handshake (pending row, state check, manifest
// conversion), resolves a git_url to its org's connector, and mints the
// clone credential. A connector never serves another org.
package connector

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/githubapp"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

const (
	GitHub     = "github"
	GitHubHost = "github.com"
)

// App is the slice of the GitHub App wrapper this leaf needs.
type App interface {
	Manifest(connectorID, ghOrg, state string) (action, manifest string, err error)
	ConvertManifest(ctx context.Context, code string) (githubapp.App, error)
	Token(ctx context.Context, key string, app githubapp.App) (string, error)
	Repos(ctx context.Context, token string) ([]githubapp.Repo, error)
}

type Leaf struct {
	conns store.ConnectorStore
	app   App
}

func New(conns store.ConnectorStore, app App) *Leaf { return &Leaf{conns: conns, app: app} }

// NoConnector is a git_url whose host has no connected connector in the org:
// a typed error the promote diff shows.
type NoConnector struct{ Host string }

func (e NoConnector) Error() string { return "no connected git connector for " + e.Host }

// config is the encrypted connectors.config: the nonce while pending, the
// App once connected.
type config struct {
	State string         `json:"state,omitempty"`
	App   *githubapp.App `json:"app,omitempty"`
}

func parse(c store.Connector) config {
	var cfg config
	_ = json.Unmarshal([]byte(c.Config), &cfg)
	return cfg
}

// Connected says whether the handshake finished.
func Connected(c store.Connector) bool { return parse(c).App != nil }

// InstallURL is GitHub's page for installing the app on an account and
// picking its repos; "" while the handshake is pending. Creating the app is
// not installing it: without an install there is no token to clone with.
// Reads the row itself, List strips the config that holds the slug.
func (l *Leaf) InstallURL(ctx context.Context, orgID, id string) (string, error) {
	c, err := l.Get(ctx, orgID, id)
	if err != nil {
		return "", err
	}
	app := parse(c).App
	if app == nil {
		return "", nil
	}
	return "https://github.com/apps/" + app.Slug + "/installations/new", nil
}

// Get refuses a connector of another org (as not found). The id can come from a stack row
// or a request, and without this a file naming another org's connector
// would clone that org's private repos with that org's token.
func (l *Leaf) Get(ctx context.Context, orgID, id string) (store.Connector, error) {
	c, err := l.conns.Get(ctx, id)
	if err != nil {
		return c, err
	}
	if c.OrgID != orgID {
		slog.Error("connector requested by another org", "connector", id, "owner", c.OrgID, "org", orgID)
		return store.Connector{}, errs.ErrNotFound // not yours = not there
	}
	return c, nil
}

// List is the org's connectors, config stripped.
func (l *Leaf) List(ctx context.Context, orgID string) ([]store.Connector, error) {
	cs, err := l.conns.ListByOrg(ctx, orgID)
	for i := range cs {
		cs[i].Config = ""
	}
	return cs, err
}

// ListConnected is the org's connectors that finished GitHub's handshake,
// config stripped: the ones a clone can use.
func (l *Leaf) ListConnected(ctx context.Context, orgID string) ([]store.Connector, error) {
	cs, err := l.conns.ListByOrg(ctx, orgID)
	cs = slices.DeleteFunc(cs, func(c store.Connector) bool { return !Connected(c) })
	for i := range cs {
		cs[i].Config = ""
	}
	return cs, err
}

// Begin makes the pending row and returns where to POST the manifest.
// ghOrg: create the App under that GitHub org instead of the user.
func (l *Leaf) Begin(ctx context.Context, orgID, ghOrg string) (c store.Connector, action, manifest string, err error) {
	if _, err := l.conns.GetByHost(ctx, orgID, GitHubHost); err == nil {
		return c, "", "", errs.Conflictf("this org already has a %s connector", GitHubHost)
	}
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	cfg, _ := json.Marshal(config{State: hex.EncodeToString(nonce)})
	c = store.Connector{
		ID:        uuid.NewString(),
		OrgID:     orgID,
		Provider:  GitHub,
		Name:      "GitHub (connecting…)",
		Host:      GitHubHost,
		Config:    string(cfg),
		CreatedAt: time.Now().UTC(),
	}
	if action, manifest, err = l.app.Manifest(c.ID, ghOrg, c.ID+"."+hex.EncodeToString(nonce)); err != nil {
		return c, "", "", err
	}
	return c, action, manifest, l.conns.Create(ctx, c)
}

// Complete takes GitHub's callback. The state must match the pending row's
// nonce, or a stray callback could fill in someone else's row.
func (l *Leaf) Complete(ctx context.Context, state, code string) (store.Connector, error) {
	id, nonce, _ := strings.Cut(state, ".")
	c, err := l.conns.Get(ctx, id)
	if err != nil {
		return c, err
	}
	cfg := parse(c)
	if cfg.App != nil || cfg.State == "" || subtle.ConstantTimeCompare([]byte(cfg.State), []byte(nonce)) != 1 {
		return store.Connector{}, errs.Refusedf("this GitHub callback does not match a pending connector")
	}
	app, err := l.app.ConvertManifest(ctx, code)
	if err != nil {
		return c, err
	}
	b, _ := json.Marshal(config{App: &app})
	c.Config, c.Name = string(b), "GitHub · "+app.Slug
	return c, l.conns.Update(ctx, c)
}

func (l *Leaf) Rename(ctx context.Context, orgID, id, name string) (store.Connector, error) {
	c, err := l.Get(ctx, orgID, id)
	if err != nil {
		return c, err
	}
	if c.Name = strings.TrimSpace(name); c.Name == "" {
		return c, errs.Invalidf("name", "a connector needs a name")
	}
	return c, l.conns.Update(ctx, c)
}

func (l *Leaf) Delete(ctx context.Context, orgID, id string) error {
	if _, err := l.Get(ctx, orgID, id); err != nil {
		return err
	}
	return l.conns.Delete(ctx, id)
}

// Host is a git_url's host; only https URLs have one.
func Host(gitURL string) string {
	u, err := url.Parse(gitURL)
	if err != nil || u.Scheme != "https" {
		return ""
	}
	return strings.ToLower(u.Host)
}

// For resolves a tile's git_url to its org's connected connector.
func (l *Leaf) For(ctx context.Context, orgID, gitURL string) (store.Connector, error) {
	host := Host(gitURL)
	c, err := l.conns.GetByHost(ctx, orgID, host)
	if err != nil || !Connected(c) {
		if err == nil || errors.Is(err, errs.ErrNotFound) {
			return store.Connector{}, NoConnector{Host: host}
		}
		return c, err
	}
	return c, nil
}

// CloneEnv is the GIT_CONFIG_* environment that authenticates a fetch of
// gitURL through c. The token never lands in argv or .git/config.
func (l *Leaf) CloneEnv(ctx context.Context, c store.Connector, gitURL string) ([]string, error) {
	tok, err := l.Token(ctx, c)
	if err != nil {
		return nil, err
	}
	return githubapp.CloneAuth(gitURL, tok), nil
}

// Repos asks GitHub which repositories the app may read: the install check.
// Empty (no error) when the app is not installed anywhere yet, or installed
// with no repos picked; either way nothing can be cloned through it.
func (l *Leaf) Repos(ctx context.Context, orgID, id string) ([]githubapp.Repo, error) {
	c, err := l.Get(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	tok, err := l.Token(ctx, c)
	if errors.Is(err, githubapp.ErrNotInstalled) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return l.app.Repos(ctx, tok)
}

// Token is c's installation token (cached by the wrapper).
func (l *Leaf) Token(ctx context.Context, c store.Connector) (string, error) {
	app := parse(c).App
	if app == nil {
		return "", NoConnector{Host: c.Host}
	}
	return l.app.Token(ctx, c.ID, *app)
}

// WebhookSecret is the HMAC key for /hooks/connectors/<id>; "" (not
// connected) means the receiver answers 404 before reading the body.
func (l *Leaf) WebhookSecret(ctx context.Context, id string) (store.Connector, string, error) {
	c, err := l.conns.Get(ctx, id)
	if err != nil {
		return c, "", err
	}
	if app := parse(c).App; app != nil {
		return c, app.WebhookSecret, nil
	}
	return c, "", nil
}
