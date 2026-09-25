// Package params owns the param store (collections of params and secrets at
// org, stack and env scope) and the one ${{ }} resolver (ref.go). Kind is
// fixed at creation, except param → secret, one way.
package params

import (
	"context"
	"crypto/rand"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/slug"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

const (
	Param  = "param"
	Secret = "secret"
)

// Scope is where an entry lives: org, stack or env, and that row's id.
type Scope struct{ Kind, ID string }

// Entry is one write. A secret with an empty Value is declared, not set: the
// stack file and a masked CLI listing both send those, and neither may
// blank a stored secret.
type Entry struct{ Collection, Name, Kind, Value string }

type Leaf struct{ params store.ParamStore }

// New takes the table; build it on a store.Tx's table to make Merge atomic.
func New(params store.ParamStore) *Leaf { return &Leaf{params: params} }

// List is a scope's entries. secrets=false never reads a secret row (B37):
// the caller without the permission gets params only, not redactions.
func (l *Leaf) List(ctx context.Context, s Scope, secrets bool) ([]store.Param, error) {
	if secrets {
		return l.params.ListByScope(ctx, s.Kind, s.ID)
	}
	return l.params.ListByKind(ctx, s.Kind, s.ID, Param)
}

// Values is a scope as the resolver's snapshot wants it: "collection.name".
func (l *Leaf) Values(ctx context.Context, s Scope, secrets bool) (map[string]Value, error) {
	ps, err := l.List(ctx, s, secrets)
	out := make(map[string]Value, len(ps))
	for _, p := range ps {
		out[p.Collection+"."+p.Name] = Value{V: p.Value, Secret: p.Kind == Secret}
	}
	return out, err
}

// Set writes one entry.
func (l *Leaf) Set(ctx context.Context, s Scope, e Entry) error { return l.Merge(ctx, s, []Entry{e}) }

// Merge upserts the given entries and leaves every other entry of the scope
// alone (B35). Every entry is checked before any row is touched (B4).
func (l *Leaf) Merge(ctx context.Context, s Scope, es []Entry) error {
	type write struct {
		e   Entry
		old *store.Param
	}
	var ws []write
	for _, e := range es {
		switch {
		case !slug.ValidName(e.Collection):
			return errs.Invalidf("collection", "%q: a collection name is lower-case letters, digits and _", e.Collection)
		case !slug.ValidName(e.Name):
			return errs.Invalidf("name", "%q: a param name is lower-case letters, digits and _", e.Name)
		case e.Kind != Param && e.Kind != Secret:
			return errs.Invalidf("type", "type must be param or secret")
		}
		old, err := l.params.GetByName(ctx, s.Kind, s.ID, e.Collection, e.Name)
		switch {
		case errors.Is(err, errs.ErrNotFound):
			ws = append(ws, write{e: e})
		case err != nil:
			return err
		case old.Kind == Secret && e.Kind == Param:
			return errs.Conflictf("%s.%s is a secret; a secret is never turned back into a param. Delete it and create a param.",
				e.Collection, e.Name)
		default:
			ws = append(ws, write{e: e, old: &old})
		}
	}
	now := time.Now().UTC()
	for _, w := range ws {
		declared := w.e.Kind == Secret && w.e.Value == ""
		switch {
		case w.old == nil && declared:
			// Nothing to store: an unset secret parks whoever refs it.
		case w.old == nil:
			if err := l.params.Create(ctx, store.Param{ID: uuid.NewString(), ScopeKind: s.Kind, ScopeID: s.ID,
				Collection: w.e.Collection, Name: w.e.Name, Kind: w.e.Kind, Value: w.e.Value,
				CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
		default:
			p := *w.old
			p.Kind = w.e.Kind
			if !declared {
				p.Value = w.e.Value
			}
			p.UpdatedAt = now
			if err := l.params.Update(ctx, p); err != nil {
				return err
			}
		}
	}
	return nil
}

func (l *Leaf) Delete(ctx context.Context, s Scope, collection, name string) error {
	p, err := l.params.GetByName(ctx, s.Kind, s.ID, collection, name)
	if err != nil {
		return err
	}
	return l.params.Delete(ctx, p.ID)
}

// Generate is a random secret value: letters and digits only, so it is safe
// unquoted in a shell, a URL and YAML.
func Generate(n int) string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	out := make([]byte, 0, n)
	buf := make([]byte, 1)
	for len(out) < n {
		_, _ = rand.Read(buf) // crypto/rand.Read never fails (Go 1.24+)
		// Rejection sampling: 248 = 4*62, so every character is equally likely.
		if buf[0] < 248 {
			out = append(out, charset[int(buf[0])%len(charset)])
		}
	}
	return string(out)
}
