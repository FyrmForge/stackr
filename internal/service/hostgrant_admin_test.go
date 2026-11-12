package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// A server admin's ask is granted on the spot, so the park's text says it was
// approved; any other actor's still says it waits.
func TestParkOnApprovalSaysAdminApproved(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	now := time.Now()
	must(t, w.st.Users.Create(ctx, store.User{ID: "adm", Email: "a@b.c", Name: "A", Role: "admin", Active: true, CreatedAt: now, UpdatedAt: now}))
	must(t, w.st.Users.Create(ctx, store.User{ID: "dev", Email: "d@b.c", Name: "D", Role: "user", Active: true, CreatedAt: now, UpdatedAt: now}))
	n := errs.NeedsApproval{Stack: w.stack, What: "elevated access: mon published:8080:80"}
	for actor, want := range map[string]string{"adm": "approved by you", "dev": "waiting:"} {
		r := &jobs.Run{Job: store.Job{ActorID: actor, Payload: "{}"}}
		err := w.orch.parkOnApproval(ctx, r, n)
		if _, ok := errs.IsNeedsApproval(err); !ok || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want it to hold %q", actor, err, want)
		}
	}
}
