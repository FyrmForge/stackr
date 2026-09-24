// Package org owns orgs, their members and invites.
package org

import (
	"context"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type Leaf struct {
	orgs    store.OrgStore
	members store.OrgMemberStore
}

func New(orgs store.OrgStore, members store.OrgMemberStore) *Leaf {
	return &Leaf{orgs: orgs, members: members}
}

func (l *Leaf) GetBySlug(ctx context.Context, slug string) (store.Org, error) {
	return l.orgs.GetBySlug(ctx, slug)
}

// Roles maps org id to the user's role there.
func (l *Leaf) Roles(ctx context.Context, userID string) (map[string]string, error) {
	ms, err := l.members.ListByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	roles := make(map[string]string, len(ms))
	for _, m := range ms {
		roles[m.OrgID] = m.Role
	}
	return roles, nil
}

// RemoveMember deletes the membership and returns the role it had, for the
// revoke rule.
func (l *Leaf) RemoveMember(ctx context.Context, orgID, userID string) (string, error) {
	m, err := l.members.GetByOrgUser(ctx, orgID, userID)
	if err != nil {
		return "", err
	}
	return m.Role, l.members.Delete(ctx, m.ID)
}
