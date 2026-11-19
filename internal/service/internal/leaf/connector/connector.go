// Package connector owns connections: one row of a registered kind (an
// org's or the server's) and the provider account it stands for, opened
// through the kind. A git connection (the Git source capability) is one
// per org and host, or a server one (no org) the admin shares with named
// orgs or all; reads widen to the server ones shared with the org, writes
// never do: Get, Rename and Delete reach an org's own rows only. A redirect
// setup's row is pending until its provider calls back.
package connector

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/connectors"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type Leaf struct {
	conns store.ConnectorStore
	kinds []connectors.Kind
}

// New is the leaf over conns, opening connections of kinds.
func New(conns store.ConnectorStore, kinds []connectors.Kind) *Leaf {
	return &Leaf{conns: conns, kinds: kinds}
}

// NoConnector is a repo on a host that no connector of the org serves and
// nothing names or inherits: a typed error the promote diff shows.
type NoConnector struct{ Host string }

func (e NoConnector) Error() string {
	return "no connector for " + e.Host + ": name one, or set the organization's connector."
}

// Pending is a redirect setup out at its provider: the nonce its state
// carries, the user who began (only they may finish), and the panel path
// the browser goes back to.
type Pending struct {
	Nonce  string `json:"nonce"`
	User   string `json:"user"`
	Return string `json:"return,omitempty"`
}

// envelope is a pending row's settings: the setup, and the kind's blob as
// it stood when the browser left.
type envelope struct {
	Pending *Pending        `json:"pending"`
	Conn    json.RawMessage `json:"conn"`
}

// pendingOf is c's setup while it is pending, nil once connected.
func pendingOf(c store.Connector) (*Pending, json.RawMessage) {
	var env envelope
	if json.Unmarshal([]byte(c.Settings), &env) != nil || env.Pending == nil {
		return nil, nil
	}
	return env.Pending, env.Conn
}

// mark sets c.Pending from its settings, before a list strips them.
func mark(c store.Connector) store.Connector {
	p, _ := pendingOf(c)
	c.Pending = p != nil
	return c
}

// Connected says whether a connection's setup finished.
func Connected(c store.Connector) bool { p, _ := pendingOf(c); return p == nil && !c.Pending }

// open is c's kind and its connection, decoded from the settings.
func (l *Leaf) open(c store.Connector) (connectors.Kind, connectors.Conn, error) {
	i := slices.IndexFunc(l.kinds, func(k connectors.Kind) bool { return k.Name() == c.Kind })
	if i < 0 {
		return nil, nil, errs.ErrNotFound
	}
	conn := l.kinds[i].New()
	if p, _ := pendingOf(c); p != nil {
		return nil, nil, NoConnector{Host: c.Host}
	}
	if err := json.Unmarshal([]byte(c.Settings), conn); err != nil {
		return nil, nil, err
	}
	return l.kinds[i], conn, nil
}

// git is c's Git source capability.
func (l *Leaf) git(c store.Connector) (connectors.GitSource, error) {
	_, conn, err := l.open(c)
	if err != nil {
		return nil, err
	}
	g, ok := conn.(connectors.GitSource)
	if !ok {
		return nil, NoConnector{Host: c.Host}
	}
	return g, nil
}

// InstallURL is the provider's page for installing c on an account and
// picking its repos; "" while pending, or for a kind with none. Creating
// the app is not installing it: without an install there is no token to
// clone with. Reads the row itself, List strips the settings that hold it.
func (l *Leaf) InstallURL(ctx context.Context, orgID, id string) (string, error) {
	c, err := l.Get(ctx, orgID, id)
	if err != nil {
		return "", err
	}
	return l.Install(c, ""), nil
}

// ServerInstallURL is InstallURL for a server connector.
func (l *Leaf) ServerInstallURL(ctx context.Context, id string) (string, error) {
	c, err := l.Server(ctx, id)
	if err != nil {
		return "", err
	}
	return l.Install(c, ""), nil
}

