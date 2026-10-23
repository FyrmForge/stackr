package service

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/connectors"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/connector"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/dnscap"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// kinds.go is the connectors core: the registered kinds (connectors/all),
// a connection's setup drawn from its kind's steps, its settings blob, and
// the redirect out to a provider and back through ConnectorCallbackPath.
// Features ask for a capability (dnsOf), never a kind. GitHub stays on
// connector.go and org.go until it moves onto the Git source capability.

// A kind's setup, as the panel's add dialog and the wizard draw it.
type (
	ConnectorStep  = connectors.Step
	ConnectorField = connectors.Field
	ConnectorOut   = connectors.Out
)

// Capabilities, as a connection lists them.
const (
	CapDNS = connectors.CapDNS
	CapGit = connectors.CapGit
)

// ConnectorCallbackPath is the one route a redirect setup comes back to.
const ConnectorCallbackPath = "/-/connectors/callback"

// ConnectorKind is a registered kind. Redirects: its setup signs in at the
// provider, so only the panel can add one.
type ConnectorKind struct {
	Name      string          `json:"name"`
	Label     string          `json:"label"`
	Hint      string          `json:"hint"`
	Caps      []string        `json:"capabilities"`
	Steps     []ConnectorStep `json:"steps"`
	Redirects bool            `json:"redirects"`
}

func kindOf(k connectors.Kind) ConnectorKind {
	return ConnectorKind{
		Name: k.Name(), Label: k.Label(), Hint: k.Hint(),
		Caps: connectors.Caps(k), Steps: k.Steps(), Redirects: connectors.Redirects(k),
	}
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
// (nil = the server's). A redirect setup also needs User, who must be the
// one to come back, and Return, the panel path it comes back to.
type ConnectorAdd struct {
	OrgID        *string
	Kind         string
	Values       map[string]string
	User, Return string
}

// ConnectorAdded is a saved connection and, for a DNS one, its account's
// domains. Out set: nothing is saved yet, the browser goes there.
type ConnectorAdded struct {
	Connector Connector     `json:"connector"`
	Domains   []DNSDomain   `json:"domains,omitempty"`
	Out       *ConnectorOut `json:"-"`
}

// AddConnector checks a setup's values by its kind's rules (a DNS one's
// account must list its domains) and saves the connection, or for a
// redirect setup sends the browser to the provider. An empty field that
// asks for a prefill takes it.
func (o *Orchestrator) AddConnector(ctx context.Context, in ConnectorAdd) (ConnectorAdded, error) {
	k, err := o.kind(in.Kind)
	if err != nil {
		return ConnectorAdded{}, err
	}
	vals := map[string]string{}
	for _, s := range k.Steps() {
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
	if connectors.Redirects(k) {
		out, err := o.beginSetup(ctx, in, k, conn)
		return ConnectorAdded{Out: out}, err
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
	c, err := o.conns.Add(ctx, in.OrgID, k.Name(), k.Label(), conn.Name(), connectors.Caps(k), string(b))
	if err != nil {
		return ConnectorAdded{}, err
	}
	out := ConnectorAdded{Connector: c}
	if zones != nil {
		out.Domains = dnscap.States(zones)
	}
	return out, nil
}

// pendingTTL is how long a redirect setup waits for the provider.
const pendingTTL = time.Hour

// pendingSetup is a redirect setup out at its provider. ponytail: in
// memory, so a restart mid-redirect loses it (the callback is refused and
// the admin starts again); a row with a sweep if that bites.
type pendingSetup struct {
	orgID            *string
	kind             connectors.Kind
	conn             connectors.Conn
	nonce, user, ret string
	at               time.Time
}

func (o *Orchestrator) beginSetup(ctx context.Context, in ConnectorAdd, k connectors.Kind, conn connectors.Conn) (*ConnectorOut, error) {
	if in.User == "" || !strings.HasPrefix(in.Return, "/") || strings.HasPrefix(in.Return, "//") {
		return nil, errs.Refusedf("%s signs in on its own site: add it in the panel", k.Label())
	}
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	id := uuid.NewString()
	p := &pendingSetup{orgID: in.OrgID, kind: k, conn: conn, nonce: hex.EncodeToString(nonce), user: in.User, ret: in.Return, at: time.Now()}
	out, err := conn.(connectors.Redirector).Out(ctx, id, o.panelURL()+ConnectorCallbackPath, id+"."+p.nonce)
	if err != nil {
		return nil, err
	}
	o.setupMu.Lock()
	defer o.setupMu.Unlock()
	if o.setups == nil {
		o.setups = map[string]*pendingSetup{}
	}
	for id, s := range o.setups {
		if time.Since(s.at) > pendingTTL {
			delete(o.setups, id)
		}
	}
	o.setups[id] = p
	return &out, nil
}

// ConnectorBack takes a provider's callback: the state must match a
// pending setup's and userID must be who began it, or a stray callback
// could finish someone else's. It saves the connection and returns the
// panel path to send the browser back to.
func (o *Orchestrator) ConnectorBack(ctx context.Context, userID string, q url.Values) (string, error) {
	id, nonce, _ := strings.Cut(q.Get("state"), ".")
	o.setupMu.Lock()
	p := o.setups[id]
	ok := p != nil && time.Since(p.at) <= pendingTTL && p.user == userID &&
		subtle.ConstantTimeCompare([]byte(p.nonce), []byte(nonce)) == 1
	if ok {
		delete(o.setups, id) // one go: the provider's code is single use
	}
	o.setupMu.Unlock()
	if !ok {
		return "", errs.Refusedf("this callback does not match a connector being set up; start again")
	}
	if err := p.conn.(connectors.Redirector).Back(ctx, q); err != nil {
		return "", err
	}
	b, err := json.Marshal(p.conn)
	if err != nil {
		return "", err
	}
	_, err = o.conns.Create(ctx, store.Connector{
		ID: id, OrgID: p.orgID, Kind: p.kind.Name(), Name: p.conn.Name(), Settings: string(b),
		CreatedAt: time.Now().UTC(), Capabilities: connectors.Caps(p.kind),
	}, p.kind.Label())
	return p.ret, err
}

// ConnectorsAt is orgID's connections of registered kinds (nil = the
// server's), settings stripped.
func (o *Orchestrator) ConnectorsAt(ctx context.Context, orgID *string) ([]Connector, error) {
	return o.conns.ListOf(ctx, orgID, "")
}

// ConnectorAt is one of them, settings stripped.
func (o *Orchestrator) ConnectorAt(ctx context.Context, orgID *string, id string) (Connector, error) {
	c, err := o.conns.Of(ctx, orgID, id)
	c.Settings = ""
	return c, err
}

// RemoveConnector removes one of them, each capability cleaning up after
// itself first: a DNS one's managed domains as UnmanageDNSDomain does,
// refused while a tile uses one.
func (o *Orchestrator) RemoveConnector(ctx context.Context, orgID *string, id string) error {
	c, err := o.conns.Of(ctx, orgID, id)
	if err != nil {
		return err
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
