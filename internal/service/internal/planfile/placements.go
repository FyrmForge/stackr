package planfile

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// OwnRung is the exits: entry for the env's own node uplink.
const OwnRung = "own"

var placementName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// CheckPlacementName refuses a name that is not a placement name; own is
// valid only where ownOK (an exits: entry).
func CheckPlacementName(key, name string, ownOK bool) error {
	if ownOK && name == OwnRung {
		return nil
	}
	if !placementName.MatchString(name) {
		return fmt.Errorf("%s: %q is not a placement name (lower-case letters, digits and hyphens)", key, name)
	}
	return nil
}

// CheckPlacing checks the three keys of one scope as parsed.
func CheckPlacing(key string, p Placing) error {
	if p.Pin != "" {
		if err := CheckPlacementName(key+".pin", p.Pin, false); err != nil {
			return err
		}
	}
	if p.Ingress != "" {
		if err := CheckPlacementName(key+".ingress", p.Ingress, false); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for _, x := range p.Exits {
		if err := CheckPlacementName(key+".exits", x, true); err != nil {
			return err
		}
		if seen[x] {
			return fmt.Errorf("%s.exits: %s is listed twice", key, x)
		}
		seen[x] = true
	}
	return nil
}

// Placing is one scope's pin, ingress and exits, by placement name. In a file
// an empty Pin or Ingress and a nil Exits say nothing (the file never clears
// a key); an empty non-nil Exits clears the ladder.
type Placing struct {
	Pin     string
	Ingress string
	Exits   []string // placement names or OwnRung, top to bottom
}

// IsZero says the file names none of the three keys.
func (p Placing) IsZero() bool { return p.Pin == "" && p.Ingress == "" && p.Exits == nil }

// PlacementRef is why a placement ref fails for an org: blocker when it is
// not declared, not open to the org, or its node lacks a tag need asks for;
// warning when it has no node yet or its node is offline. Either is "" when
// fine. A placement with no node has no tags to check.
func PlacementRef(ref string, need Needs, f PlacementFacts) (blocker, warning string) {
	if ref == "" {
		return "", ""
	}
	fact, ok := f.Known[ref]
	switch {
	case !ok:
		return fmt.Sprintf("placement %s is not declared", ref), ""
	case !fact.Open:
		return fmt.Sprintf("placement %s is not open to this organization", ref), ""
	case !fact.HasNode:
		return "", fmt.Sprintf("placement %s has no node yet; new envs wait", ref)
	}
	if miss := Fits(fact.Tags, need); len(miss) > 0 {
		blocker = fmt.Sprintf("placement %s does not fit: %s", ref, strings.Join(miss, ", "))
	}
	if !fact.Online {
		warning = fmt.Sprintf("placement %s is offline; new work waits", ref)
	}
	return blocker, warning
}

// PlacingIn is one scope's keys to plan.
type PlacingIn struct {
	Scope string // org | stack | env: Change.Field
	Tile  string // the stack or env slug; "" for the org
	Want  Placing
	Have  Placing
	Need  Needs          // what the envs the pin covers ask of their node
	Facts PlacementFacts // placements as this org sees them
	// Moves is, per env path, the move the pin change causes. Read only when
	// the pin changes.
	Moves map[string]MoveFact
}

// PlacingPlan is the plan for one scope's pin, ingress and exits: a Change
// per key that differs (Kind pin, ingress or exits; Field the scope), and
// the blockers and warnings of every ref the file names. A pin that moves
// envs is an impact line when its checks pass and a blocker when they fail;
// the line's Kind|Tile|Field is stable so an approved move stays approved.
func PlacingPlan(in PlacingIn) (cs []Change, blockers, warnings []string) {
	seenB, seenW := map[string]bool{}, map[string]bool{}
	ref := func(r string, need Needs) {
		b, w := PlacementRef(r, need, in.Facts)
		if b != "" && !seenB[b] {
			seenB[b] = true
			blockers = append(blockers, b)
		}
		if w != "" && !seenW[w] {
			seenW[w] = true
			warnings = append(warnings, w)
		}
	}
	w, h := in.Want, in.Have
	if w.Pin != "" {
		ref(w.Pin, in.Need)
		if w.Pin != h.Pin {
			c := Change{Kind: "pin", Tile: in.Tile, Field: in.Scope, Old: h.Pin, New: w.Pin}
			// a pin the org cannot use moves nothing: its blocker says why
			if b, _ := PlacementRef(w.Pin, in.Need, in.Facts); b == "" {
				fails := moveFails(w.Pin, in.Moves)
				blockers = append(blockers, fails...)
				if len(fails) == 0 {
					c.Impact = moveImpact(w.Pin, in.Moves)
				}
			}
			cs = append(cs, c)
		}
	}
	if w.Ingress != "" {
		ref(w.Ingress, Needs{Ingress: true})
		if w.Ingress != h.Ingress {
			cs = append(cs, Change{Kind: "ingress", Tile: in.Tile, Field: in.Scope, Old: h.Ingress, New: w.Ingress})
		}
	}
	if w.Exits != nil {
		for _, x := range w.Exits {
			if x != OwnRung {
				ref(x, Needs{Egress: true})
			}
		}
		if !slices.Equal(w.Exits, h.Exits) && (len(w.Exits) > 0 || len(h.Exits) > 0) {
			cs = append(cs, Change{
				Kind: "exits", Tile: in.Tile, Field: in.Scope,
				Old: strings.Join(h.Exits, ", "), New: strings.Join(w.Exits, ", "),
			})
		}
	}
	return cs, blockers, warnings
}

// moveFails lists each failed check of each env the pin would move.
func moveFails(pin string, moves map[string]MoveFact) []string {
	var out []string
	for _, path := range slices.Sorted(maps.Keys(moves)) {
		for _, c := range moves[path].Checks {
			if !c.OK {
				s := fmt.Sprintf("pin %s: %s cannot move: %s", pin, path, c.Name)
				if c.Detail != "" {
					s += ": " + c.Detail
				}
				out = append(out, s)
			}
		}
	}
	return out
}

// moveImpact is the impact line of a pin that moves envs; "" when none move.
func moveImpact(pin string, moves map[string]MoveFact) string {
	if len(moves) == 0 {
		return ""
	}
	paths := slices.Sorted(maps.Keys(moves))
	list := strings.Join(paths[:min(3, len(paths))], ", ")
	if len(paths) > 3 {
		list += fmt.Sprintf(", +%d", len(paths)-3)
	}
	noun := "envs"
	if len(paths) == 1 {
		noun = "env"
	}
	down := moves[paths[0]].Downtime
	for _, p := range paths {
		if moves[p].Downtime != down {
			down = ""
		}
	}
	s := fmt.Sprintf("moves %d %s (%s) to %s", len(paths), noun, list, pin)
	if down != "" {
		s += "; each stops for " + down
	}
	return s
}