// Install is c's install page, state carried back to its return path;
// "" while pending or for a kind with none.
func (l *Leaf) Install(c store.Connector, state string) string {
	_, conn, err := l.open(c)
	if err != nil {
		return ""
	}
	if i, ok := conn.(connectors.Installer); ok {
		return i.Install(state)
	}
	return ""
}

// Get refuses a connector of another org (as not found). The id can come
// from a stack row or a request, and without this a file naming another
// org's connector would clone that org's private repos with that org's
// token.
func (l *Leaf) Get(ctx context.Context, orgID, id string) (store.Connector, error) {
	c, err := l.conns.Get(ctx, id)
	if err != nil {
		return c, err
	}
	// A server connector (nil OrgID) is never an org's to write through
	// Get: orgs only read it, through Usable, ListConnected and For. A
	// connection without the git capability is reached through Of only.
	if !IsGit(c) {
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
	return mark(c), nil
}

// List is the org's git connectors, its own first, then the server
// connectors shared with it (Shared set: writes through Get still refuse
// them), settings stripped.
func (l *Leaf) List(ctx context.Context, orgID string) ([]store.Connector, error) {
	return l.list(ctx, orgID, false)
}

// ListConnected is List less the connectors whose setup has not finished.
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
		cs = slices.DeleteFunc(cs, func(c store.Connector) bool { return c.Pending })
	}
	for i := range cs {
		cs[i].Settings = ""
		cs[i].Shared = cs[i].OrgID == nil
	}
	return cs, nil
}

// gitOnly is the git connections of cs, marked.
func gitOnly(cs []store.Connector) []store.Connector {
	cs = slices.DeleteFunc(cs, func(c store.Connector) bool { return !IsGit(c) })
	for i := range cs {
		cs[i] = mark(cs[i])
	}
	return cs
}

