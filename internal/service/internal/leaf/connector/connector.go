// Package connector owns git connectors: one GitHub App per org and host,
// or a server connector (no org) the admin shares with named orgs or all.
// It runs the connect handshake (pending row, state check, manifest
// conversion), resolves a git_url to a connector, and mints the clone
// credential. Reads widen to the server connectors shared with the org;
// writes never do: Get, Rename and Delete reach an org's own rows only.
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
	Branches(ctx context.Context, token, repo string) ([]string, error)
	YAMLFiles(ctx context.Context, token, repo, ref string) ([]string, error)
	SetHookURL(ctx context.Context, connectorID string, app githubapp.App) error
	HookURL(connectorID string) string
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

// Ambiguous is a git_url whose host several shared server connectors serve,
// and the org has none of its own: the binding must name one.
type Ambiguous struct {
	Host  string
	Names []string
}

func (e Ambiguous) Error() string {
	return "several shared git connectors serve " + e.Host + " (" + strings.Join(e.Names, ", ") + "); name the connector to use"
}

// config is the encrypted connectors.config: the nonce and the user who
// began while pending, the App once connected.
type config struct {
	State string         `json:"state,omitempty"`
	User  string         `json:"user,omitempty"`
	App   *githubapp.App `json:"app,omitempty"`
	DNS   *DNS           `json:"dns,omitempty"`
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
	return installURL(c), nil
}

// ServerInstallURL is InstallURL for a server connector.
func (l *Leaf) ServerInstallURL(ctx context.Context, id string) (string, error) {
	c, err := l.Server(ctx, id)
	if err != nil {
		return "", err
	}
	return installURL(c), nil
}

func installURL(c store.Connector) string {
	app := parse(c).App
	if app == nil {
		return ""
	}
	return "https://github.com/apps/" + app.Slug + "/installations/new"
}

// Get refuses a connector of another org (as not found). The id can come from a stack row
// or a request, and without this a file naming another org's connector
// would clone that org's private repos with that org's token.
func (l *Leaf) Get(ctx context.Context, orgID, id string) (store.Connector, error) {
	c, err := l.conns.Get(ctx, id)
	if err != nil {
		return c, err
	}
	// A server connector (nil OrgID) is never an org's to write through
	// Get: orgs only read it, through Usable, ListConnected and For. A DNS
	// connector is an owner's, reached through DNSConnector only.
	if IsDNS(c) {
		return store.Connector{}, errs.ErrNotFound
	}
	if c.OrgID == nil || *c.OrgID != orgID {
		owner := "server"
		if c.OrgID != nil {
			owner = *c.OrgID
		}
		slog.Error("connector requested by another org", "connector", id, "owner", owner, "org", orgID)
		return store.Connector{}, errs.ErrNotFound // not yours = not there
	}
	return c, nil
}

// List is the org's connectors, its own first, then the server connectors
// shared with it (Shared set: writes through Get still refuse them),
// config stripped.
func (l *Leaf) List(ctx context.Context, orgID string) ([]store.Connector, error) {
	return l.list(ctx, orgID, false)
}

// ListConnected is List less the connectors that have not finished
// GitHub's handshake.
func (l *Leaf) ListConnected(ctx context.Context, orgID string) ([]store.Connector, error) {
	return l.list(ctx, orgID, true)
}

func (l *Leaf) list(ctx context.Context, orgID string, connected bool) ([]store.Connector, error) {
	own, err := l.conns.ListByOrg(ctx, orgID)
	if err != nil {
		return nil, err
	}
	shared, err := l.conns.ListSharedWith(ctx, orgID)
	if err != nil {
		return nil, err
	}
	cs := gitOnly(slices.Concat(own, shared))
	if connected {
		cs = slices.DeleteFunc(cs, func(c store.Connector) bool { return !Connected(c) })
	}
	for i := range cs {
		cs[i].Config = ""
		cs[i].Shared = cs[i].OrgID == nil
	}
	return cs, nil
}

