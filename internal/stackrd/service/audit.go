package service

import (
	"context"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// AuditService reads the audit trail.
//
// Writing to it is not here on purpose: an audit row is written by whatever
// did the thing, inside the same call, through store/audit. A service that
// owned the write would put one hop between an action and its record, and the
// record would be the thing that gets forgotten.
type AuditService struct {
	store repo.Store
}

func NewAuditService(store repo.Store) *AuditService { return &AuditService{store: store} }

// For is one resource's trail, newest first. Tenancy is the route's: every
// caller reaches this behind a gate that has already resolved whose resource
// it is.
func (s *AuditService) For(ctx context.Context, ownerKind, ownerID string, limit int) ([]repo.AuditEvent, error) {
	return s.store.ListAuditEvents(ctx, ownerKind, ownerID, limit)
}

// All is the server-wide trail, optionally narrowed to one actor or one
// action. Admin-only by nature — it spans every organization — which is why
// its one route carries VerbAdminRead.
func (s *AuditService) All(ctx context.Context, actor, action string, limit int) ([]repo.AuditEvent, error) {
	return s.store.ListAllAuditEvents(ctx, actor, action, limit)
}
