package service

import (
	"context"
	"errors"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/errs"
)

// The join and controller-cert verbs refuse a non-admin and a nil principal
// on their own, not only at the route.
func TestJoinVerbsNeedNodeAdmin(t *testing.T) {
	ctx := context.Background()
	w, _ := secWorld(t)
	refused := func(err error) bool { return errors.Is(err, errs.ErrRefused) }
	for _, by := range []*Principal{nil, ownerP("o1")} {
		if _, err := w.orch.CreateJoinToken(ctx, by, JoinTokenSpec{NodeName: "w9"}); !refused(err) {
			t.Errorf("CreateJoinToken(%v) = %v", by, err)
		}
		if _, err := w.orch.PendingJoins(ctx, by); !refused(err) {
			t.Errorf("PendingJoins(%v) = %v", by, err)
		}
		if _, err := w.orch.ConfirmJoin(ctx, by, "x"); !refused(err) {
			t.Errorf("ConfirmJoin(%v) = %v", by, err)
		}
		if err := w.orch.RejectJoin(ctx, by, "x"); !refused(err) {
			t.Errorf("RejectJoin(%v) = %v", by, err)
		}
		if err := w.orch.RevokeControllerCert(ctx, by); !refused(err) {
			t.Errorf("RevokeControllerCert(%v) = %v", by, err)
		}
	}
}