// Usable is Get for reads: the org's own connector, or a server connector
// shared with the org. Anything else is not found. Never use it to write.
func (l *Leaf) Usable(ctx context.Context, orgID, id string) (store.Connector, error) {
	c, err := l.conns.Get(ctx, id)
	if err != nil {
		return c, err
	}
	if IsDNS(c) {
		return store.Connector{}, errs.ErrNotFound
	}
	if c.OrgID != nil && *c.OrgID == orgID {
		return c, nil
	}
	if c.OrgID == nil {
		shared, err := l.conns.ListSharedWith(ctx, orgID)
		if err != nil {
			return store.Connector{}, err
		}
		if slices.ContainsFunc(shared, func(s store.Connector) bool { return s.ID == id }) {
			return c, nil
		}
	}
	slog.Error("connector requested by another org", "connector", id, "org", orgID)
	return store.Connector{}, errs.ErrNotFound
}

// Server is a server connector by id; an org's row is not found.
func (l *Leaf) Server(ctx context.Context, id string) (store.Connector, error) {
	c, err := l.conns.Get(ctx, id)
	if err != nil {
		return c, err
	}
	if c.OrgID != nil {
		return store.Connector{}, errs.ErrNotFound
	}
	return c, nil
}

// ListServer is the server's git connectors, config stripped (DNS ones:
// ListDNS).
func (l *Leaf) ListServer(ctx context.Context) ([]store.Connector, error) {
	cs, err := l.conns.ListServer(ctx)
	cs = gitOnly(cs)
	for i := range cs {
		cs[i].Config = ""
	}
	return cs, err
}

// SharedOrgs is the org ids a server connector is shared with by name.
func (l *Leaf) SharedOrgs(ctx context.Context, id string) ([]string, error) {
	return l.conns.ShareOrgs(ctx, id)
}

// SetShares sets who may read a server connector: all orgs (all wins and
// clears the named ones), or exactly orgIDs; none when both are empty. The
// caller has checked the ids name orgs.
func (l *Leaf) SetShares(ctx context.Context, id string, orgIDs []string, all bool) (store.Connector, error) {
	c, err := l.Server(ctx, id)
	if err != nil {
		return c, err
	}
	if IsDNS(c) {
		return store.Connector{}, errs.Refusedf("a DNS connector is not shared; each managed domain says who may use it")
	}
	if all {
		orgIDs = nil
	}
	if err := l.conns.SetShares(ctx, id, orgIDs); err != nil {
		return c, err
	}
	c.ShareAll = all
	return c, l.conns.SetShareAll(ctx, id, all)
}

// Begin makes the pending row and returns where to POST the manifest.
// ghOrg: create the App under that GitHub org instead of the user. userID is
// who started it; only they can complete it.
func (l *Leaf) Begin(ctx context.Context, orgID, userID, ghOrg string) (c store.Connector, action, manifest string, err error) {
	if _, err := l.conns.GetByHost(ctx, orgID, GitHubHost); err == nil {
		return c, "", "", errs.Conflictf("This org already has a %s connector", GitHubHost)
	}
	return l.begin(ctx, &orgID, userID, ghOrg)
}

// BeginServer is Begin for a server connector. A server may hold several
// per host, so there is no host check; the pending name carries the id
// because server connector names are unique.
func (l *Leaf) BeginServer(ctx context.Context, userID, ghOrg string) (store.Connector, string, string, error) {
	return l.begin(ctx, nil, userID, ghOrg)
}

func (l *Leaf) begin(ctx context.Context, orgID *string, userID, ghOrg string) (c store.Connector, action, manifest string, err error) {
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	cfg, _ := json.Marshal(config{State: hex.EncodeToString(nonce), User: userID})
	c = store.Connector{
		ID:        uuid.NewString(),
		OrgID:     orgID,
		Provider:  GitHub,
		Name:      "GitHub (connecting…)",
		Host:      GitHubHost,
		Config:    string(cfg),
		CreatedAt: time.Now().UTC(),

		Capabilities: store.StringList{CapGit},
	}
	if orgID == nil {
		c.Name = "GitHub (connecting… " + c.ID[:8] + ")"
	}
	if action, manifest, err = l.app.Manifest(c.ID, ghOrg, c.ID+"."+hex.EncodeToString(nonce)); err != nil {
		return c, "", "", err
	}
	return c, action, manifest, l.conns.Create(ctx, c)
}

