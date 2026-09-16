package repo

import "strings"

// Slugify turns a display name into a URL/DNS-safe slug: lowercase, runs of
// anything non-alphanumeric collapse to a single '-', trimmed at the ends.
// UI renames touch only the display name; config-driven org/stack renames
// re-derive the slug from the file's declared name.
func Slugify(name string) string {
	var b strings.Builder
	dash := true // suppress leading dash
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		default:
			if !dash {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

// ReservedSlug reports whether a slug collides with a reference bucket. Those
// names sit where a source slug goes in ${{ scope.<slug>.<output> }}, so
// a tile or shared instance called one of them would make
// ${{ stack.vars.NAME }} ambiguous. See internal/stackrd/config/varref.
func ReservedSlug(s string) bool {
	return s == "vars" || s == "secrets" || s == "backups" || s == "storage"
}
