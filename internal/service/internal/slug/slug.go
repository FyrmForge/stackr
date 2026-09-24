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
func Valid(s string) bool { return slugRe.MatchString(s) }

// Reserved is a slug a ref would read as a keyword: `params` is the only one
// ("Param store and refs").
func Reserved(s string) bool { return s == "params" }

// ValidName reports whether s is a param collection or param name.
func ValidName(s string) bool { return nameRe.MatchString(s) }

// ValidEnvKey reports whether s is a POSIX environment variable name.
func ValidEnvKey(s string) bool { return envRe.MatchString(s) }
