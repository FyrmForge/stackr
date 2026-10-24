package service

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/connectors"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/orgconfig"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/serverconfig"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/connector"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/dnscap"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// kinds.go is the connectors core: the registered kinds (connectors/all),
// a connection's setup drawn from its kind's steps, its settings blob, and
// the redirect out to a provider and back through ConnectorCallbackPath,
// then on through the provider's install and back to ConnectorReturnPath.
// Features ask for a capability (dnsOf, the git verbs), never a kind.

// A kind's setup, as the panel's add dialog and the wizard draw it.
type (
	ConnectorStep  = connectors.Step
	ConnectorField = connectors.Field
	ConnectorOut   = connectors.Out
	ConnectorAlt   = connectors.Alt
)

// Capabilities, as a connection lists them.
const (
	CapDNS = connectors.CapDNS
	CapGit = connectors.CapGit
)

// ConnectorCallbackPath is the one route a redirect setup comes back to.
const ConnectorCallbackPath = connectors.CallbackPath

// ConnectorReturnPath is the route pattern a provider sends the browser
// back to after a step on its own site (connectors.ReturnPath);
// ConnectorLegacyBack the callback path a kind's apps were registered
// with before the core had one, which hands off to the core's.
const (
	ConnectorReturnPath = "/settings/:kind/installed/:connector"
	ConnectorLegacyBack = "/settings/:kind/callback"
)

// ConnectorKind is a registered kind. Redirects: its setup signs in at the
// provider, so only the panel can add one. Alt: its second way to set up
// (nil = none).
type ConnectorKind struct {
	Name      string          `json:"name"`
	Label     string          `json:"label"`
	Hint      string          `json:"hint"`
	Caps      []string        `json:"capabilities"`
	Steps     []ConnectorStep `json:"steps"`
	Redirects bool            `json:"redirects"`
	Alt       *ConnectorAlt   `json:"alt,omitempty"`
}

func kindOf(k connectors.Kind) ConnectorKind {
	out := ConnectorKind{
		Name: k.Name(), Label: k.Label(), Hint: k.Hint(),
		Caps: connectors.Caps(k), Steps: k.Steps(), Redirects: connectors.Redirects(k),
	}
	if a, ok := k.(connectors.Alter); ok {
		alt := a.Alt()
		out.Alt = &alt
	}
	return out
}

// ConnectorKinds is every registered kind, in the add dialog's order.
func (o *Orchestrator) ConnectorKinds() []ConnectorKind {
	out := make([]ConnectorKind, len(o.kinds))
	for i, k := range o.kinds {
		out[i] = kindOf(k)
	}
	return out
}

func (o *Orchestrator) kind(name string) (connectors.Kind, error) {
	i := slices.IndexFunc(o.kinds, func(k connectors.Kind) bool { return k.Name() == name })
	if i < 0 {
		return nil, errs.ErrNotFound
	}
	return o.kinds[i], nil
}

// ConnectorKindOf is kind name; not registered is not found.
func (o *Orchestrator) ConnectorKindOf(name string) (ConnectorKind, error) {
	k, err := o.kind(name)
	if err != nil {
		return ConnectorKind{}, err
	}
	return kindOf(k), nil
}

// ConnectorPrefill is what field f starts with: the fact it asks for, ""
// for none.
func (o *Orchestrator) ConnectorPrefill(ctx context.Context, f ConnectorField) string {
	if f.Prefill == connectors.ServerIPv4 {
		return o.serverIPv4(ctx)
	}
	return ""
}

// ConnectorAdd is a setup's form values for a connection of Kind, of OrgID
// (nil = the server's); Alt: the values are the kind's second way's. A
// redirect setup also needs User, who must be the one to come back, and
// Return, the panel path it comes back to.
type ConnectorAdd struct {
	OrgID        *string
	Kind         string
	Alt          bool
	Values       map[string]string
	User, Return string
}

// ConnectorAdded is a saved connection and, for a DNS one, its account's
// domains. Out set: the connection waits pending, the browser goes there.
type ConnectorAdded struct {
	Connector Connector     `json:"connector"`
	Domains   []DNSDomain   `json:"domains,omitempty"`
	Out       *ConnectorOut `json:"-"`
}