// Usable is Get for reads: the org's own connector, or a server connector
// shared with the org. Anything else is not found. Never use it to write.
func (l *Leaf) Usable(ctx context.Context, orgID, id string) (store.Connector, error) {
	c, err := l.conns.Get(ctx, id)
	if err != nil {
		return c, err
	}
	if !IsGit(c) {
		return store.Connector{}, errs.ErrNotFound
	}
	if c.OrgID != nil && *c.OrgID == orgID {
		return mark(c), nil
	}
	if c.OrgID == nil {
		shared, err := l.conns.ListSharedWith(ctx, orgID)
		if err != nil {
			return store.Connector{}, err
		}
		if slices.ContainsFunc(shared, func(s store.Connector) bool { return s.ID == id }) {
			return mark(c), nil
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
	return mark(c), nil
}

// ListServer is the server's git connectors, settings stripped (other
// kinds: ListOf).
func (l *Leaf) ListServer(ctx context.Context) ([]store.Connector, error) {
	cs, err := l.conns.ListServer(ctx)
	cs = gitOnly(cs)
	for i := range cs {
		cs[i].Settings = ""
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
	if !IsGit(c) {
		return store.Connector{}, errs.Refusedf("only a git connector is shared; a DNS connector's managed domains each say who may use them")
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

// MakeServer moves org orgID's own git connector id up to the server,
// shared with that org. Nothing it holds changes, so no provider is asked.
func (l *Leaf) MakeServer(ctx context.Context, orgID, id string) (store.Connector, error) {
	c, err := l.Get(ctx, orgID, id)
	if err != nil {
		return c, err
	}
	if err := l.conns.MakeServer(ctx, id, orgID); err != nil {
		if _, ok := errs.IsConflict(err); ok {
			// the unique name, or a share row for this org that is already there
			if ss, lerr := l.conns.ListServer(ctx); lerr == nil && slices.ContainsFunc(ss, func(s store.Connector) bool { return s.Name == c.Name }) {
				return c, errs.Conflictf("A server connector named %q exists; rename this one first.", c.Name)
			}
			return c, errs.Conflictf("This connector already has a share row for the organization; remove it, then try again.")
		}
		return c, err
	}
	c.OrgID = nil
	return c, nil
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

// RenameAny renames connection id; the caller has checked its scope.
func (l *Leaf) RenameAny(ctx context.Context, id, name string) (store.Connector, error) {
	c, err := l.conns.Get(ctx, id)
	if err != nil {
		return c, err
	}
	return l.rename(ctx, mark(c), name)
}

func (l *Leaf) rename(ctx context.Context, c store.Connector, name string) (store.Connector, error) {
	if c.Name = strings.TrimSpace(name); c.Name == "" {
		return c, errs.Invalidf("name", "A connector needs a name")
	}
	if err := l.conns.SetName(ctx, c.ID, c.Name); err != nil {
		return c, err
	}
	c.Settings = ""
	return c, nil
}

func (l *Leaf) Delete(ctx context.Context, orgID, id string) error {
	if _, err := l.Get(ctx, orgID, id); err != nil {
		return err
	}
	return l.conns.Delete(ctx, id)
}

// SweepPending deletes the connections whose redirect setup began before
// cutoff and the provider never finished: a provider's sign-in walked
// away from. Nothing binds one: a pending connection has nothing to bind.
func (l *Leaf) SweepPending(ctx context.Context, cutoff time.Time) (int, error) {
	cs, err := l.conns.ListAll(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, c := range cs {
		if p, _ := pendingOf(c); p == nil || !c.CreatedAt.Before(cutoff) {
			continue
		}
		if err := l.conns.Delete(ctx, c.ID); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// UsableFor is Usable for a clone of gitURL: the connector must serve that
// URL's host, or the panel would authenticate and probe whatever host a
// caller names.
func (l *Leaf) UsableFor(ctx context.Context, orgID, id, gitURL string) (store.Connector, error) {
	c, err := l.Usable(ctx, orgID, id)
	if err != nil {
		return c, err
	}
	if h := Host(gitURL); h == "" || h != strings.ToLower(c.Host) {
		return store.Connector{}, errs.Invalidf("repo", "The URL is not on this connector's host.")
	}
	return c, nil
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

// OrgDefault is the connector id a stack or tile that names none inherits
// from its org. It is the org binding's connector. Open question (fraedi
// 5d5b4176, part 3): what an org with no binding connector inherits (a
// connector of its own, or the single one of the host). Only this function
// changes when that is answered.
func OrgDefault(og store.Org) string { return og.ConfigConnectorID }

// Pick is the connector a clone of a repo on host uses, from conns (an org's
// ListConnected: its own, then the server ones shared with it). A named id
// wins and, when it is not in conns, is NoConnector: it never falls through.
// Otherwise the first id of inherit that is in conns and serves host.
// Nothing is ever picked by host alone.
func Pick(conns []store.Connector, host, named string, inherit ...string) (store.Connector, error) {
	find := func(id string) int {
		return slices.IndexFunc(conns, func(c store.Connector) bool { return c.ID == id })
	}
	if named != "" {
		if i := find(named); i >= 0 {
			return conns[i], nil
		}
		return store.Connector{}, NoConnector{Host: host}
	}
	for _, id := range inherit {
		if i := find(id); id != "" && i >= 0 && conns[i].Host == host {
			return conns[i], nil
		}
	}
	return store.Connector{}, NoConnector{Host: host}
}

// ForStack is the connector the stack's own config repo clones through: the
// one it names, else the org's.
func ForStack(conns []store.Connector, og store.Org, st store.Stack) (store.Connector, error) {
	return Pick(conns, Host(st.ConfigRepo), st.ConfigConnectorID, OrgDefault(og))
}

// ForTile is the connector a tile's git_url clones through. A tile has no
// connector field: its stack's, when that serves the host, else the org's.
func ForTile(conns []store.Connector, og store.Org, st store.Stack, gitURL string) (store.Connector, error) {
	return Pick(conns, Host(gitURL), "", st.ConfigConnectorID, OrgDefault(og))
}

// CloneEnv is the environment that authenticates a fetch of gitURL
// through c. The credential never lands in argv or .git/config.
func (l *Leaf) CloneEnv(ctx context.Context, c store.Connector, gitURL string) ([]string, error) {
	g, err := l.git(c)
	if err != nil {
		return nil, err
	}
	return g.CloneEnv(ctx, gitURL)
}

// RepoURL is raw as the first of cs whose Git source owns it spells it;
// raw trimmed when none does.
func (l *Leaf) RepoURL(cs []store.Connector, raw string) string {
	raw = strings.TrimSpace(raw)
	for _, c := range cs {
		if g, err := l.git(c); err == nil {
			if u, ok := g.RepoURL(raw); ok {
				return u
			}
		}
	}
	return raw
}

// Gits is every connected git connection, settings kept: what a repo's
// spelling is asked of.
func (l *Leaf) Gits(ctx context.Context) ([]store.Connector, error) {
	cs, err := l.conns.ListAll(ctx)
	return slices.DeleteFunc(gitOnly(cs), func(c store.Connector) bool { return c.Pending }), err
}

// OrgOf is who owns connector id: its org, or nil for a server connector.
// For routing a browser a provider sent back; never an access check.
func (l *Leaf) OrgOf(ctx context.Context, id string) (*string, error) {
	c, err := l.conns.Get(ctx, id)
	return c.OrgID, err
}

// Repos asks the provider which repositories the connection may read: the
// install check. Empty (no error) when it is not installed anywhere yet, or
// installed with no repos picked; either way nothing can be cloned through
// it.
func (l *Leaf) Repos(ctx context.Context, orgID, id string) ([]connectors.Repo, error) {
	c, err := l.Usable(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	return l.repos(ctx, c)
}

// ServerRepos is Repos for a server connector.
func (l *Leaf) ServerRepos(ctx context.Context, id string) ([]connectors.Repo, error) {
	c, err := l.Server(ctx, id)
	if err != nil {
		return nil, err
	}
	return l.repos(ctx, c)
}

func (l *Leaf) repos(ctx context.Context, c store.Connector) ([]connectors.Repo, error) {
	if c.Pending {
		return nil, nil
	}
	g, err := l.git(c)
	if err != nil {
		return nil, err
	}
	return g.Repos(ctx)
}

// RepoBranches lists repo's branches through connector id, for the config
// picker.
func (l *Leaf) RepoBranches(ctx context.Context, orgID, id, repo string) ([]string, error) {
	c, err := l.Usable(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	g, err := l.git(c)
	if err != nil {
		return nil, err
	}
	return g.Branches(ctx, repo)
}

// RepoYAMLFiles lists repo's YAML files at ref through connector id, for
// the config file picker.
func (l *Leaf) RepoYAMLFiles(ctx context.Context, orgID, id, repo, ref string) ([]string, error) {
	c, err := l.Usable(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	g, err := l.git(c)
	if err != nil {
		return nil, err
	}
	return g.YAMLFiles(ctx, repo, ref)
}

// Hook checks and decodes a delivery to connection id. One that is not
// connected, or has no Git source, proves nothing: ErrBadSignature.
func (l *Leaf) Hook(ctx context.Context, id string, h http.Header, body []byte) (store.Connector, connectors.Event, error) {
	c, err := l.conns.Get(ctx, id)
	if err != nil {
		return c, connectors.Event{}, err
	}
	g, err := l.git(c)
	if errors.As(err, &NoConnector{}) || errors.Is(err, errs.ErrNotFound) {
		return c, connectors.Event{}, connectors.ErrBadSignature
	}
	if err != nil {
		return c, connectors.Event{}, err
	}
	ev, err := g.Hook(ctx, h, body)
	return c, ev, err
}
