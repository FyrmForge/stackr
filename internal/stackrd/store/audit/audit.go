// Package audit records reads and writes of secret variable values. An event
// is best-effort by design: losing one audit row is better than blocking the
// action it describes, so Record logs failures instead of returning them.
package audit

import (
	"context"
	"log/slog"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Actions. "Read" actions mean the plaintext left the server.
const (
	Reveal = "reveal" // shown unmasked in the UI
	Copy   = "copy"   // fetched for the clipboard
	Edit   = "edit"   // pre-filled into an edit form
	Read   = "read"   // listed unmasked over the API
	Share  = "share"  // revealed through a one-time share link
	Set    = "set"    // value written
	Delete = "delete" // variable removed
)

func Record(ctx context.Context, s repo.Store, actor, action, ownerKind, ownerID, name string) {
	e := &repo.AuditEvent{Actor: actor, Action: action, OwnerKind: ownerKind, OwnerID: ownerID, Name: name}
	if err := s.AddAuditEvent(ctx, e); err != nil {
		slog.Error("audit event dropped", "action", action, "owner", ownerKind+":"+ownerID, "name", name, "err", err)
	}
}