// AddServer saves a GitHub App made outside stackr as a server connector
// shared with every org. The key must parse: a bad one would only fail at
// the first clone.
func (l *Leaf) AddServer(ctx context.Context, app githubapp.App) (store.Connector, error) {
	app.Slug, app.WebhookSecret = strings.TrimSpace(app.Slug), strings.TrimSpace(app.WebhookSecret)
	switch {
	case app.ID <= 0:
		return store.Connector{}, errs.Invalidf("app_id", "The App ID is the number on the App's page")
	case app.Slug == "":
		return store.Connector{}, errs.Invalidf("slug", "The App's slug is required")
	case app.WebhookSecret == "":
		return store.Connector{}, errs.Invalidf("webhook_secret", "The webhook secret is required")
	}
	if _, err := githubapp.ParseKey(app.PEM); err != nil {
		return store.Connector{}, errs.Invalidf("private_key", "The private key does not parse. Paste the whole .pem file")
	}
	b, _ := json.Marshal(config{App: &app})
	c := store.Connector{
		ID:        uuid.NewString(),
		Provider:  GitHub,
		Name:      "GitHub · " + app.Slug,
		Host:      GitHubHost,
		Config:    string(b),
		CreatedAt: time.Now().UTC(),

		Capabilities: store.StringList{CapGit},
	}
	if err := l.conns.Create(ctx, c); err != nil {
		return store.Connector{}, err
	}
	return l.SetShares(ctx, c.ID, nil, true)
}

// HookURL is where GitHub must send connector id's webhooks.
func (l *Leaf) HookURL(id string) string { return l.app.HookURL(id) }

// Complete takes GitHub's callback. The state must match the pending row's
// nonce and the caller must be the user who began, or a stray callback could
// fill in someone else's row.
func (l *Leaf) Complete(ctx context.Context, userID, state, code string) (store.Connector, error) {
	id, nonce, _ := strings.Cut(state, ".")
	c, err := l.conns.Get(ctx, id)
	if err != nil {
		return c, err
	}
	cfg := parse(c)
	if cfg.App != nil || cfg.State == "" || cfg.User != userID || subtle.ConstantTimeCompare([]byte(cfg.State), []byte(nonce)) != 1 {
		return store.Connector{}, errs.Refusedf("this GitHub callback does not match a pending connector")
	}
	app, err := l.app.ConvertManifest(ctx, code)
	if err != nil {
		return c, err
	}
	b, _ := json.Marshal(config{App: &app})
	c.Config, c.Name = string(b), "GitHub · "+app.Slug
	return c, l.conns.SetApp(ctx, c.ID, c.Name, c.Config)
}

func (l *Leaf) Rename(ctx context.Context, orgID, id, name string) (store.Connector, error) {
	c, err := l.Get(ctx, orgID, id)
	if err != nil {
		return c, err
	}
	return l.rename(ctx, c, name)
}

// RenameServer renames a server connector; names are unique among them.
func (l *Leaf) RenameServer(ctx context.Context, id, name string) (store.Connector, error) {
	c, err := l.Server(ctx, id)
	if err != nil {
		return c, err
	}
	return l.rename(ctx, c, name)
}

func (l *Leaf) rename(ctx context.Context, c store.Connector, name string) (store.Connector, error) {
	if c.Name = strings.TrimSpace(name); c.Name == "" {
		return c, errs.Invalidf("name", "A connector needs a name")
	}
	if err := l.conns.SetName(ctx, c.ID, c.Name); err != nil {
		return c, err
	}
	c.Config = ""
	return c, nil
}

func (l *Leaf) Delete(ctx context.Context, orgID, id string) error {
	if _, err := l.Get(ctx, orgID, id); err != nil {
		return err
	}
	return l.conns.Delete(ctx, id)
}

