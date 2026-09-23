package main

import (
	"context"
	"strings"

	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// roomReadable decides whether a websocket client may join a room.
//
// The rooms are named in handlers/notify/notify.go. Payloads carry no data,
// only the kind of thing that changed, so this is about not telling a stranger
// when an org is busy — it is not protecting content. It fails closed: an
// unknown room name, or no session on the upgrade, is a refusal.
func roomReadable(ctx context.Context, store repo.Store, access *service.AccessService, userID, room string) bool {
	if userID == "" || room == "" {
		return false
	}
	u, err := store.GetUserByID(ctx, userID)
	if err != nil || u == nil || !u.Active {
		return false
	}
	p, err := access.Principal(ctx, u, nil, false)
	if err != nil {
		return false
	}
	kind, id, _ := strings.Cut(room, ":")
	switch kind {
	case "notifications":
		// The user's own inbox badge. Anyone signed in has one.
		return true
	case "containers", "server", "flows":
		// Whole-box rooms, which are the admin screens.
		return p.Admin
	case "org":
		return access.Require(p, service.VerbOrgRead, id) == nil
	case "project":
		st, err := store.GetStack(ctx, id)
		if err != nil || st == nil {
			return false
		}
		return access.Require(p, service.VerbOrgRead, st.OrgID) == nil
	}
	return false
}
