package admin

import (
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	ui "github.com/FyrmForge/stackr/internal/ui/drawer/admin"
)

const extBase = ui.Base + "/extensions"

// mountExtensions is the Postgres extensions section's actions. It reads
// with admin.read (the section route) and writes with serverdefaults.set.
func (h *handler) mountExtensions(g *echo.Group, a *middleware.Access) {
	w := a.Require("serverdefaults.set")
	g.POST(extBase, h.act("extensions", h.saveRecipe(false)), w)
	g.POST(extBase+"/:name", h.act("extensions", h.saveRecipe(true)), w)
	g.POST(extBase+"/:name/enabled", h.act("extensions", h.switchRecipe), w)
	g.POST(extBase+"/:name/delete", h.act("extensions", func(c echo.Context) (string, *service.Job, error) {
		return "Recipe removed.", nil, h.orch.DeletePGRecipe(c.Request().Context(), middleware.Principal(c), c.Param("name"))
	}), w)
}

// saveRecipe adds a recipe (edit false, the name is posted) or rewrites the
// one named in the path.
func (h *handler) saveRecipe(edit bool) func(echo.Context) (string, *service.Job, error) {
	return func(c echo.Context) (string, *service.Job, error) {
		f, err := c.FormParams()
		if err != nil {
			return "", nil, errs.Invalidf("name", "bad form")
		}
		v, err := recipeFromForm(f)
		if edit {
			v.Name = c.Param("name")
		}
		if err != nil {
			return "", nil, err
		}
		v.Enabled = true
		rs, err := h.orch.PGRecipes(c.Request().Context())
		if err != nil {
			return "", nil, err
		}
		i := slices.IndexFunc(rs, func(r service.PGRecipeView) bool { return r.Name == v.Name })
		switch {
		case i >= 0 && !edit:
			return "", nil, errs.Invalidf("name", "a recipe %s already exists", v.Name)
		case i >= 0: // an edit keeps the switch where it was
			v.Enabled = rs[i].Enabled
		}
		_, err = h.orch.SetPGRecipe(c.Request().Context(), middleware.Principal(c), v)
		return "Saved.", nil, err
	}
}

// switchRecipe flips a recipe's switch (?on=true or false); the recipe is
// otherwise written back as it is.
func (h *handler) switchRecipe(c echo.Context) (string, *service.Job, error) {
	ctx := c.Request().Context()
	rs, err := h.orch.PGRecipes(ctx)
	if err != nil {
		return "", nil, err
	}
	i := slices.IndexFunc(rs, func(r service.PGRecipeView) bool { return r.Name == c.Param("name") })
	if i < 0 {
		return "", nil, errs.ErrNotFound
	}
	v := rs[i]
	v.Enabled = c.QueryParam("on") == "true"
	if _, err := h.orch.SetPGRecipe(ctx, middleware.Principal(c), v); err != nil {
		return "", nil, err
	}
	if v.Enabled {
		return v.Name + " is on.", nil, nil
	}
	return v.Name + " is off. Instances that have it keep it.", nil, nil
}

// recipeFromForm reads the recipe form. Lists are one entry per line, the
// versions comma separated, sha256 as 17-amd64=hex. Values go to the service
// as they are: its checks (message 23) are the only validation.
func recipeFromForm(f url.Values) (service.PGRecipeView, error) {
	v := service.PGRecipeView{
		Name:       strings.TrimSpace(f.Get("name")),
		PackageURL: strings.TrimSpace(f.Get("package_url")),
		Version:    strings.TrimSpace(f.Get("version")),
		Packages:   lines(f.Get("packages")),
		Extensions: lines(f.Get("extensions")),
		Preload:    lines(f.Get("preload")),
		Requires:   lines(f.Get("requires")),
	}
	for _, m := range strings.FieldsFunc(f.Get("majors"), func(r rune) bool { return r == ',' || r == ' ' }) {
		n, err := strconv.Atoi(m)
		if err != nil {
			return v, errs.Invalidf("majors", "%q is not a postgres version.", m)
		}
		v.Majors = append(v.Majors, n)
	}
	for _, l := range lines(f.Get("sha256")) {
		k, sum, ok := strings.Cut(l, "=")
		if !ok {
			return v, errs.Invalidf("sha256", "%q is not like 17-amd64=hex.", l)
		}
		if v.SHA256 == nil {
			v.SHA256 = map[string]string{}
		}
		v.SHA256[strings.TrimSpace(k)] = strings.TrimSpace(sum)
	}
	switch f.Get("share") {
	case comp.ShareAll:
		v.Share = []string{comp.ShareAll}
	case comp.ShareOnly:
		v.Share = f["org"]
	}
	return v, nil
}

