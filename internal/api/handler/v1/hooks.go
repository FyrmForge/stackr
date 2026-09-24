package v1

import (
	"io"
	"net/http"

	"github.com/labstack/echo/v4"
)

// hookLimit caps a delivery; GitHub's own cap is 25MB, a push of a normal
// size is far below this.
const hookLimit = 1 << 20

// Hook takes a GitHub delivery for one connector. The signature is the
// auth: it sits outside /api/v1, and the verb checks it (401 when bad, 400
// when the payload is not what the event says).
func (h *H) Hook(c echo.Context) error {
	body, err := io.ReadAll(io.LimitReader(c.Request().Body, hookLimit+1))
	if err != nil {
		return err
	}
	if len(body) > hookLimit {
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge)
	}
	r := c.Request()
	if err := h.S.Webhook(rc(c), c.Param("connector"), r.Header.Get("X-GitHub-Event"),
		r.Header.Get("X-Hub-Signature-256"), body); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}
