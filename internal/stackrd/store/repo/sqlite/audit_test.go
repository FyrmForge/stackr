package sqlite_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// The audit trail is only useful if it reads back newest-first and stays
// inside its own scope: one stack's reveals must not show up under another's.
func TestAuditEventsScopedAndNewestFirst(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)

	for _, e := range []repo.AuditEvent{
		{Actor: "a@example.com", Action: "set", OwnerKind: repo.OwnerStack, OwnerID: "s1", Name: "FIRST"},
		{Actor: "b@example.com", Action: "reveal", OwnerKind: repo.OwnerStack, OwnerID: "s1", Name: "SECOND"},
		{Actor: "c@example.com", Action: "copy", OwnerKind: repo.OwnerStack, OwnerID: "other", Name: "ELSEWHERE"},
	} {
		require.NoError(t, store.AddAuditEvent(ctx, &e), "add %s", e.Name)
	}

	got, err := store.ListAuditEvents(ctx, repo.OwnerStack, "s1", 50)
	require.NoError(t, err)
	require.Len(t, got, 2, "the other scope's event must not leak in")
	assert.Equal(t, "SECOND", got[0].Name, "newest first")
	assert.Equal(t, "reveal", got[0].Action)
	assert.Equal(t, "b@example.com", got[0].Actor)
	assert.False(t, got[0].CreatedAt.IsZero(), "created_at defaults on insert")

	got, err = store.ListAuditEvents(ctx, repo.OwnerStack, "s1", 1)
	require.NoError(t, err)
	assert.Len(t, got, 1, "limit applies")
}
