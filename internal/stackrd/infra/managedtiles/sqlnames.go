package managedtiles

import (
	"fmt"
	"strings"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Naming rules every SQL engine shares: an identifier a CREATE DATABASE will
// accept, and a name no other slice on this instance has taken.

// sqlIdent turns a tile slug into a safe SQL identifier, lowercase, digits
// and underscores, never leading with a digit.
func sqlIdent(slug string) string {
	id := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			return r
		case r == '-':
			return '_'
		}
		return -1
	}, strings.ToLower(slug))
	if id == "" || (id[0] >= '0' && id[0] <= '9') {
		id = "db_" + id
	}
	return id
}

// uniqueSliceName suffixes base until no existing slice answers to it. sep is
// the engine's own joiner: "_" where the name is an SQL identifier, "-" where
// it has to be DNS-safe.
func uniqueSliceName(base string, existing []repo.Provision, sep string) string {
	taken := map[string]bool{}
	for i := range existing {
		taken[existing[i].DBName] = true
	}
	name := base
	for i := 2; taken[name]; i++ {
		name = fmt.Sprintf("%s%s%d", base, sep, i)
	}
	return name
}