// SweepPending deletes the GitHub connectors whose handshake began before
// cutoff and GitHub never finished: a "Create on GitHub" walked away from.
// Nothing binds one: a pending connector has no App to bind.
func (l *Leaf) SweepPending(ctx context.Context, cutoff time.Time) (int, error) {
	cs, err := l.conns.ListAll(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, c := range cs {
		if cfg := parse(c); c.Provider != GitHub || cfg.App != nil || cfg.State == "" || !c.CreatedAt.Before(cutoff) {
			continue
		}
		if err := l.conns.Delete(ctx, c.ID); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// DeleteServer deletes a server connector and, by cascade, its shares. The
// caller has checked nothing binds it.
func (l *Leaf) DeleteServer(ctx context.Context, id string) error {
	if _, err := l.Server(ctx, id); err != nil {
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

// For resolves a tile's git_url to a connected connector of the org, by
// Resolve's rule.
func (l *Leaf) For(ctx context.Context, orgID, gitURL string) (store.Connector, error) {
	cs, err := l.ListConnected(ctx, orgID)
	if err != nil {
		return store.Connector{}, err
	}
	c, err := Resolve(cs, "", Host(gitURL))
	if err != nil {
		return store.Connector{}, err
	}
	// ListConnected strips the config the token mint needs.
	return l.conns.Get(ctx, c.ID)
}

// Resolve picks the connector a clone of a repo on host uses, from conns (an
// org's ListConnected: its connected ones, then the server connectors shared
// with it): the one named by id wins; else the org's own for the host; else exactly one
// shared server connector. None is NoConnector, several Ambiguous.
func Resolve(conns []store.Connector, named, host string) (store.Connector, error) {
	if named != "" {
		if i := slices.IndexFunc(conns, func(c store.Connector) bool { return c.ID == named }); i >= 0 {
			return conns[i], nil
		}
		return store.Connector{}, NoConnector{Host: host}
	}
	on := func(own bool) []store.Connector {
		return slices.DeleteFunc(slices.Clone(conns), func(c store.Connector) bool {
			return c.Host != host || (c.OrgID != nil) != own
		})
	}
	if own := on(true); len(own) > 0 {
		return own[0], nil
	}
	switch shared := on(false); len(shared) {
	case 0:
		return store.Connector{}, NoConnector{Host: host}
	case 1:
		return shared[0], nil
	default:
		names := make([]string, len(shared))
		for i, c := range shared {
			names[i] = c.Name
		}
		return store.Connector{}, Ambiguous{Host: host, Names: names}
	}
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

// OrgOf is who owns connector id: its org, or nil for a server connector.
// For routing a browser GitHub sent back; never an access check.
func (l *Leaf) OrgOf(ctx context.Context, id string) (*string, error) {
	c, err := l.conns.Get(ctx, id)
	return c.OrgID, err
}

// Repos asks GitHub which repositories the app may read: the install check.
// Empty (no error) when the app is not installed anywhere yet, or installed
// with no repos picked; either way nothing can be cloned through it.
func (l *Leaf) Repos(ctx context.Context, orgID, id string) ([]githubapp.Repo, error) {
	c, err := l.Usable(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	return l.repos(ctx, c)
}

// ServerRepos is Repos for a server connector.
func (l *Leaf) ServerRepos(ctx context.Context, id string) ([]githubapp.Repo, error) {
	c, err := l.Server(ctx, id)
	if err != nil {
		return nil, err
	}
	return l.repos(ctx, c)
}

func (l *Leaf) repos(ctx context.Context, c store.Connector) ([]githubapp.Repo, error) {
	tok, err := l.Token(ctx, c)
	if errors.Is(err, githubapp.ErrNotInstalled) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return l.app.Repos(ctx, tok)
}

// RepoBranches lists repo's branches through connector id, for the config
// picker.
func (l *Leaf) RepoBranches(ctx context.Context, orgID, id, repo string) ([]string, error) {
	c, err := l.Usable(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	tok, err := l.Token(ctx, c)
	if err != nil {
		return nil, err
	}
	return l.app.Branches(ctx, tok, repo)
}

// RepoYAMLFiles lists repo's YAML files at ref through connector id, for
// the config file picker.
func (l *Leaf) RepoYAMLFiles(ctx context.Context, orgID, id, repo, ref string) ([]string, error) {
	c, err := l.Usable(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	tok, err := l.Token(ctx, c)
	if err != nil {
		return nil, err
	}
	return l.app.YAMLFiles(ctx, tok, repo, ref)
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
