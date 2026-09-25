// Package address is a tile's name across its org and the allow list a
// managed tile keeps (DECIDE 194). Pure: no store, no docker, no I/O.
// Leaves and flows both use it, so it lives outside leaf/ and flow/, as slug does.
//
//	address         org:stack:env:tile, four slugs
//	allow entry     one to four segments; the first is this org, never * or another org
//	* in the middle matches exactly one segment
//	trailing *      as the last written segment swallows the rest (testorg:*)
//	provision_from  stack:env:tile in the consumer's own org; no allow list = own env only
package address

import (
	"slices"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/slug"
)

const star = "*"

// Address is one tile anywhere in an org, by slugs.
type Address struct {
	Org, Stack, Env, Tile string
}

func (a Address) String() string {
	return strings.Join(a.segs(), ":")
}

func (a Address) segs() []string {
	return []string{
		a.Org,
		a.Stack,
		a.Env,
		a.Tile,
	}
}

// Pattern is one parsed allow entry. Only ParseAllow builds one, so every
// Pattern has passed the own-org rule. A trailing * is expanded at parse,
// so each of the four segments is a slug or a one-segment *.
type Pattern struct {
	segs [4]string
}

// ParseAllow parses a managed tile's allow list. The first bad entry fails
// the whole list: a half-applied list would widen or narrow access silently.
func ParseAllow(orgSlug string, list []string) ([]Pattern, error) {
	out := make([]Pattern, 0, len(list))
	for _, entry := range list {
		p, err := parsePattern(orgSlug, entry)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func parsePattern(orgSlug, entry string) (Pattern, error) {
	segs := strings.Split(entry, ":")
	switch {
	case slices.Contains(segs, ""):
		return Pattern{}, errs.Invalidf("allow", "%s: a segment is empty", entry)
	case segs[0] != orgSlug:
		return Pattern{}, errs.Invalidf("allow", "%s: the first segment must be this org, %s", entry, orgSlug)
	case len(segs) > 4:
		return Pattern{}, errs.Invalidf("allow", "%s: more than four segments", entry)
	}
	for _, s := range segs {
		if s != star && !slug.Valid(s) {
			return Pattern{}, errs.Invalidf("allow", "%s: %q is not a slug (lower-case letters, digits and single hyphens) or *", entry, s)
		}
	}
	if len(segs) < 4 && segs[len(segs)-1] != star {
		return Pattern{}, errs.Invalidf("allow", "%s: four segments or a trailing *", entry)
	}
	var p Pattern
	for i := range p.segs {
		p.segs[i] = star
	}
	copy(p.segs[:], segs)
	return p, nil
}

// Match reports whether any pattern admits a.
func Match(patterns []Pattern, a Address) bool {
	return slices.ContainsFunc(patterns, func(p Pattern) bool {
		return p.match(a)
	})
}

func (p Pattern) match(a Address) bool {
	for i, got := range a.segs() {
		if p.segs[i] != star && p.segs[i] != got {
			return false
		}
	}
	return true
}

// Allowed decides whether the tile at a may connect to the managed tile at
// self. No list (nil or empty) is the tile's own env, any tile in it, as env
// scope was. A list replaces that default, it does not add to it.
func Allowed(orgSlug string, list []string, self, a Address) (bool, error) {
	if len(list) == 0 {
		return a.Org == self.Org && a.Stack == self.Stack && a.Env == self.Env, nil
	}
	patterns, err := ParseAllow(orgSlug, list)
	if err != nil {
		return false, err
	}
	return Match(patterns, a), nil
}

// Target is a slice tile's provision_from: a managed tile in the consumer's
// own org.
type Target struct {
	Stack, Env, Tile string
}

// ParseTarget parses provision_from, <stack>:<env>:<tile>. A segment holding
// a ${{ ... }} ref is kept verbatim so the stack file can carry it; refs never
// contain ":", so splitting first is safe.
func ParseTarget(s string) (Target, error) {
	segs := strings.Split(s, ":")
	if len(segs) != 3 {
		return Target{}, errs.Invalidf("provision_from", "%s: three segments, <stack>:<env>:<tile>", s)
	}
	for _, seg := range segs {
		switch {
		case seg == "":
			return Target{}, errs.Invalidf("provision_from", "%s: a segment is empty", s)
		case strings.Contains(seg, "${{"):
			// ponytail: any segment with a ref passes; the flow resolves refs
			// and parses again, where the result must be a slug.
		case !slug.Valid(seg):
			return Target{}, errs.Invalidf("provision_from", "%s: %q is not a slug (lower-case letters, digits and single hyphens)", s, seg)
		}
	}
	return Target{
		Stack: segs[0],
		Env:   segs[1],
		Tile:  segs[2],
	}, nil
}