// AddConnector checks a setup's values by its kind's rules (a DNS one's
// account must list its domains) and saves the connection, or for a
// redirect setup saves it pending and sends the browser to the provider.
// An empty field that asks for a prefill takes it. A server git
// connection is shared with every org from the start (the admin narrows
// it after).
func (o *Orchestrator) AddConnector(ctx context.Context, in ConnectorAdd) (ConnectorAdded, error) {
	k, err := o.kind(in.Kind)
	if err != nil {
		return ConnectorAdded{}, err
	}
	steps := k.Steps()
	if in.Alt {
		a, ok := k.(connectors.Alter)
		if !ok {
			return ConnectorAdded{}, errs.ErrNotFound
		}
		steps = a.Alt().Steps
	}
	vals := map[string]string{}
	for _, s := range steps {
		for _, f := range s.Fields {
			if vals[f.Name] = in.Values[f.Name]; vals[f.Name] == "" {
				vals[f.Name] = o.ConnectorPrefill(ctx, f)
			}
		}
	}
	conn := k.New()
	if err := conn.Setup(vals); err != nil {
		return ConnectorAdded{}, err
	}
	if s, ok := conn.(connectors.Settled); connectors.Redirects(k) && (!ok || !s.Settled()) {
		c, out, err := o.beginSetup(ctx, in, k, conn)
		return ConnectorAdded{Connector: c, Out: out}, err
	}
	var zones []dnscap.Zone
	if slices.Contains(connectors.Caps(k), CapDNS) {
		p, err := o.dnsConn(k, conn)
		if err != nil {
			return ConnectorAdded{}, err
		}
		if zones, err = p.Zones(ctx); err != nil {
			return ConnectorAdded{}, err
		}
	}
	b, err := json.Marshal(conn)
	if err != nil {
		return ConnectorAdded{}, err
	}
	c, err := o.conns.Add(ctx, in.OrgID, k.Name(), k.Label(), conn.Name(), hostOf(conn), connectors.Caps(k), string(b))
	if err != nil {
		return ConnectorAdded{}, err
	}
	if c, err = o.shareNew(ctx, c); err != nil {
		return ConnectorAdded{}, err
	}
	out := ConnectorAdded{Connector: c}
	if zones != nil {
		out.Domains = dnscap.States(zones)
	}
	return out, nil
}

// hostOf is the host a git connection serves; "" (the kind) for others.
func hostOf(conn connectors.Conn) string {
	if g, ok := conn.(connectors.GitSource); ok {
		return g.Host()
	}
	return ""
}

// shareNew shares a new server git connection with every org.
func (o *Orchestrator) shareNew(ctx context.Context, c store.Connector) (store.Connector, error) {
	if c.OrgID != nil || !connector.IsGit(c) {
		return c, nil
	}
	pending := c.Pending
	c, err := o.conns.SetShares(ctx, c.ID, nil, true)
	c.Pending, c.Settings = pending, ""
	return c, err
}

// pendingTTL is how long a redirect setup waits for the provider; the
// sweep removes one older.
const pendingTTL = time.Hour

// beginSetup saves conn pending, named "<kind> (connecting…)" until the
// provider answers, and returns where the browser goes.
func (o *Orchestrator) beginSetup(ctx context.Context, in ConnectorAdd, k connectors.Kind, conn connectors.Conn) (Connector, *ConnectorOut, error) {
	if in.User == "" || !local(in.Return) {
		return Connector{}, nil, errs.Refusedf("%s signs in on its own site: add it in the panel", k.Label())
	}
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	p := connector.Pending{Nonce: hex.EncodeToString(nonce), User: in.User, Return: in.Return}
	id := uuid.NewString()
	out, err := conn.(connectors.Redirector).Out(ctx, id, o.appBase(ctx)+ConnectorCallbackPath, id+"."+p.Nonce)
	if err != nil {
		return Connector{}, nil, err
	}
	b, err := json.Marshal(conn)
	if err != nil {
		return Connector{}, nil, err
	}
	c := store.Connector{
		ID: id, OrgID: in.OrgID, Kind: k.Name(), Name: k.Label() + " (connecting…)", Host: hostOf(conn),
		CreatedAt: time.Now().UTC(), Capabilities: connectors.Caps(k),
	}
	if in.OrgID == nil { // server connection names are unique
		c.Name = k.Label() + " (connecting… " + id[:8] + ")"
	}
	if c, err = o.conns.Pend(ctx, c, k.Label(), p, string(b)); err != nil {
		return Connector{}, nil, err
	}
	c, err = o.shareNew(ctx, c)
	return c, &out, err
}

