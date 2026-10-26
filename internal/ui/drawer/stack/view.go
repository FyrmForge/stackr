package stack

import (
	"strconv"
	"strings"

	"github.com/FyrmForge/stackr/internal/ui/access"
	c "github.com/FyrmForge/stackr/internal/ui/components"
	"github.com/FyrmForge/stackr/internal/ui/drawer/env"
)

// Rows are the Settings tab's accordion rows, in the drawer's order.
func Rows(v SettingsView) []c.AccRow {
	rs := []c.AccRow{
		{Key: "general", Title: "General", Summary: GeneralSummary(v.Name, v.Slug), Body: general(v)},
		{Key: "order", Title: "Env order", Summary: OrderSummary(v.Order.Rungs), Body: order(v.Order)},
		{Key: "config", Title: "Config as code", Summary: ConfigSummary(v.Repo, v.Branch, v.Path), Changed: v.Repo != "", Body: config(v)},
		{Key: "resources", Title: "Resources", Summary: ResourcesSummary(v.Cascade.Rows), Changed: anyChanged(v.Cascade.Rows), Body: resources(v.Cascade)},
		{Key: "variables", Title: "Variables", Summary: v.Vars, Changed: v.VarsSet, Body: v.VarsBody},
		{Key: "placement", Title: "Placement", Summary: v.Placement, Changed: v.PlacementSet, Body: v.PlacementBody},
		{Key: "host", Title: "Host access", Summary: HostSummary(v.Host.Tiles), Changed: granted(v.Host.Tiles), Body: host(v.Host)},
	}
	if v.Base != "" {
		rs = append(rs, c.AccRow{Key: "delete", Title: "Delete", Summary: DeleteSummary(len(v.Order.Rungs)), Body: danger(v)})
	}
	return rs
}

// DeleteSummary is what a delete takes: "Holds 2 envs", "No envs".
func DeleteSummary(envs int) string {
	if envs == 0 {
		return "No envs"
	}
	return "Holds " + plural(envs, "env")
}

func GeneralSummary(name, slug string) string {
	if slug == "" || slug == name {
		return name
	}
	return name + " · " + slug
}

// OrderSummary is the ladder's names, bottom rung first.
func OrderSummary(rs []Rung) string {
	if len(rs) == 0 {
		return "no environments"
	}
	names := make([]string, len(rs))
	for i, r := range rs {
		names[i] = r.Name
	}
	return strings.Join(names, ", ")
}

// ConfigSummary is the bound repository, short, and the file it reads.
func ConfigSummary(repo, branch, path string) string {
	if repo == "" {
		return "managed from the UI"
	}
	r := strings.TrimSuffix(strings.TrimPrefix(repo, "https://"), ".git")
	if host, rest, ok := strings.Cut(r, "/"); ok && strings.Contains(host, ".") {
		r = rest
	}
	parts := []string{r}
	if branch != "" {
		parts = append(parts, branch)
	}
	if path == "" {
		path = "stackr-compose.yml"
	}
	return strings.Join(append(parts, path), " · ")
}

// ResourcesSummary is the values in effect, as the env's row says them:
// "2 cores · 512 MB (from org) · protect off".
func ResourcesSummary(rows []c.SettingRowView) string { return env.ResourcesSummary(rows) }

// granted: a server admin gave a tile host access, which no tile has unset.
func granted(ts []access.TilePerms) bool {
	for _, t := range ts {
		for _, p := range t.Perms {
			if !p.Waiting {
				return true
			}
		}
	}
	return false
}

func anyChanged(rows []c.SettingRowView) bool {
	for _, r := range rows {
		if r.Changed {
			return true
		}
	}
	return false
}


// PlacementSummary is where the stack runs: its own pin, the org's, or
// the default.
func PlacementSummary(pin, inherited, ingress string) string {
	s := "inherits org (default)"
	switch {
	case pin != "":
		s = "runs on " + pin
	case inherited != "":
		s = "runs on " + inherited + " (from org)"
	}
	if ingress != "" {
		s += " · ingress " + ingress
	}
	return s
}

// HostSummary counts the grants per tile and what still waits.
func HostSummary(ts []access.TilePerms) string {
	granted, waiting := 0, 0
	var tiles []string
	for _, t := range ts {
		g := 0
		for _, p := range t.Perms {
			if p.Waiting {
				waiting++
			} else {
				g++
			}
		}
		if g > 0 {
			tiles = append(tiles, t.Tile)
			granted += g
		}
	}
	var parts []string
	if granted > 0 {
		parts = append(parts, plural(granted, "grant")+" on "+strings.Join(tiles, ", "))
	}
	if waiting > 0 {
		parts = append(parts, strconv.Itoa(waiting)+" waiting")
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, " · ")
}

func plural(n int, word string) string {
	if n != 1 {
		word += "s"
	}
	return strconv.Itoa(n) + " " + word
}
