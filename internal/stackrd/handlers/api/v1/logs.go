package v1

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"
)

func (a *API) getAppLogs(c echo.Context) error {
	t, err := a.tile(c, c.Param("id"))
	if err != nil {
		return err
	}
	tail := 200
	if n, err := strconv.Atoi(c.QueryParam("tail")); err == nil && n > 0 && n <= 5000 {
		tail = n
	}
	follow := c.QueryParam("follow") == "1" || c.QueryParam("follow") == "true"
	ctx := c.Request().Context()
	// Where the lines come from is the telemetry service's answer, shared
	// with the panel's SSE handler so the two cannot pick different sources.
	src := a.telemetry.Logs(ctx, t)
	if src.Service != "" {
		if follow {
			return a.streamServiceLogs(c, src.Service, tail)
		}
		if out, err := a.clus.ServiceLogs(ctx, src.Service, tail); err == nil {
			return c.JSON(http.StatusOK, logsOut{Lines: out})
		}
		// Fall through: a tile that has never deployed has no service, and a
		// cron tile's history lives on its runs, not here.
		src.Container = a.telemetry.Container(ctx, t)
	}
	if src.Container == "" {
		if follow {
			// not running → nothing to follow; poll-until-started if a CLI ever needs it
			return c.NoContent(http.StatusNoContent)
		}
		return c.JSON(http.StatusOK, logsOut{Lines: ""})
	}
	if follow {
		return a.streamAppLogs(c, src.Container, tail)
	}
	out, err := a.clus.Logs(ctx, src.Container, tail)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, logsOut{Lines: out})
}

// streamAppLogs follows container logs as SSE. Each event is "data: <mark> <line>"
// where mark is O (stdout) or E (stderr). Ends when the client disconnects or the
// container stops.
func (a *API) streamServiceLogs(c echo.Context, service string, tail int) error {
	lines, stop, err := a.clus.StreamServiceLogsMarked(c.Request().Context(), service, tail)
	if err != nil {
		return err
	}
	return a.pumpLogs(c, lines, stop)
}

func (a *API) streamAppLogs(c echo.Context, containerID string, tail int) error {
	ctx := c.Request().Context()
	lines, stop, err := a.clus.StreamLogsMarked(ctx, a.clus.Self(ctx), containerID, tail)
	if err != nil {
		return err
	}
	return a.pumpLogs(c, lines, stop)
}

// pumpLogs turns a marked line channel into the SSE stream, whatever produced
// it, one container's logs or a whole service's.
func (a *API) pumpLogs(c echo.Context, lines <-chan string, stop func()) error {
	defer stop()
	ctx := c.Request().Context()
	w := c.Response()
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	w.Flush()

	ping := time.NewTicker(25 * time.Second) // keep idle proxies from dropping the connection
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ping.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return nil
			}
			w.Flush()
		case line, ok := <-lines:
			if !ok {
				return nil // container stopped / stream ended
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", line); err != nil {
				return nil
			}
			w.Flush()
		}
	}
}

func (a *API) getDeploymentLogs(c echo.Context) error {
	d, err := a.loadDeployment(c, c.Param("id"))
	if err != nil {
		return err
	}
	out, _ := a.engine.DeployLog(d.ID) // empty when no log file yet
	return c.JSON(http.StatusOK, logsOut{Lines: out})
}
