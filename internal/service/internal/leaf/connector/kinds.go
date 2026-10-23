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

// kinds.go is the connections of registered kinds (connectors/all): a row
// whose settings are the kind's blob. Git connectors (GitHub) stay on the
// code above until they move onto the Git source capability; nothing here
// reaches one, and nothing above reaches one of these.

// CapGit is the git capability; a row made before capabilities carries it.
const CapGit = connectors.CapGit

// Has says whether c has capability cp switched on.
func Has(c store.Connector, cp string) bool { return slices.Contains(c.Capabilities, cp) }

// IsGit: a GitHub row, the code above's. ponytail: by kind until GitHub
// is a registered kind with the Git source capability (phase 2).
func IsGit(c store.Connector) bool { return c.Kind == GitHub }

func gitOnly(cs []store.Connector) []store.Connector {
	return slices.DeleteFunc(cs, func(c store.Connector) bool { return !IsGit(c) })
}

// Add saves a connection of kind for orgID (nil = the server's), named
// name, with capabilities caps and the kind's settings blob. label names
// the kind in a refusal. The caller has checked the account.
func (l *Leaf) Add(ctx context.Context, orgID *string, kind, label, name string, caps []string, settings string) (store.Connector, error) {
	c := store.Connector{
		ID:           uuid.NewString(),
		OrgID:        orgID,
		Kind:         kind,
		Name:         name,
		Host:         kind,
		Settings:     settings,
		CreatedAt:    time.Now().UTC(),
		Capabilities: store.StringList(caps),
	}
	return l.Create(ctx, c, label)
}

// Create saves c as built (its id chosen by the caller: a redirect setup
// hands it to the provider first).
func (l *Leaf) Create(ctx context.Context, c store.Connector, label string) (store.Connector, error) {
	c.Host = c.Kind
	if err := l.conns.Create(ctx, c); err != nil {
		if _, ok := errs.IsConflict(err); ok {
			if c.OrgID != nil {
				return store.Connector{}, errs.Conflictf("This organization already has a %s connector", label)
			}
			return store.Connector{}, errs.Conflictf("A server connector named %q exists", c.Name)
		}
		return store.Connector{}, err
	}
	c.Settings = ""
	return c, nil
}

// Of is connection id of orgID (nil = a server one), settings included for
// the kind. Anything else is not found: an org never reaches a server
// connection or another org's here, and a git connector never.
func (l *Leaf) Of(ctx context.Context, orgID *string, id string) (store.Connector, error) {
	c, err := l.conns.Get(ctx, id)
	if err != nil {
		return c, err
	}
	same := (orgID == nil && c.OrgID == nil) || (orgID != nil && c.OrgID != nil && *orgID == *c.OrgID)
	if !same || IsGit(c) {
		return store.Connector{}, errs.ErrNotFound
	}
	return c, nil
}

// ListOf is orgID's connections (nil = the server's) with capability cp
// ("" = any), settings stripped. Never widened to shared server ones.
func (l *Leaf) ListOf(ctx context.Context, orgID *string, cp string) ([]store.Connector, error) {
	var cs []store.Connector
	var err error
	if orgID == nil {
		cs, err = l.conns.ListServer(ctx)
	} else {
		cs, err = l.conns.ListByOrg(ctx, *orgID)
	}
	cs = slices.DeleteFunc(cs, func(c store.Connector) bool { return IsGit(c) || cp != "" && !Has(c, cp) })
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

// Reshape moves settings blobs out of the envelope this leaf wrapped DNS
// accounts in before connection kinds existed ({"dns": {...}}). Run at
// start; a reshaped row is left alone. ponytail: delete once every install
// has started on 033.
func (l *Leaf) Reshape(ctx context.Context) error {
	cs, err := l.conns.ListAll(ctx)
	if err != nil {
		return err
	}
	for _, c := range cs {
		var env map[string]json.RawMessage
		if IsGit(c) || json.Unmarshal([]byte(c.Settings), &env) != nil || len(env) != 1 || env["dns"] == nil {
			continue
		}
		if err := l.conns.SetSettings(ctx, c.ID, string(env["dns"])); err != nil {
			return err
		}
	}
	return nil
}
