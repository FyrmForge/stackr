package app

import (
	"context"
	"net/http"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// editGate is what stands between the panel and every field the config file
// owns, so it is worth pinning all three answers: a UI-managed stack stages,
// a config-managed one refuses unless it declares ui_edits: stage.
func TestEditGate(t *testing.T) {
	ctx := context.Background()

	t.Run("ui-managed stages", func(t *testing.T) {
		store := testdb.New(t)
		seed := testdb.SeedStack(t, store, false)
		h := &handler{store: store, gate: service.NewGateService(store)}
		stage, err := h.editGate(ctx, seed.Stack.ID)
		require.NoError(t, err, "ui-managed stack was refused")
		assert.True(t, stage, "ui-managed stack did not stage")
	})

	t.Run("config-managed refuses by default", func(t *testing.T) {
		store := testdb.New(t)
		seed := testdb.SeedStack(t, store, true)
		h := &handler{store: store, gate: service.NewGateService(store)}
		stage, err := h.editGate(ctx, seed.Stack.ID)
		assert.False(t, stage, "a blocked stack must not stage")
		he, ok := err.(*echo.HTTPError)
		require.True(t, ok, "want an HTTP error, got %v", err)
		assert.Equal(t, http.StatusConflict, he.Code, "code = %d, want 409", he.Code)
	})

	t.Run("config-managed with ui_edits stage", func(t *testing.T) {
		store := testdb.New(t)
		seed := testdb.SeedStack(t, store, true)
		seed.Stack.UIEditsMode = repo.UIEditsStage
		require.NoError(t, store.UpdateStack(ctx, seed.Stack), "update stack")
		h := &handler{store: store, gate: service.NewGateService(store)}
		stage, err := h.editGate(ctx, seed.Stack.ID)
		require.NoError(t, err, "ui_edits: stage was still refused")
		assert.True(t, stage, "ui_edits: stage did not stage")
	})

	// Fails closed: a stack that cannot be read is not an allowed write.
	t.Run("missing stack refuses", func(t *testing.T) {
		store := testdb.New(t)
		h := &handler{store: store, gate: service.NewGateService(store)}
		_, err := h.editGate(ctx, "nope")
		he, ok := err.(*echo.HTTPError)
		require.True(t, ok, "want an HTTP error, got %v", err)
		assert.Equal(t, http.StatusNotFound, he.Code, "code = %d, want 404", he.Code)
	})
}
