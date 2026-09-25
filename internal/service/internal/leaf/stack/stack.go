// Package stack owns stacks: create, rename, delete, the config repo pointer
// and the opaque defaults slot (settings, merged by leaf/settings). The
// stack file's domain reservations are stack-level rows of leaf/domainres.
// Deleting a stack's tiles and containers first is the flow's job.
package stack

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/slug"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type Leaf struct{ stacks store.StackStore }

func New(stacks store.StackStore) *Leaf {
	return &Leaf{stacks: stacks}
}

func (l *Leaf) Get(ctx context.Context, id string) (store.Stack, error) {
	return l.stacks.Get(ctx, id)
}

func (l *Leaf) GetBySlug(ctx context.Context, orgID, slug string) (store.Stack, error) {
	return l.stacks.GetBySlug(ctx, orgID, slug)
}

func (l *Leaf) List(ctx context.Context, orgID string) ([]store.Stack, error) {
	return l.stacks.ListByOrg(ctx, orgID)
}

// Create makes an empty stack; the slug comes from the name.
func (l *Leaf) Create(ctx context.Context, orgID, name, description string) (store.Stack, error) {
	s := store.Stack{
		ID:          uuid.NewString(),
		OrgID:       orgID,
		Description: description,
		Settings:    "{}",
		CreatedAt:   time.Now().UTC(),
	}
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
func (l *Leaf) SetConfigRepo(
	ctx context.Context,
	s store.Stack,
	connectorID, repo, branch, path string,
) (store.Stack, error) {
	s.ConfigConnectorID = connectorID
	s.ConfigRepo = repo
	s.ConfigBranch = branch
	s.ConfigPath = path
	if repo == "" {
		s.ConfigConnectorID, s.ConfigBranch, s.ConfigPath = "", "", ""
	}
	return s, l.stacks.Update(ctx, s)
}

// SetSettings stores the defaults cascade slot as given; leaf/settings owns
// its shape and the merge.
func (l *Leaf) SetSettings(ctx context.Context, s store.Stack, blob string) (store.Stack, error) {
	s.Settings = blob
	return s, l.stacks.Update(ctx, s)
}
