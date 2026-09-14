package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

func ctx() echo.Context {
	return echo.New().NewContext(httptest.NewRequest(http.MethodPost, "/", nil), httptest.NewRecorder())
}

// RequireUnmanaged is the shared discriminator behind every web-side structural
// write guard. The unknown-stack case matters most: it must fail closed, since
// "can't tell" on a locked stack has to block, not allow.
func TestRequireUnmanaged(t *testing.T) {
	store := testdb.New(t)
	ui := testdb.SeedStack(t, store, false)

	require.NoError(t, stackrmw.RequireUnmanaged(ctx(), store, ui.Stack.ID), "ui-managed stack: want nil")

	store2 := testdb.New(t)
	managed := testdb.SeedStack(t, store2, true)
	err := stackrmw.RequireUnmanaged(ctx(), store2, managed.Stack.ID)
	he, ok := err.(*echo.HTTPError)
	require.True(t, ok, "config-managed stack: want 409, got %v", err)
	require.Equal(t, http.StatusConflict, he.Code, "config-managed stack: want 409, got %v", err)

	err = stackrmw.RequireUnmanaged(ctx(), store, "no-such-stack")
	he, ok = err.(*echo.HTTPError)
	require.True(t, ok, "unknown stack: want 404 (fail closed), got %v", err)
	require.Equal(t, http.StatusNotFound, he.Code, "unknown stack: want 404 (fail closed), got %v", err)
}
