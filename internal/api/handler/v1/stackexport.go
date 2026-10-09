package v1

import (
	"net/http"

	"github.com/labstack/echo/v4"
)

// ExportStack answers the stack (or the ?env= one) as a stackr-compose.yml download.
func (h *H) ExportStack() Endpoint {
	return Streamed("application/yaml", func(c echo.Context) error {
		b, err := h.Orch.ExportStack(rc(c), stackID(c), c.QueryParam("env"))
		if err != nil {
			return err
		}
		c.Response().Header().Set(echo.HeaderContentDisposition, `attachment; filename="stackr-compose.yml"`)
		return c.Blob(http.StatusOK, "application/yaml", b)
	})
}
