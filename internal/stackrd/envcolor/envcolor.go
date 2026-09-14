// Package envcolor resolves the colour an environment is drawn in.
//
// Six built-in hues. Well-known slugs get a fixed default (production red,
// staging amber, dev violet); every other static env takes the next unused
// hue in ladder order. Overrides, most specific wins: the env row's own
// colour (the config file on a managed stack, stack settings otherwise), the
// org's default for that slug, then these defaults.
package envcolor

import (
	"encoding/json"
	"regexp"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Names are the palette, in ladder order. Each is a --rw-env-<name> token in
// css/input.css, tuned per theme, which is why a name is stored rather than
// a hex: the same env reads right in light and dark.
var Names = []string{"teal", "sky", "lime", "amber", "rose", "violet"}

// bySlug are the defaults people expect without reading a legend: red means
// production, and violet (the accent) marks the env you hack on.
var bySlug = map[string]string{
	"production": "rose", "prod": "rose",
	"staging": "amber", "stg": "amber",
	"dev": "violet", "development": "violet",
}

var hexRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// Valid accepts a palette name or a #rrggbb value. "" is valid: it clears.
func Valid(v string) bool {
	if v == "" || hexRe.MatchString(v) {
		return true
	}
	for _, n := range Names {
		if n == v {
			return true
		}
	}
	return false
}

// CSS is the value to put in a style attribute.
func CSS(v string) string {
	if hexRe.MatchString(v) {
		return v
	}
	return "var(--rw-env-" + v + ")"
}

// Resolved is one env's colour and where it came from.
type Resolved struct {
	Value  string // palette name or hex
	CSS    string // CSS(Value)
	Source string // file | stack | org | default
}

// OrgDefaults parses the org row's slug to colour map. Nil-safe.
func OrgDefaults(org *repo.Org) map[string]string {
	out := map[string]string{}
	if org == nil || org.EnvColors == "" {
		return out
	}
	_ = json.Unmarshal([]byte(org.EnvColors), &out)
	return out
}

// Map resolves every env of one stack, keyed by env id. envs is the stack's
// list in store order (the ladder). managed says the stack's config file
// owns the env rows, which only changes what Source reports.
func Map(envs []repo.Environment, org *repo.Org, managed bool) map[string]Resolved {
	out := make(map[string]Resolved, len(envs))
	orgDefaults := OrgDefaults(org)
	// Named slugs claim their hue first, so a stack listed staging then
	// production still gets amber and red. The rest, statics before PR
	// envs, take the next hue nobody holds; past six they cycle.
	used := map[string]bool{}
	def := map[string]string{}
	for _, e := range envs {
		if h, ok := bySlug[e.Slug]; ok && e.Type != "ephemeral" {
			def[e.ID] = h
			used[h] = true
		}
	}
	next := 0
	for pass := 0; pass < 2; pass++ {
		for _, e := range envs {
			if (e.Type == "ephemeral") != (pass == 1) || def[e.ID] != "" {
				continue
			}
			h := Names[next%len(Names)]
			for tries := 0; tries < len(Names) && used[h]; tries++ {
				next++
				h = Names[next%len(Names)]
			}
			next++
			used[h] = true
			def[e.ID] = h
		}
	}
	for _, e := range envs {
		r := Resolved{Value: def[e.ID], Source: "default"}
		if v := orgDefaults[e.Slug]; Valid(v) && v != "" {
			r = Resolved{Value: v, Source: "org"}
		}
		if Valid(e.Color) && e.Color != "" {
			r = Resolved{Value: e.Color, Source: "stack"}
			if managed {
				r.Source = "file"
			}
		}
		r.CSS = CSS(r.Value)
		out[e.ID] = r
	}
	return out
}

// For resolves one env. envs must be the stack's full list.
func For(env *repo.Environment, envs []repo.Environment, org *repo.Org, managed bool) Resolved {
	if r, ok := Map(envs, org, managed)[env.ID]; ok {
		return r
	}
	return Resolved{Value: Names[0], CSS: CSS(Names[0]), Source: "default"}
}

// BySlug is Map keyed by slug, for callers that hold a slug (plan rows).
func BySlug(envs []repo.Environment, org *repo.Org, managed bool) map[string]Resolved {
	m := Map(envs, org, managed)
	out := make(map[string]Resolved, len(m))
	for _, e := range envs {
		out[e.Slug] = m[e.ID]
	}
	return out
}
