// Package annotate is the HTTP half of canvas annotations: bind, validate and
// write one note against a canvas owner key the caller has already authorized.
// Every canvas level (root, org, stack, env) wraps these two functions with its
// own authorization, the same shape as the node-position handlers.
package annotate

import (
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Save upserts one annotation posted as JSON. The server owns the id: a save
// without one is a create and gets a fresh uuid. The id always comes back in
// the response so the client can address the note from then on.
func Save(c echo.Context, store repo.Store, owner string) error {
	var in struct {
		ID    string  `json:"id"`
		Kind  string  `json:"kind"`
		Body  string  `json:"body"`
		X     float64 `json:"x"`
		Y     float64 `json:"y"`
		W     float64 `json:"w"`
		H     float64 `json:"h"`
		Color string  `json:"color"`
	}
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad annotation body")
	}
	if in.ID == "" {
		in.ID = uuid.New().String()
	}
	now := time.Now().UTC()
	a := &repo.Annotation{
		ID: in.ID, OwnerID: owner, Kind: in.Kind, Body: in.Body,
		X: in.X, Y: in.Y, W: in.W, H: in.H, Color: in.Color,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := repo.ValidateAnnotation(a); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if err := store.UpsertAnnotation(c.Request().Context(), a); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]string{"id": a.ID})
}

// Delete removes one annotation by id, scoped to the owner so a request can
// never delete another canvas's note.
func Delete(c echo.Context, store repo.Store, owner string) error {
	var in struct {
		ID string `json:"id"`
	}
	if err := c.Bind(&in); err != nil || in.ID == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "id required")
	}
	if err := store.DeleteAnnotation(c.Request().Context(), owner, in.ID); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}
