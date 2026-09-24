// Package cliauth is the browser half of `stackr login`: the CLI opens
// /cli/authorize with a loopback port and a state, the signed-in user picks
// an org, and the page sends a one-time code to the CLI's listener. The key
// itself never passes through the browser (ExchangeCLICode).
package cliauth

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/web/render"
)

type handler struct{ orch *service.Orchestrator }

func NewHandler(orch *service.Orchestrator) *handler { return &handler{orch: orch} }

// port is the CLI's loopback port, or "" when the value is not one.
func port(raw string) string {
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != raw {
		return ""
	}
	return raw
}

// GET /cli/authorize?port=&state=&name=
func (h *handler) Page(c echo.Context) error {
	v := View{Name: c.QueryParam("name"), Port: port(c.QueryParam("port")), State: c.QueryParam("state")}
	if v.Port == "" || v.State == "" {
		v.Error = "This link is not one the stackr CLI made. Run stackr login again."
		return render.Page(c, http.StatusBadRequest, "CLI login", page(v))
	}
	orgs, err := h.orch.Orgs(c.Request().Context(), middleware.Principal(c).User.ID)
	if err != nil {
		return middleware.HTTPError(err)
	}
	for _, o := range orgs {
		v.Orgs = append(v.Orgs, Org{Name: o.Name, Action: "/cli/authorize/" + o.Slug})
	}
	return render.Page(c, http.StatusOK, "CLI login", page(v))
}

// POST /cli/authorize/:org (org.read): mint the code and send the browser
// to the CLI's listener on this machine, never anywhere else.
func (h *handler) Approve(c echo.Context) error {
	p := port(c.FormValue("port"))
	state := c.FormValue("state")
	if p == "" || state == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "not a CLI login")
	}
	code, err := h.orch.CLICode(c.Request().Context(), middleware.Principal(c).User.ID, middleware.ScopeOf(c).Org.ID, c.FormValue("name"))
	if err != nil {
		return middleware.HTTPError(err)
	}
	q := url.Values{"code": {code}, "state": {state}}
	c.Response().Header().Set("HX-Redirect", "http://127.0.0.1:"+p+"/?"+q.Encode())
	return c.NoContent(http.StatusOK)
}
