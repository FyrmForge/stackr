// Package stack owns stacks: create, rename, delete, the config repo pointer,
// the stack file's domain reservations and the opaque defaults slot
// (settings, merged by leaf/settings). Deleting a stack's tiles and
// containers first is the flow's job.
package stack

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/slug"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Reservation is one host the stack file claims at stack level. Hosts are
// unique across the server; that check needs the domains table, so the flow
// does it.
type Reservation struct {
	Host                string `json:"host"`
	ACMEEmail           string `json:"acme_email,omitempty"`
	IncludeEnvOnDefault bool   `json:"include_env_on_default"` // api.prod.stack.org vs api.stack.org
}

type Leaf struct{ stacks store.StackStore }

func New(stacks store.StackStore) *Leaf { return &Leaf{stacks: stacks} }

func (l *Leaf) Get(ctx context.Context, id string) (store.Stack, error) { return l.stacks.Get(ctx, id) }

func (l *Leaf) GetBySlug(ctx context.Context, orgID, slug string) (store.Stack, error) {
	return l.stacks.GetBySlug(ctx, orgID, slug)
}

func (l *Leaf) List(ctx context.Context, orgID string) ([]store.Stack, error) {
	return l.stacks.ListByOrg(ctx, orgID)
}

// Create makes an empty stack; the slug comes from the name.
func (l *Leaf) Create(ctx context.Context, orgID, name, description string) (store.Stack, error) {
	s := store.Stack{ID: uuid.NewString(), OrgID: orgID, Description: description,
		Settings: "{}", Domains: "[]", CreatedAt: time.Now().UTC()}
	if err := l.name(ctx, &s, name); err != nil {
		return s, err
	}
	return s, l.stacks.Create(ctx, s)
}

// Rename moves name and slug together.
func (l *Leaf) Rename(ctx context.Context, s store.Stack, name string) (store.Stack, error) {
	if err := l.name(ctx, &s, name); err != nil {
		return s, err
	}
	return s, l.stacks.Update(ctx, s)
}

func (l *Leaf) name(ctx context.Context, s *store.Stack, name string) error {
	name = strings.TrimSpace(name)
	sl := slug.Make(name)
	switch {
	case name == "":
		return errs.Invalidf("name", "Give the stack a name.")
	case sl == "":
		return errs.Invalidf("name", "That name needs at least one letter or digit, since it becomes the URL.")
	case slug.Reserved(sl):
		return errs.Invalidf("name", "%q is reserved.", sl)
	}
	if other, err := l.stacks.GetBySlug(ctx, s.OrgID, sl); err == nil && other.ID != s.ID {
		return errs.Invalidf("name", "Another stack in this organization already uses that name.")
	} else if err != nil && !errors.Is(err, errs.ErrNotFound) {
		return err
	}
	s.Name, s.Slug = name, sl
	return nil
}

// Delete removes the row; the cascade takes envs, tiles and params with it.
func (l *Leaf) Delete(ctx context.Context, id string) error { return l.stacks.Delete(ctx, id) }

// SetConfigRepo points the stack at its stack file. An empty repo clears it.
func (l *Leaf) SetConfigRepo(ctx context.Context, s store.Stack, connectorID, repo, branch, path string) (store.Stack, error) {
	s.ConfigConnectorID, s.ConfigRepo, s.ConfigBranch, s.ConfigPath = connectorID, repo, branch, path
	if repo == "" {
		s.ConfigConnectorID, s.ConfigBranch, s.ConfigPath = "", "", ""
	}
	return s, l.stacks.Update(ctx, s)
}

// Reservations parses the stack's domain reservations.
func Reservations(s store.Stack) ([]Reservation, error) {
	var rs []Reservation
	return rs, json.Unmarshal([]byte(s.Domains), &rs)
}

// SetReservations replaces the list: hosts required, lower-cased, no repeats.
func (l *Leaf) SetReservations(ctx context.Context, s store.Stack, rs []Reservation) (store.Stack, error) {
	seen := map[string]bool{}
	for i := range rs {
		h := strings.ToLower(strings.TrimSpace(rs[i].Host))
		if h == "" {
			return s, errs.Invalidf("domains", "Each domain needs a host.")
		}
		if seen[h] {
			return s, errs.Invalidf("domains", "%s is declared twice.", h)
		}
		seen[h], rs[i].Host = true, h
	}
	if rs == nil {
		rs = []Reservation{}
	}
	b, err := json.Marshal(rs)
	if err != nil {
		return s, err
	}
	s.Domains = string(b)
	return s, l.stacks.Update(ctx, s)
}

// SetSettings stores the defaults cascade slot as given; leaf/settings owns
// its shape and the merge.
func (l *Leaf) SetSettings(ctx context.Context, s store.Stack, blob string) (store.Stack, error) {
	s.Settings = blob
	return s, l.stacks.Update(ctx, s)
}
