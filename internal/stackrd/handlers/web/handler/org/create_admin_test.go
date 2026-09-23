package org

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// Creating an org makes the creator its owner, so an open create is a
// self-service path to owning something with its own connector, stacks and
// secrets. The isolated-org case needs users who belong to exactly the orgs an
// admin put them in, which is only true if they cannot make their own.
//
// The route carries adminOnly as well; this is the handler half, so a second
// route reaching Create cannot reopen it by omission.
func TestCreateOrgIsAdminOnly(t *testing.T) {
	s := testdb.New(t)
	ctx := context.Background()
	now := time.Now().UTC()

	plain := &repo.User{ID: "u1", Email: "u@example.com", Name: "U", Role: "user", Active: true, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, s.CreateUser(ctx, plain))
	admin := &repo.User{ID: "a1", Email: "a@example.com", Name: "A", Role: "admin", Active: true, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, s.CreateUser(ctx, admin))

	seed := testdb.SeedStack(t, s, false)
	orgs := []repo.Org{*seed.Org}
	h := NewHandler(s, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil).
		WithDomainResources(service.NewDomainResourceService(s, nil)).
		WithPlans(service.NewPlanService(s, nil, nil)).
		WithGraph(service.NewGraphService(s)).
		WithConnectors(service.NewConnectorService(s))

	before, err := s.ListOrgs(ctx)
	require.NoError(t, err)

	// Not-found rather than forbidden, matching adminOnly: a plain user has no
	// business learning the route is there.
	c := asUser(t, http.MethodPost, "mode=ui", plain, orgs, "owner")
	wantStatus(t, "plain user creating an org", h.Create(c), http.StatusNotFound)

	after, err := s.ListOrgs(ctx)
	require.NoError(t, err)
	require.Len(t, after, len(before), "refused create must not have written an org")

	// The admin still gets through, or the install has no way to make one.
	c = asUser(t, http.MethodPost, "mode=ui", admin, orgs, "owner")
	require.NoError(t, h.Create(c), "admin creating an org")

	made, err := s.ListOrgs(ctx)
	require.NoError(t, err)
	require.Len(t, made, len(before)+1, "admin create must have written an org")
}
