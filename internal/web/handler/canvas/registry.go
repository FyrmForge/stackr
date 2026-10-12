package canvas

import (
	"fmt"
	"time"

	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	orgui "github.com/FyrmForge/stackr/internal/ui/drawer/org"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// orgRegistry is the Registry tab: the login and push lines, then the org's
// repositories. A registry that does not answer is the tab's text.
func (h *handler) orgRegistry(c echo.Context, cd card) (templ.Component, error) {
	ctx := c.Request().Context()
	v := orgui.RegistryView{Host: h.orch.RegistryHost(ctx), Org: cd.s.Org.Slug}
	repos, err := h.orch.OrgRepositories(ctx, cd.s.Org.ID)
	if err != nil {
		v.Err = err.Error()
		return orgui.Registry(v), nil
	}
	for _, r := range repos {
		row := orgui.RepoRow{Name: r.Name, Tags: fmt.Sprintf("%d tags", r.Tags)}
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
