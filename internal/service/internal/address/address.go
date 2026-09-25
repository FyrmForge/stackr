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

// Place is the target stack as Resolve reads it: its slug, the target
// tile's env_pairs, and every env of it by slug with the target tile there.
type Place struct {
	Stack string
	Pairs map[string]string
	Envs  map[string]*Host // nil value: the env has no tile of the target's slug
}

// Host is the target tile in one env of the target stack.
type Host struct {
	TileID  string // the caller's handle; Resolve never reads it
	Managed bool
	Ready   bool // its instance row exists (or lands in this very promote)
	Allow   []string
}

// Resolve picks the env of pl a slice at me lands in and checks the allow
// list there, the one rule plan and deploy share. tg is provision_from with
// its refs expanded; base is a PR env's base env slug ("" otherwise): with
// no env pair of its own the PR env maps as its base, and its address stays
// its own. The error is the reason, as a conflict.
func Resolve(orgSlug string, me Address, base string, tg Target, pl Place) (string, error) {
	names := []string{tg.Env}
	if tg.Env == me.Env && base != "" {
		names = append(names, base)
	}
	var env string
	if len(pl.Pairs) > 0 {
		i := slices.IndexFunc(names, func(n string) bool {
			return pl.Pairs[n] != ""
		})
		if i < 0 {
			return "", errs.Conflictf("%s's %s has no env pair for %s", pl.Stack, tg.Tile, tg.Env)
		}
		env = pl.Pairs[names[i]]
		if _, ok := pl.Envs[env]; !ok {
			return "", errs.Conflictf("%s has no environment %s", pl.Stack, env)
		}
	} else {
		// No map: the env name as written must exist there.
		i := slices.IndexFunc(names, func(n string) bool {
			_, ok := pl.Envs[n]
			return ok
		})
		if i < 0 {
			return "", errs.Conflictf("%s has no environment %s", pl.Stack, tg.Env)
		}
		env = names[i]
	}
	at := pl.Stack + "/" + env
	h := pl.Envs[env]
	switch {
	case h == nil:
		return "", errs.Conflictf("%s has no tile %s", at, tg.Tile)
	case !h.Managed:
		return "", errs.Conflictf("%s/%s is not a managed tile", at, tg.Tile)
	case !h.Ready:
		return "", errs.Conflictf("%s/%s has no instance yet; deploy it first", at, tg.Tile)
	}
	self := Address{
		Org:   orgSlug,
		Stack: pl.Stack,
		Env:   env,
		Tile:  tg.Tile,
	}
	ok, err := Allowed(orgSlug, h.Allow, self, me)
	switch {
	case err != nil:
		return "", errs.Conflictf("%s/%s: %v", at, tg.Tile, err)
	case !ok:
		return "", errs.Conflictf("%s/%s does not allow %s", at, tg.Tile, me)
	}
	return env, nil
}