func lines(s string) []string {
	var out []string
	for l := range strings.SplitSeq(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// extensionsTab is the built-in recipes, the admin's own, and the add form.
func (h *handler) extensionsTab(c echo.Context) (templ.Component, error) {
	ctx := c.Request().Context()
	rs, err := h.orch.PGRecipes(ctx)
	if err != nil {
		return nil, err
	}
	ogs, err := h.orch.AllOrgs(ctx)
	if err != nil {
		return nil, err
	}
	v := ui.ExtensionsView{Add: recipeForm(service.PGRecipeView{Share: []string{comp.ShareAll}}, extBase, "ext-add", ogs)}
	fillRecipeForm(c, &v.Add)
	for _, r := range rs {
		row := ui.RecipeRow{
			Name: r.Name, Enabled: r.Enabled, Info: recipeInfo(r),
			Toggle: extBase + "/" + r.Name + "/enabled?on=" + strconv.FormatBool(!r.Enabled),
		}
		if r.Builtin {
			v.Builtins = append(v.Builtins, row)
			continue
		}
		row.Shared = comp.ShareSummary(slices.Contains(r.Share, comp.ShareAll), len(r.Share))
		ef := recipeForm(r, extBase+"/"+r.Name, "ext-"+r.Name, ogs)
		fillRecipeForm(c, &ef)
		row.Edit = &ef
		row.Delete = comp.ConfirmView{
			Button:  "Delete",
			Title:   "Delete " + r.Name + "?",
			Warning: "Refused while an instance has it enabled.",
			Action:  extBase + "/" + r.Name + "/delete",
			Target:  ui.Target,
			Quiet:   true,
		}
		v.Own = append(v.Own, row)
	}
	return ui.Extensions(v), nil
}

// fillRecipeForm draws a refused post of this form's own action as typed.
func fillRecipeForm(c echo.Context, f *ui.RecipeForm) {
	p := refill(c, f.Action)
	if p == nil {
		return
	}
	f.Refused = true
	if p.Has("name") {
		f.Name = p.Get("name")
	}
	f.Majors, f.Packages = p.Get("majors"), p.Get("packages")
	f.PackageURL, f.Version, f.SHA256 = p.Get("package_url"), p.Get("version"), p.Get("sha256")
	f.Extensions, f.Preload, f.Requires = p.Get("extensions"), p.Get("preload"), p.Get("requires")
	f.Share.Mode = p.Get("share")
	for i := range f.Share.Orgs {
		f.Share.Orgs[i].On = slices.Contains(p["org"], f.Share.Orgs[i].ID)
	}
}

func recipeForm(r service.PGRecipeView, action, id string, ogs []service.Org) ui.RecipeForm {
	f := ui.RecipeForm{
		ID: id, Action: action, Name: r.Name, Majors: majorsText(r.Majors),
		Packages: strings.Join(r.Packages, "\n"), PackageURL: r.PackageURL, Version: r.Version,
		Extensions: strings.Join(r.Extensions, "\n"), Preload: strings.Join(r.Preload, "\n"),
		Requires: strings.Join(r.Requires, "\n"),
		Share:    comp.ShareView{ModeName: "share", OrgName: "org", Mode: comp.ShareServer},
	}
	var sums []string
	for _, k := range slices.Sorted(maps.Keys(r.SHA256)) {
		sums = append(sums, k+"="+r.SHA256[k])
	}
	f.SHA256 = strings.Join(sums, "\n")
	for _, o := range ogs {
		f.Share.Orgs = append(f.Share.Orgs, comp.ShareOrg{ID: o.ID, Name: o.Name, On: slices.Contains(r.Share, o.ID)})
	}
	switch {
	case slices.Contains(r.Share, comp.ShareAll):
		f.Share.Mode = comp.ShareAll
	case len(r.Share) > 0:
		f.Share.Mode = comp.ShareOnly
	}
	return f
}

func majorsText(ms []int) string {
	s := make([]string, len(ms))
	for i, m := range ms {
		s[i] = strconv.Itoa(m)
	}
	return strings.Join(s, ", ")
}

// recipeInfo is the row's one line: the versions it builds for.
func recipeInfo(r service.PGRecipeView) string {
	if len(r.Majors) == 0 {
		return "any postgres version"
	}
	return fmt.Sprintf("postgres %s", majorsText(r.Majors))
}
