package domain

import (
	"slices"
	"strings"
)

// Wildcard is a wildcard certificate subject and the managed domain whose
// DNS answers its challenge.
type Wildcard struct {
	Subject string // *.stack.org.example.com
	Domain  string // example.com
}

// WildcardSubjects is the wildcards the hosts in managed domains need, one
// per level in use: a * covers one label, so each host is covered by
// *.<its parent> when that parent is the managed domain or under it.
// Auto names (tile.stack.org.<root>) share *.stack.org.<root>; typed
// one-level names share *.<domain>. An apex, a host in no managed domain
// and a wildcard host itself get none (the apex falls back to its own
// certificate).
//
// D3's rule. D2's caddy.go stub of the same name returns *.<domain>; on
// merge this body replaces it.
func WildcardSubjects(managed, hosts []string) []Wildcard {
	var out []Wildcard
	for _, h := range hosts {
		h = strings.ToLower(strings.TrimSuffix(h, "."))
		if strings.HasPrefix(h, "*.") {
			continue
		}
		_, parent, ok := strings.Cut(h, ".")
		if !ok {
			continue
		}
		best := ""
		for _, m := range managed {
			m = strings.ToLower(strings.TrimSuffix(m, "."))
			if (parent == m || strings.HasSuffix(parent, "."+m)) && len(m) > len(best) {
				best = m
			}
		}
		if best == "" {
			continue
		}
		w := Wildcard{Subject: "*." + parent, Domain: best}
		if !slices.Contains(out, w) {
			out = append(out, w)
		}
	}
	slices.SortFunc(out, func(a, b Wildcard) int { return strings.Compare(a.Subject, b.Subject) })
	return out
}
