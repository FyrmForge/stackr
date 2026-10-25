// Package slug is the one grammar for names that end up in URLs, DNS aliases
// and refs (DECIDE 5 a): org, stack, env and tile slugs are [a-z0-9-]
// (a tile slug is a DNS alias, where "_" is not allowed); param collection
// and param names are [a-z0-9_]. Leaves share it, so it lives outside leaf/.
package slug

import (
	"regexp"
	"strings"
)

var (
	slugRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	nameRe = regexp.MustCompile(`^[a-z0-9_]+$`)
	envRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Make turns a display name into a slug: lower case, every run of anything
// that is not a letter or digit becomes one hyphen, hyphens trimmed. "" when
// the name has no letter or digit.
func Make(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
			continue
		}
		dash = true
	}
	return b.String()
}

// Valid reports whether s is a slug.
func Valid(s string) bool {
	return slugRe.MatchString(s)
}

// Reserved is a slug a ref or a share list would read as a keyword: `params`
// ("Param store and refs") and `all` (a connector shared with every org).
func Reserved(s string) bool {
	return s == "params" || s == "all"
}

// routeReserved is every top-level path segment the panel and API register
// (a test walks the routes, so a new one cannot collide silently), plus
// static, which the file server owns.
var routeReserved = map[string]bool{
	"metrics": true, "admin": true, "login": true, "logout": true, "api": true,
	"hooks": true, "join": true, "invite": true, "cli": true, "account": true,
	"static": true, "dev": true, "settings": true, "setup": true, "-": true,
}

// RouteReserved is a slug that would lose its org home URL (/<slug>) to a
// panel or API route.
func RouteReserved(s string) bool { return routeReserved[s] }

// ValidName reports whether s is a param collection or param name.
func ValidName(s string) bool {
	return nameRe.MatchString(s)
}

// Suggest is the param name closest to s, for a refusal's example:
// lower case, anything else a "_". "my_value" when nothing is left.
func Suggest(s string) string {
	out := strings.Trim(strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, strings.ToLower(s)), "_")
	if out == "" {
		return "my_value"
	}
	return out
}

// ValidEnvKey reports whether s is a POSIX environment variable name.
func ValidEnvKey(s string) bool {
	return envRe.MatchString(s)
}

// OfHost is the org slug a hostname would squat: its first label, past a
// leading "*.". "" for a single label, which names nothing under a root.
// The one label rule for both squat checks (domainres forward, org rename
// reverse).
func OfHost(host string) string {
	label, _, ok := strings.Cut(strings.TrimPrefix(host, "*."), ".")
	if !ok {
		return ""
	}
	return label
}
