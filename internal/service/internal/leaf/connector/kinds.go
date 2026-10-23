package connector

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/connectors"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// kinds.go is the scope-checked reach of every connection (Of, ListOf),
// the save of one, and a redirect setup's pending row.

// CapGit is the git capability; a row made before capabilities carries it.
const CapGit = connectors.CapGit

// Has says whether c has capability cp switched on.
func Has(c store.Connector, cp string) bool { return slices.Contains(c.Capabilities, cp) }

// IsGit: a connection with the Git source capability, which the git verbs
// above reach.
func IsGit(c store.Connector) bool { return Has(c, CapGit) }

// Add saves a connection of kind for orgID (nil = the server's), named
// name, on host ("" = the kind), with capabilities caps and the kind's
// settings blob. label names the kind in a refusal. The caller has checked
// the account.
func (l *Leaf) Add(ctx context.Context, orgID *string, kind, label, name, host string, caps []string, settings string) (store.Connector, error) {
	c := store.Connector{
		ID:           uuid.NewString(),
		OrgID:        orgID,
		Kind:         kind,
		Name:         name,
		Host:         host,
		Settings:     settings,
		CreatedAt:    time.Now().UTC(),
		Capabilities: store.StringList(caps),
	}
	return l.Create(ctx, c, label)
}

// Create saves c as built (its id chosen by the caller: a redirect setup
// hands it to the provider first). A connection without a host holds its
// kind there, so the (org, host) key never makes two kinds collide.
func (l *Leaf) Create(ctx context.Context, c store.Connector, label string) (store.Connector, error) {
	if c.Host == "" {
		c.Host = c.Kind
	}
	if err := l.conns.Create(ctx, c); err != nil {
		if _, ok := errs.IsConflict(err); ok {
			switch {
			case c.OrgID != nil && IsGit(c):
				return store.Connector{}, errs.Conflictf("This org already has a %s connector", c.Host)
			case c.OrgID != nil:
				return store.Connector{}, errs.Conflictf("This organization already has a %s connector", label)
			}
			return store.Connector{}, errs.Conflictf("A server connector named %q exists", c.Name)
		}
		return store.Connector{}, err
	}
	c.Settings = ""
	return c, nil
}

// Pend saves c pending: p is its setup, blob the kind's settings so far.
func (l *Leaf) Pend(ctx context.Context, c store.Connector, label string, p Pending, blob string) (store.Connector, error) {
	b, err := json.Marshal(envelope{Pending: &p, Conn: json.RawMessage(blob)})
	if err != nil {
		return c, err
	}
	c.Settings = string(b)
	c, err = l.Create(ctx, c, label)
	c.Pending = true
	return c, err
}

// Pending is pending connection id with its setup and blob so far. One
// that is gone or already connected is not found.
func (l *Leaf) Pending(ctx context.Context, id string) (store.Connector, Pending, string, error) {
	c, err := l.conns.Get(ctx, id)
	if err != nil {
		return c, Pending{}, "", err
	}
	p, blob := pendingOf(c)
	if p == nil {
		return c, Pending{}, "", errs.ErrNotFound
	}
	if len(blob) == 0 {
		blob = json.RawMessage("{}")
	}
	return c, *p, string(blob), nil
}

// Settle is a pending connection's setup done: its name and blob.
func (l *Leaf) Settle(ctx context.Context, id, name, blob string) error {
	return l.conns.SetApp(ctx, id, name, blob)
}

// Of is connection id of orgID (nil = a server one), settings included for
// the kind. Anything else is not found: an org never reaches a server
// connection or another org's here.
func (l *Leaf) Of(ctx context.Context, orgID *string, id string) (store.Connector, error) {
	c, err := l.conns.Get(ctx, id)
	if err != nil {
		return c, err
	}
	same := (orgID == nil && c.OrgID == nil) || (orgID != nil && c.OrgID != nil && *orgID == *c.OrgID)
	if !same {
		return store.Connector{}, errs.ErrNotFound
	}
	return mark(c), nil
}

// ListOf is orgID's connections (nil = the server's) with capability cp
// ("" = any), settings stripped; pending ones only when pending is set.
// Never widened to shared server ones.
func (l *Leaf) ListOf(ctx context.Context, orgID *string, cp string, pending bool) ([]store.Connector, error) {
	var cs []store.Connector
	var err error
	if orgID == nil {
		cs, err = l.conns.ListServer(ctx)
	} else {
		cs, err = l.conns.ListByOrg(ctx, *orgID)
	}
	for i := range cs {
		cs[i] = mark(cs[i])
	}
	cs = slices.DeleteFunc(cs, func(c store.Connector) bool { return cp != "" && !Has(c, cp) || c.Pending && !pending })
	for i := range cs {
		cs[i].Settings = ""
	}
	return cs, err
}

// Remove deletes a connection and, by cascade, what hangs off it.
func (l *Leaf) Remove(ctx context.Context, orgID *string, id string) error {
	if _, err := l.Of(ctx, orgID, id); err != nil {
		return err
	}
	return l.conns.Delete(ctx, id)
}

// Reshape moves settings blobs out of the envelopes this leaf kept before
// connection kinds existed: {"dns": {...}} and {"app": {...}} become the
// kind's own blob, a handshake begun ({"state", "user"}) a pending setup
// whose callback still finishes it. Run at start; a reshaped row is left
// alone. ponytail: delete once every install has started on phase 2.
func (l *Leaf) Reshape(ctx context.Context) error {
	cs, err := l.conns.ListAll(ctx)
	if err != nil {
		return err
	}
	for _, c := range cs {
		var env map[string]json.RawMessage
		if json.Unmarshal([]byte(c.Settings), &env) != nil {
			continue
		}
		var blob []byte
		switch {
		case len(env) == 1 && env["dns"] != nil:
			blob = env["dns"]
		case len(env) == 1 && env["app"] != nil:
			blob = env["app"]
		case env["state"] != nil && env["app"] == nil && env["pending"] == nil:
			var p Pending
			_ = json.Unmarshal(env["state"], &p.Nonce)
			_ = json.Unmarshal(env["user"], &p.User)
			blob, _ = json.Marshal(envelope{Pending: &p, Conn: json.RawMessage("{}")})
		default:
			continue
		}
		if err := l.conns.SetSettings(ctx, c.ID, string(blob)); err != nil {
			return err
		}
	}
	return nil
}
