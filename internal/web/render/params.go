package render

import (
	"sort"
	"strings"

	"github.com/FyrmForge/stackr/internal/service"
)

// ParamEntries reads the params editor's form (ui/components ParamEditor):
// param.<collection>.<name>, secret.<collection>.<name> (empty = keep the
// stored one) and the new_ row. Every scope's editor posts it.
func ParamEntries(form map[string][]string) []service.ParamEntry {
	var out []service.ParamEntry
	for k, vs := range form {
		kind, rest, ok := strings.Cut(k, ".")
		coll, name, ok2 := strings.Cut(rest, ".")
		if !ok || !ok2 || (kind != "param" && kind != "secret") {
			continue
		}
		out = append(out, service.ParamEntry{
			Collection: coll,
			Name:       name,
			Kind:       kind,
			Value:      vs[0],
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Collection+"."+out[i].Name < out[j].Collection+"."+out[j].Name
	})
	if n := strings.TrimSpace(first(form["new_name"])); n != "" {
		out = append(out, service.ParamEntry{
			Collection: strings.TrimSpace(first(form["new_collection"])),
			Name:       n,
			Kind:       first(form["new_kind"]),
			Value:      first(form["new_value"]),
		})
	}
	return out
}

func first(vs []string) string {
	if len(vs) == 0 {
		return ""
	}
	return vs[0]
}
