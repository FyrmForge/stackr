package connector

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Capabilities and DNS kinds. A connector without "dns" is a git one: rows
// made before capabilities existed carry none.
const (
	CapGit        = "git"
	CapDNS        = "dns"
	Namecheap     = "namecheap"
	NamecheapHost = "api.namecheap.com"
)

// DNS is a DNS connector's account, sealed in connectors.config. Key never
// leaves the service.
type DNS struct {
	User     string `json:"user"`
	Key      string `json:"key"`
	ClientIP string `json:"client_ip"`
}

// IsDNS says whether c is a DNS connector.
func IsDNS(c store.Connector) bool { return slices.Contains(c.Capabilities, CapDNS) }

// DNSOf is c's DNS account; false for a git connector.
func DNSOf(c store.Connector) (DNS, bool) {
	cfg := parse(c)
	if cfg.DNS == nil {
		return DNS{}, false
	}
	return *cfg.DNS, true
}

func gitOnly(cs []store.Connector) []store.Connector {
	return slices.DeleteFunc(cs, IsDNS)
}

// AddDNS saves a DNS connector of kind for orgID (nil = the server's). The
// caller has checked the account answers.
func (l *Leaf) AddDNS(ctx context.Context, orgID *string, kind string, d DNS) (store.Connector, error) {
	if kind != Namecheap {
		return store.Connector{}, errs.Invalidf("provider", "unknown DNS provider %q", kind)
	}
	b, _ := json.Marshal(config{DNS: &d})
	c := store.Connector{
		ID:           uuid.NewString(),
		OrgID:        orgID,
		Provider:     kind,
		Name:         "Namecheap · " + d.User,
		Host:         NamecheapHost,
		Config:       string(b),
		CreatedAt:    time.Now().UTC(),
		Capabilities: store.StringList{CapDNS},
	}
	if err := l.conns.Create(ctx, c); err != nil {
		if _, ok := errs.IsConflict(err); ok {
			if orgID != nil {
				return store.Connector{}, errs.Conflictf("this organization already has a Namecheap connector")
			}
			return store.Connector{}, errs.Conflictf("a server connector named %q exists", c.Name)
		}
		return store.Connector{}, err
	}
	c.Config = ""
	return c, nil
}

// DNSConnector is DNS connector id of orgID (nil = a server one), config
// included for the provider call. Anything else is not found: an org never
// reaches a server connector or another org's here.
func (l *Leaf) DNSConnector(ctx context.Context, orgID *string, id string) (store.Connector, error) {
	c, err := l.conns.Get(ctx, id)
	if err != nil {
		return c, err
	}
	same := (orgID == nil && c.OrgID == nil) || (orgID != nil && c.OrgID != nil && *orgID == *c.OrgID)
	if !same || !IsDNS(c) {
		return store.Connector{}, errs.ErrNotFound
	}
	return c, nil
}

// ListDNS is orgID's DNS connectors (nil = the server's), config stripped.
// Never widened to shared server connectors: their domains are shared, not
// the account.
func (l *Leaf) ListDNS(ctx context.Context, orgID *string) ([]store.Connector, error) {
	var cs []store.Connector
	var err error
	if orgID == nil {
		cs, err = l.conns.ListServer(ctx)
	} else {
		cs, err = l.conns.ListByOrg(ctx, *orgID)
	}
	cs = slices.DeleteFunc(cs, func(c store.Connector) bool { return !IsDNS(c) })
	for i := range cs {
		cs[i].Config = ""
	}
	return cs, err
}

// DeleteDNS deletes a DNS connector and, by cascade, its managed domains.
func (l *Leaf) DeleteDNS(ctx context.Context, orgID *string, id string) error {
	if _, err := l.DNSConnector(ctx, orgID, id); err != nil {
		return err
	}
	return l.conns.Delete(ctx, id)
}
