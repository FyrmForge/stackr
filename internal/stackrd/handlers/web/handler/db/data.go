package db

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/managedtiles"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// The data browser (Railway-style tables/rows grid). One fragment serves two
// scopes, both resolved to postgres credentials so grants enforce the
// boundary:
//   - the instance drawer's Data tab, superuser, with a database picker
//     (its own db + every provisioned one)
//   - a provisioned slice's drawer, that provision's own user, its db only
//
// "locked" (which hides the picker) is a display flag off the request, not a
// containment boundary: credentials follow the chosen database, and both
// drawers hang off the same tile, so anyone who can open the slice can open
// the instance. Tenancy is the tile's org check in load(), nothing here.

const pgPageSize = 100

// dataScope is one resolved browser target.
type dataScope struct {
	d      *repo.Tile
	creds  managedtiles.PGCreds
	dbs    []string // the instance's databases (its own first)
	base   string   // the fragment's own URL
	locked bool     // slice drawer: one db only, no database level
	chosen bool     // a database is selected (creds are valid)
}

// dataView is everything the fragment renders. The browser drills down:
// databases (instance scope only) -> tables -> rows.
type dataView struct {
	sc       *dataScope
	err      string // load failure, nothing below it rendered
	banner   string // a failed update/insert/delete, grid stays up
	atDBs    bool   // level 1: the database list
	tables   []string
	table    string // set = level 3, the grid; empty = level 2, the table list
	page     int
	total    int64
	cols     []string
	rows     [][]string
	canWrite bool
}

