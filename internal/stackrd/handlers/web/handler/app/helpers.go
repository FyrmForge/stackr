package app

import (
	"encoding/json"

	"github.com/FyrmForge/stackr/internal/stackrd/config/varref"
)

// isRef reports whether a stored value contains a ${{ ... }} reference, so the
// view can badge it instead of showing it as an opaque literal.
func isRef(v string) bool { return varref.HasRef(v) }

// refsJSON serialises the reference catalogue for the editor's autocomplete.
// Metadata only, the catalogue carries no values, secret or otherwise.
func refsJSON(sources []varref.Source) string {
	type out struct {
		Expr   string `json:"expr"`
		Label  string `json:"label"`
		Scope  string `json:"scope"`
		Secret bool   `json:"secret"`
	}
	var list []out
	for _, s := range sources {
		for _, o := range s.Outputs {
			label := s.Scope
			if s.Slug != "" {
				label += " · " + s.Slug
			}
			list = append(list, out{Expr: s.Expr(o.Name), Label: o.Name + ": " + label, Scope: s.Scope, Secret: o.Secret})
		}
	}
	b, err := json.Marshal(list)
	if err != nil {
		return "[]"
	}
	return string(b)
}
