package canvas

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	orgui "github.com/FyrmForge/stackr/internal/ui/drawer/org"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// orgRegistry is the Registry tab: the login and push lines, then the org's
// repositories; ?repo= opens that repository's versions. A registry that does
// not answer is the tab's text.
func (h *handler) orgRegistry(c echo.Context, cd card) (templ.Component, error) {
	ctx := c.Request().Context()
	base := urlOf(cd.s) + "/-/drawer"
	v := orgui.RegistryView{Host: h.orch.RegistryHost(ctx), Org: cd.s.Org.Slug}
	if repo := c.QueryParam("repo"); repo != "" {
		return h.orgVersions(c, cd, v, base, repo)
	}
	repos, err := h.orch.OrgRepositories(ctx, cd.s.Org.ID)
	if err != nil {
		v.Err = err.Error()
		return orgui.Registry(v), nil
	}
	for _, r := range repos {
		row := orgui.RepoRow{
			Name: r.Name,
			Tags: fmt.Sprintf("%d tags", r.Tags),
			Open: base + "?tab=registry&repo=" + url.QueryEscape(r.Name),
		}
		if r.Tags == 1 {
			row.Tags = "1 tag"
		}
		if r.Latest != "" {
			row.Latest = r.Latest + " · " + ago(r.LatestAt)
			row.Size = render.Size(r.Size)
		}
		v.Repos = append(v.Repos, row)
	}
	return orgui.Registry(v), nil
}

// orgVersions is one repository's versions, with the envs that run each. The
// delete button is there for whoever may delete; for a version in use its
// warning names the envs and the post forces, which the service re-checks.
func (h *handler) orgVersions(c echo.Context, cd card, v orgui.RegistryView, base, repo string) (templ.Component, error) {
	v.Repo, v.Back = repo, base+"?tab=registry"
	vs, err := h.orch.RegistryVersions(c.Request().Context(), cd.s.Org.ID, repo)
	if errors.Is(err, errs.ErrNotFound) {
		return nil, err
	}
	if err != nil {
		v.Err = err.Error()
		return orgui.Registry(v), nil
	}
	mayDelete := can(c, cd.s, "registry.tag.delete")
	for _, x := range vs {
		row := orgui.VersionRow{
			Tag:       x.Tag,
			Size:      "n/a",
			Platforms: strings.Join(x.Platforms, ", "),
			Source:    sourceOf(x),
			UsedBy:    x.UsedBy,
		}
		if x.Size > 0 {
			row.Size = render.Size(x.Size)
		}
		if !x.CreatedAt.IsZero() {
			row.Arrived = ago(x.CreatedAt)
		}
		if mayDelete {
			row.Delete = comp.ConfirmView{
				Button:  "Delete",
				Title:   "Delete " + repo + ":" + x.Tag + "?",
				Warning: "It cannot be pulled again.",
				Action:  base + "/registry/delete?repo=" + url.QueryEscape(repo) + "&tag=" + url.QueryEscape(x.Tag),
				Target:  "#" + comp.DrawerRoot,
				Quiet:   true,
			}
			if len(x.UsedBy) > 0 {
				row.Delete.Warning = "Running in " + strings.Join(x.UsedBy, ", ") + ". Those envs cannot pull it again."
				row.Delete.Action += "&force=true"
			}
		}
		v.Versions = append(v.Versions, row)
	}
	return orgui.Registry(v), nil
}

func sourceOf(v service.RegistryVersion) string {
	switch v.Source {
	case "built":
		return "built"
	case "imported":
		return "imported from " + v.UpstreamRef
	case "system":
		return "system"
	}
	return "pushed"
}

// mountRegistry is the Registry tab's one action, for whoever may delete a
// version (the tab shows it again, on the same repository).
func (h *handler) mountRegistry(site *echo.Group, a *middleware.Access) {
	site.POST("/:org/-/drawer/registry/delete", h.orgAction("registry", func(c echo.Context, og *service.Org) (string, error) {
		repo, tag := c.QueryParam("repo"), c.QueryParam("tag")
		err := h.orch.DeleteRegistryVersion(c.Request().Context(), og.ID, repo, tag, c.QueryParam("force") == "true")
		return "Deleted " + repo + ":" + tag + ".", err
	}), a.Require("registry.tag.delete"))
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	case d < 48*time.Hour:
		return "yesterday"
	}
	return t.Format("Jan 2")
}
