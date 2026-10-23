package service

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/connectors"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// connstatus.go is a connection's own state, the same for every kind: its
// last check of the provider (a capability's cheapest read: DNS lists the
// zones, Git the repos) and its credentials, updated in place through the
// kind's setup fields.

// checkTTL is how old a check may be before opening the connection asks
// again.
const checkTTL = 5 * time.Minute

// ask asks conn's provider the cheapest thing its capabilities offer.
func (o *Orchestrator) ask(ctx context.Context, k connectors.Kind, conn connectors.Conn) error {
	caps := connectors.Caps(k)
	switch {
	case slices.Contains(caps, CapDNS):
		p, err := o.dnsConn(k, conn)
		if err == nil {
			_, err = p.Zones(ctx)
		}
		return err
	case slices.Contains(caps, CapGit):
		_, err := conn.(connectors.GitSource).Repos(ctx)
		return err
	}
	return nil
}

// refusalOf is err as the status line says it: the kind's explanation.
func refusalOf(err error) string {
	if inv, ok := errs.IsInvalid(err); ok {
		return inv.Msg
	}
	return err.Error()
}

// check asks c's provider now and keeps the answer on the row.
func (o *Orchestrator) check(ctx context.Context, c store.Connector) store.Connector {
	if c.Pending {
		return c
	}
	k, conn, err := o.open(c)
	if err == nil {
		err = o.ask(ctx, k, conn)
	}
	o.noteCheck(ctx, &c, err)
	return c
}

// noteCheck keeps a check's answer on c: err nil is working.
func (o *Orchestrator) noteCheck(ctx context.Context, c *store.Connector, err error) {
	now, msg := time.Now().UTC(), ""
	if err != nil {
		msg = refusalOf(err)
	}
	if o.store.Connectors.SetCheck(ctx, c.ID, now, msg) == nil {
		c.CheckedAt, c.CheckError = &now, msg
	}
}

// failed keeps a capability call's provider refusal on connection id, so
// its status says so without a check.
func (o *Orchestrator) failed(ctx context.Context, id string, err error) {
	if err == nil || errors.Is(err, errs.ErrNotFound) {
		return
	}
	c := store.Connector{ID: id}
	o.noteCheck(ctx, &c, err)
}

// ConnectorStatus is connection id of orgID (nil = a server one), settings
// stripped, with a check newer than checkTTL: one older is asked again.
func (o *Orchestrator) ConnectorStatus(ctx context.Context, orgID *string, id string) (Connector, error) {
	c, err := o.conns.Of(ctx, orgID, id)
	if err != nil {
		return c, err
	}
	if c.CheckedAt == nil || time.Since(*c.CheckedAt) > checkTTL {
		c = o.check(ctx, c)
	}
	c.Settings = ""
	return c, nil
}

// CheckConnector asks connection id's provider now (Check now).
func (o *Orchestrator) CheckConnector(ctx context.Context, orgID *string, id string) (Connector, error) {
	c, err := o.conns.Of(ctx, orgID, id)
	if err != nil {
		return c, err
	}
	c = o.check(ctx, c)
	c.Settings = ""
	return c, nil
}

// credFields is k's fields an update asks for: its setup's, or for a kind
// whose setup leaves for the provider, its Alt form's; none for a kind
// that offers neither.
func credFields(k connectors.Kind) []connectors.Field {
	steps := k.Steps()
	if connectors.Redirects(k) {
		a, ok := k.(connectors.Alter)
		if !ok {
			return nil
		}
		steps = a.Alt().Steps
	}
	var out []connectors.Field
	for _, s := range steps {
		out = append(out, s.Fields...)
	}
	return out
}

// ConnectorCredentials is connection id's update form: the fields and the
// non-secret ones' values. No fields: its credentials are not updated in
// place.
func (o *Orchestrator) ConnectorCredentials(ctx context.Context, orgID *string, id string) ([]ConnectorField, map[string]string, error) {
	c, err := o.conns.Of(ctx, orgID, id)
	if err != nil || c.Pending {
		return nil, nil, err
	}
	k, conn, err := o.open(c)
	if err != nil {
		return nil, nil, err
	}
	v, ok := conn.(connectors.Valuer)
	if !ok {
		return nil, nil, nil
	}
	fs, cur := credFields(k), v.Values()
	vals := map[string]string{}
	for _, f := range fs {
		if !f.Secret {
			vals[f.Name] = cur[f.Name]
		}
	}
	return fs, vals, nil
}

// UpdateConnector replaces connection id's credentials with values (a
// secret left empty keeps the stored one), checked by its kind's rules and
// then against the provider: a refusal keeps the old ones. A name that was
// the connection's own follows the new account.
func (o *Orchestrator) UpdateConnector(ctx context.Context, orgID *string, id string, values map[string]string) (Connector, error) {
	c, err := o.conns.Of(ctx, orgID, id)
	if err != nil {
		return c, err
	}
	if c.Pending {
		return Connector{}, errs.Conflictf("This connector is still being set up.")
	}
	k, conn, err := o.open(c)
	if err != nil {
		return Connector{}, err
	}
	v, ok := conn.(connectors.Valuer)
	fs := credFields(k)
	if !ok || len(fs) == 0 {
		return Connector{}, errs.Refusedf("%s credentials are not updated here", k.Label())
	}
	cur, vals := v.Values(), map[string]string{}
	for _, f := range fs {
		if vals[f.Name] = values[f.Name]; f.Secret && vals[f.Name] == "" {
			vals[f.Name] = cur[f.Name]
		}
	}
	next := k.New()
	if err := next.Setup(vals); err != nil {
		return Connector{}, err
	}
	if err := o.ask(ctx, k, next); err != nil {
		return Connector{}, err
	}
	b, err := json.Marshal(next)
	if err != nil {
		return Connector{}, err
	}
	name := c.Name
	if name == conn.Name() {
		name = next.Name()
	}
	if err := o.conns.Settle(ctx, id, name, string(b)); err != nil {
		return Connector{}, err
	}
	c.Name = name
	o.noteCheck(ctx, &c, nil)
	c.Settings = ""
	return c, nil
}

// RenameConnectorAt renames connection id of orgID (nil = a server one).
func (o *Orchestrator) RenameConnectorAt(ctx context.Context, orgID *string, id, name string) (Connector, error) {
	if orgID == nil {
		sc, err := o.RenameServerConnector(ctx, id, name)
		return sc.Connector, err
	}
	if _, err := o.conns.Of(ctx, orgID, id); err != nil {
		return Connector{}, err
	}
	return o.conns.RenameAny(ctx, id, name)
}
