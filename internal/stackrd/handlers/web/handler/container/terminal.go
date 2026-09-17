package container

import (
	"context"
	"io"
	"net/http"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/components"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/wsterm"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/cluster"
)

// GET /containers/:id/term, terminal page.
func (h *handler) TermPage(c echo.Context) error {
	ctx, cancel := components.PageCtx(c.Request().Context())
	defer cancel()
	d, down, err := h.inspect(ctx, c)
	if err != nil {
		return err
	}
	return respond.HTML(c, http.StatusOK, termPage(c, d, h.node(c), down))
}

// GET /containers/:id/term/ws, websocket bridged to docker exec TTY.
func (h *handler) TermWS(c echo.Context) error {
	return wsterm.Serve(c, nodeExec{c: h.clus, node: h.node(c)}, c.Param("id"), nil)
}

// nodeExec binds the cluster to one node so it fits the single-method
// interface wsterm asks for. wsterm bridges a browser to an exec and has no
// business knowing which machine the exec is on.
type nodeExec struct {
	c    *cluster.Cluster
	node string
}

func (e nodeExec) ExecTTY(ctx context.Context, id string, cmd []string) (io.ReadWriter, func(w, h uint) error, func(), error) {
	return e.c.ExecTTY(ctx, e.node, id, cmd)
}