// loadData resolves :id + ?db= into credentials. A db that is neither the
// instance's own nor a provisioned one 404s.
func (h *handler) loadData(c echo.Context) (*dataScope, error) {
	d, err := h.load(c)
	if err != nil {
		return nil, err
	}
	if !managedtiles.Engines[d.Engine].DataBrowser {
		return nil, echo.NewHTTPError(http.StatusNotFound, "this engine has no data browser")
	}
	ps, err := h.store.ListProvisionsByInstance(c.Request().Context(), d.ID)
	if err != nil {
		return nil, err
	}
	sc := &dataScope{d: d, base: "/dbs/" + d.ID + "/data", dbs: []string{d.DBName}}
	for _, p := range ps {
		if !contains(sc.dbs, p.DBName) {
			sc.dbs = append(sc.dbs, p.DBName)
		}
	}
	sc.locked = firstOf(c.FormValue("locked"), c.QueryParam("locked")) != ""
	db := firstOf(c.FormValue("db"), c.QueryParam("db"))
	if db == "" {
		if sc.locked {
			return nil, echo.NewHTTPError(http.StatusNotFound, "no database selected")
		}
		return sc, nil // level 1: the database list, no connection yet
	}
	sc.chosen = true
	if db == d.DBName {
		sc.creds = managedtiles.PGCreds{User: d.DBUser, Password: d.DBPassword, DB: d.DBName}
		return sc, nil
	}
	for _, p := range ps {
		if p.DBName == db {
			sc.creds = managedtiles.PGCreds{User: p.DBUser, Password: p.DBPassword, DB: p.DBName}
			return sc, nil
		}
	}
	return nil, echo.NewHTTPError(http.StatusNotFound, "no such database on this instance")
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// render loads tables + the current page and renders the fragment.
func (h *handler) renderData(c echo.Context, sc *dataScope) error {
	ctx := c.Request().Context()
	v := dataView{sc: sc, canWrite: stackrmw.CanWriteHere(c)}
	v.banner, _ = c.Get(ctxDataErr).(string)
	if !sc.chosen {
		v.atDBs = true // level 1: list the instance's databases
		return respond.HTML(c, http.StatusOK, dataBrowser(c, v))
	}
	tables, err := h.dbs.PGTables(ctx, sc.d, sc.creds)
	if err != nil {
		v.err = err.Error()
		return respond.HTML(c, http.StatusOK, dataBrowser(c, v))
	}
	v.tables = tables
	v.table = firstOf(c.FormValue("table"), c.QueryParam("table"))
	if v.table != "" && !contains(tables, v.table) {
		v.table = ""
	}
	if v.table == "" {
		// level 2: the table list
		return respond.HTML(c, http.StatusOK, dataBrowser(c, v))
	}
	v.page, _ = strconv.Atoi(firstOf(c.FormValue("page"), c.QueryParam("page")))
	if v.page < 1 {
		v.page = 1
	}
	v.cols, v.rows, v.total, err = h.dbs.PGSelect(ctx, sc.d, sc.creds, v.table, pgPageSize, (v.page-1)*pgPageSize)
	if err != nil {
		v.err = err.Error()
	}
	return respond.HTML(c, http.StatusOK, dataBrowser(c, v))
}

func firstOf(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// GET /dbs/:id/data?db=&table=&page=, the browser fragment.
func (h *handler) Data(c echo.Context) error {
	sc, err := h.loadData(c)
	if err != nil {
		return err
	}
	return h.renderData(c, sc)
}

// GET /dbs/:id/data/cell?db=&table=&page=&ctid=&col=&val=, inline edit form
// for one cell (swapped into its <td>).
func (h *handler) DataCell(c echo.Context) error {
	sc, err := h.loadData(c)
	if err != nil {
		return err
	}
	return respond.HTML(c, http.StatusOK, dataCellForm(c, sc,
		c.QueryParam("table"), c.QueryParam("page"),
		c.QueryParam("ctid"), c.QueryParam("col"), c.QueryParam("val")))
}

// POST /dbs/:id/data/update, write one cell, re-render the fragment.
func (h *handler) DataUpdate(c echo.Context) error {
	sc, err := h.loadData(c)
	if err != nil {
		return err
	}
	err = h.dbs.PGUpdateCell(c.Request().Context(), sc.d, sc.creds,
		c.FormValue("table"), c.FormValue("ctid"), c.FormValue("col"),
		c.FormValue("value"), c.FormValue("null") != "")
	if err != nil {
		return h.renderDataErr(c, sc, err)
	}
	return h.renderData(c, sc)
}

// POST /dbs/:id/data/delete, delete the row at ctid.
func (h *handler) DataDelete(c echo.Context) error {
	sc, err := h.loadData(c)
	if err != nil {
		return err
	}
	err = h.dbs.PGDeleteRow(c.Request().Context(), sc.d, sc.creds,
		c.FormValue("table"), c.FormValue("ctid"))
	if err != nil {
		return h.renderDataErr(c, sc, err)
	}
	return h.renderData(c, sc)
}

// POST /dbs/:id/data/insert, insert one row from the v_<col> form fields;
// empty inputs fall back to column defaults.
func (h *handler) DataInsert(c echo.Context) error {
	sc, err := h.loadData(c)
	if err != nil {
		return err
	}
	form, err := c.FormParams()
	if err != nil {
		return err
	}
	var cols, vals []string
	for name, vs := range form {
		col, ok := strings.CutPrefix(name, "v_")
		if !ok || len(vs) == 0 || vs[0] == "" {
			continue
		}
		cols = append(cols, col)
		vals = append(vals, vs[0])
	}
	err = h.dbs.PGInsertRow(c.Request().Context(), sc.d, sc.creds, c.FormValue("table"), cols, vals)
	if err != nil {
		return h.renderDataErr(c, sc, err)
	}
	return h.renderData(c, sc)
}

// renderDataErr re-renders the fragment with the failed statement's error on
// top, the grid stays usable underneath.
func (h *handler) renderDataErr(c echo.Context, sc *dataScope, opErr error) error {
	c.Set(ctxDataErr, opErr.Error())
	return h.renderData(c, sc)
}

const ctxDataErr = "data_op_err"

// GET /dbs/:id/pgdbs/:db/panel, the drawer behind a postgres slice card:
// that provisioned database only, browsed with its own credentials.
func (h *handler) PGDBPanel(c echo.Context) error {
	d, err := h.load(c)
	if err != nil {
		return err
	}
	if !managedtiles.Engines[d.Engine].DataBrowser {
		return echo.NewHTTPError(http.StatusNotFound, "this engine has no data browser")
	}
	dbName := c.Param("db")
	ps, err := h.store.ListProvisionsByInstance(c.Request().Context(), d.ID)
	if err != nil {
		return err
	}
	var found *repo.Provision
	for i := range ps {
		if ps[i].DBName == dbName {
			found = &ps[i]
			break
		}
	}
	if found == nil {
		return echo.NewHTTPError(http.StatusNotFound, "database not found")
	}
	return respond.HTML(c, http.StatusOK, pgDBPanel(d, found, c.QueryParam("tab")))
}