// local: a panel path, never another site.
func local(p string) bool {
	return strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "/\\")
}

// ConnectorBack takes a provider's callback: the state must match a
// pending setup's and userID must be who began it, or a stray callback
// could finish someone else's. It saves the connection and returns where
// the browser goes: the provider's install page for a kind installed
// there (which brings it on to the panel path the setup began on), else
// that path ("" for a setup begun before one was kept).
func (o *Orchestrator) ConnectorBack(ctx context.Context, userID string, q url.Values) (string, error) {
	id, nonce, _ := strings.Cut(q.Get("state"), ".")
	refused := errs.Refusedf("this callback does not match a connector being set up; start again")
	c, p, blob, err := o.conns.Pending(ctx, id)
	if errors.Is(err, errs.ErrNotFound) {
		return "", refused
	}
	if err != nil {
		return "", err
	}
	if p.User != userID || time.Since(c.CreatedAt) > pendingTTL || subtle.ConstantTimeCompare([]byte(p.Nonce), []byte(nonce)) != 1 {
		return "", refused
	}
	k, err := o.kind(c.Kind)
	if err != nil {
		return "", err
	}
	conn := k.New()
	if err := json.Unmarshal([]byte(blob), conn); err != nil {
		return "", err
	}
	r, ok := conn.(connectors.Redirector)
	if !ok {
		return "", refused
	}
	if err := r.Back(ctx, q); err != nil {
		return "", err
	}
	b, err := json.Marshal(conn)
	if err != nil {
		return "", err
	}
	if err := o.conns.Settle(ctx, id, conn.Name(), string(b)); err != nil {
		return "", err
	}
	if i, ok := conn.(connectors.Installer); ok {
		return i.Install(o.ReturnState(p.Return)), nil
	}
	return p.Return, nil
}

// ReturnState is the state a provider carries back to ConnectorReturnPath
// so the browser lands on path: path and its MAC, so a link cannot send
// it elsewhere. "" for no path.
func (o *Orchestrator) ReturnState(path string) string {
	if !local(path) {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(path)) + "." + o.returnMAC(path)
}

// ReturnTo is the panel path a ReturnState stands for; false when it is
// none (a provider's later visit carries none).
func (o *Orchestrator) ReturnTo(state string) (string, bool) {
	enc, mac, _ := strings.Cut(state, ".")
	b, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil || !local(string(b)) || !hmac.Equal([]byte(mac), []byte(o.returnMAC(string(b)))) {
		return "", false
	}
	return string(b), true
}

func (o *Orchestrator) returnMAC(path string) string {
	m := hmac.New(sha256.New, []byte(o.cfg.SecretsKey))
	m.Write([]byte("connector-return\x00" + path))
	return hex.EncodeToString(m.Sum(nil))[:32]
}

// appBase is the panel's URL as a provider must reach it: BaseURL with its
// host swapped for the panel_domain setting when one is set (scheme and
// port kept), so a connection made after the panel moved does not register
// the old host.
func (o *Orchestrator) appBase(ctx context.Context) string {
	host, _ := o.settings.Get(ctx, "panel_domain")
	return withHost(o.cfg.BaseURL, host)
}

func withHost(base, host string) string {
	base = strings.TrimRight(base, "/")
	u, err := url.Parse(base)
	if host == "" || err != nil || u.Host == "" {
		return base
	}
	if p := u.Port(); p != "" {
		host += ":" + p
	}
	u.Host = host
	return strings.TrimRight(u.String(), "/")
}

// ConnectorsAt is orgID's connections of registered kinds (nil = the
// server's), settings stripped; none still pending.
func (o *Orchestrator) ConnectorsAt(ctx context.Context, orgID *string) ([]Connector, error) {
	return o.conns.ListOf(ctx, orgID, "", false)
}

