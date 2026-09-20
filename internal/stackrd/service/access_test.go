package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo/sqlite"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// seatIn puts a user in an org at a role and returns the resolved principal.
func seatIn(t *testing.T, store *sqlite.Store, access *service.AccessService, orgID, role string) service.Principal {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	u := &repo.User{ID: "u-" + role, Email: role + "@example.com", Name: role,
		Role: "user", Active: true, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, store.CreateUser(ctx, u))
	require.NoError(t, store.UpsertOrgMember(ctx, &repo.OrgMember{OrgID: orgID, UserID: u.ID, Role: role, CreatedAt: now}))
	p, err := access.Principal(ctx, u, nil, false)
	require.NoError(t, err)
	return p
}

// The point-15 rule: where the two surfaces disagreed on a verb, the higher
// level wins, because the lower one was a forgotten guard and not a considered
// grant. These four are the rows the earlier points decided; the table is
// where they are now written down once.
func TestTheContestedVerbsTookTheHigherLevel(t *testing.T) {
	for _, tc := range []struct {
		verb service.Verb
		want service.Level
	}{
		{service.VerbRegistryCredential, service.LevelOwner},
		{service.VerbRegistryTagDelete, service.LevelOwner},
		{service.VerbOrgPlanApprove, service.LevelOwner},
		{service.VerbOrgDefaults, service.LevelOwner},
		{service.VerbDeploymentCancel, service.LevelWrite},
		{service.VerbSetupDomain, service.LevelOwner},
		{service.VerbSetupConnector, service.LevelOwner},
	} {
		assert.Equal(t, tc.want, service.LevelOf(tc.verb), "%s", tc.verb)
	}
}

// A verb nobody registered is an operation whose level nobody decided. It has
// to fail closed, or the next one added is a hole.
func TestAnUnknownVerbNeedsAdmin(t *testing.T) {
	assert.Equal(t, service.LevelAdmin, service.LevelOf(service.Verb("something.new")))
}

func TestTheLadder(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)
	access := service.NewAccessService(store)

	viewer := seatIn(t, store, access, seed.Org.ID, "viewer")
	member := seatIn(t, store, access, seed.Org.ID, "member")
	owner := seatIn(t, store, access, seed.Org.ID, "owner")

	// A viewer reads and nothing else.
	assert.NoError(t, access.Require(viewer, service.VerbOrgRead, seed.Org.ID))
	assert.ErrorIs(t, access.Require(viewer, service.VerbStackWrite, seed.Org.ID), svcerr.ErrForbidden)
	assert.ErrorIs(t, access.Require(viewer, service.VerbDeploymentCancel, seed.Org.ID), svcerr.ErrForbidden)

	// A member writes but does not own.
	assert.NoError(t, access.Require(member, service.VerbStackWrite, seed.Org.ID))
	assert.NoError(t, access.Require(member, service.VerbDeploymentCancel, seed.Org.ID))
	assert.ErrorIs(t, access.Require(member, service.VerbRegistryCredential, seed.Org.ID), svcerr.ErrForbidden)

	assert.NoError(t, access.Require(owner, service.VerbRegistryCredential, seed.Org.ID))
	assert.ErrorIs(t, access.Require(owner, service.VerbNodeManage, seed.Org.ID), svcerr.ErrForbidden,
		"a node is not org content; owning an org does not reach it")

	// Outside the org is NotFound, not Forbidden: a 403 confirms the id to
	// someone who should not have it.
	outsider, err := access.Principal(ctx, &repo.User{ID: "nobody", Role: "user", Active: true}, nil, false)
	require.NoError(t, err)
	assert.True(t, errors.Is(access.Require(outsider, service.VerbOrgRead, seed.Org.ID), svcerr.ErrNotFound))

	admin, err := access.Principal(ctx, &repo.User{ID: "root", Role: "admin", Active: true}, nil, false)
	require.NoError(t, err)
	for _, v := range service.Verbs() {
		assert.NoError(t, access.Require(admin, v, seed.Org.ID), "an admin is owner everywhere: %s", v)
	}
}

// A scope narrows a key below its user's rights; it never widens one, so the
// role check runs first and a missing scope is the second refusal.
func TestAScopeIsCheckedOnTopOfTheRole(t *testing.T) {
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)
	access := service.NewAccessService(store)
	member := seatIn(t, store, access, seed.Org.ID, "member")

	keyed := member
	keyed.Keyed, keyed.Scopes = true, []string{"stacks:read"}
	err := access.RequireScope(keyed, "stacks:write", service.VerbStackWrite, seed.Org.ID)
	assert.ErrorIs(t, err, svcerr.ErrForbidden)
	assert.Contains(t, err.Error(), "stacks:write")

	keyed.Scopes = []string{"stacks:write"}
	assert.NoError(t, access.RequireScope(keyed, "stacks:write", service.VerbStackWrite, seed.Org.ID))

	// A browser session carries no scopes and is not scope-checked.
	assert.NoError(t, access.RequireScope(member, "stacks:write", service.VerbStackWrite, seed.Org.ID))
}
