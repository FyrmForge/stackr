package annotate

import (
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// SaveGroup upserts one graph group posted as JSON, same contract as Save:
// the server mints the id on create and always returns it.
func SaveGroup(c echo.Context, graph *service.GraphService, owner string) error {
	var in struct {
		ID      string   `json:"id"`
		Members []string `json:"members"`
	}
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad group body")
	}
	if in.ID == "" {
		in.ID = uuid.New().String()
	}
	now := time.Now().UTC()
	g := &repo.GraphGroup{
		ID: in.ID, OwnerID: owner, Members: in.Members,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := repo.ValidateGraphGroup(g); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if err := graph.SaveGroup(c.Request().Context(), g); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]string{"id": g.ID})
}

// DeleteGroup removes one group by id, scoped to the owner.
func DeleteGroup(c echo.Context, graph *service.GraphService, owner string) error {
	var in struct {
		ID string `json:"id"`
	}
	if err := c.Bind(&in); err != nil || in.ID == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "id required")
	}
	if err := graph.DeleteGroup(c.Request().Context(), owner, in.ID); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}