// ConnectorRows is ConnectorsAt with the pending ones (Pending set), for
// a list that shows them waiting.
func (o *Orchestrator) ConnectorRows(ctx context.Context, orgID *string) ([]Connector, error) {
	return o.conns.ListOf(ctx, orgID, "", true)
}

// ConnectorAt is one of them, settings stripped.
func (o *Orchestrator) ConnectorAt(ctx context.Context, orgID *string, id string) (Connector, error) {
	c, err := o.conns.Of(ctx, orgID, id)
	c.Settings = ""
	return c, err
}

// RemoveConnector removes one of them, each capability cleaning up after
// itself first: a DNS one's managed domains as UnmanageDNSDomain does,
// refused while a tile uses one; a server git one refused while a binding
// names it.
func (o *Orchestrator) RemoveConnector(ctx context.Context, orgID *string, id string) error {
	c, err := o.conns.Of(ctx, orgID, id)
	if err != nil {
		return err
	}
	if connector.Has(c, CapGit) && orgID == nil {
		if err := o.unbound(ctx, id); err != nil {
			return err
		}
	}
	if connector.Has(c, CapDNS) {
		if err := o.forgetAll(ctx, c); err != nil {
			return err
		}
		defer o.dnsChanged(ctx)
	}
	return o.conns.Remove(ctx, orgID, id)
}

// open is connection c's kind and its settings decoded.
func (o *Orchestrator) open(c store.Connector) (connectors.Kind, connectors.Conn, error) {
	k, err := o.kind(c.Kind)
	if err != nil {
		return nil, nil, err
	}
	conn := k.New()
	if err := json.Unmarshal([]byte(c.Settings), conn); err != nil {
		return nil, nil, err
	}
	return k, conn, nil
}

// dnsOf is connection c's DNS capability.
func (o *Orchestrator) dnsOf(c store.Connector) (dnscap.Provider, error) {
	k, conn, err := o.open(c)
	if err != nil {
		return nil, err
	}
	return o.dnsConn(k, conn)
}

// dnsConn is conn as a DNS provider; WithDNS's fake when one is set.
func (o *Orchestrator) dnsConn(k connectors.Kind, conn connectors.Conn) (dnscap.Provider, error) {
	if o.dnsOpen != nil {
		var m DNSCreds
		b, _ := json.Marshal(conn)
		_ = json.Unmarshal(b, &m)
		return o.dnsOpen(k.Name(), m)
	}
	p, ok := conn.(connectors.DNS)
	if !ok {
		return nil, errs.ErrNotFound
	}
	return p, nil
}

// label is kind's label, the kind itself when not registered.
func (o *Orchestrator) label(kind string) string {
	if k, err := o.kind(kind); err == nil {
		return k.Label()
	}
	return kind
}

// repoURL is raw in the one stored spelling of a repo: the spelling of the
// connection that owns it, else of a kind that would; raw trimmed when
// none does. A repo no connection owns is refused where it is cloned.
func (o *Orchestrator) repoURL(ctx context.Context, raw string) string {
	cs, _ := o.conns.Gits(ctx)
	if u := o.conns.RepoURL(cs, raw); u != strings.TrimSpace(raw) {
		return u
	}
	for _, k := range o.kinds {
		if g, ok := k.New().(connectors.GitSource); ok {
			if u, ok := g.RepoURL(strings.TrimSpace(raw)); ok {
				return u
			}
		}
	}
	return strings.TrimSpace(raw)
}

// spell is repoURL for a file's flows, which run without a context.
func (o *Orchestrator) spell(ctx context.Context) func(string) string {
	return func(raw string) string { return o.repoURL(ctx, raw) }
}

// parseOrgFile is an org config file parsed, its repos spelled by the
// connections that own them.
func (o *Orchestrator) parseOrgFile(ctx context.Context, data []byte) (*orgconfig.File, error) {
	f, err := orgconfig.Parse(data)
	if err == nil {
		f.RepoURLs(o.spell(ctx))
	}
	return f, err
}

// parseServerFile is parseOrgFile for the server config file.
func (o *Orchestrator) parseServerFile(ctx context.Context, data []byte) (*serverconfig.File, error) {
	f, err := serverconfig.Parse(data)
	if err == nil {
		f.RepoURLs(o.spell(ctx))
	}
	return f, err
}
