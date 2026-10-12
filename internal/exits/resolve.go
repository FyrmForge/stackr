// Package exits is the pure and argv-plan side of exit nodes: which ladder a
// tile uses, the commands a node and an exit run for one exit, and the
// ladder state machine that picks the live rung. Nothing here touches the
// system; commands go through an injected Runner and health through an
// injected Pinger.
package exits

// Ladder levels in order of precedence, for Resolution.Source.
const (
	SourceTile    = "tile"
	SourceEnv     = "env"
	SourceStack   = "stack"
	SourceOrg     = "org"
	SourceDefault = "default"
	SourceNone    = "none"
)

// Levels is the stored ladder (node ids in order) at each level, nil or empty
// when that level has none.
type Levels struct {
	Tile, Env, Stack, Org []string
}

// Resolution is the ladder one tile uses.
type Resolution struct {
	// Source is the level the ladder came from; SourceNone when nothing is set.
	Source string
	// Nodes is the ordered exit node ids, first duplicate wins. Empty means no
	// exit routing: the tile uses its own node's uplink.
	Nodes []string
}

// Resolve picks the first non-empty ladder of tile, env, stack, org, then def
// (the controller default). The tile's own node is an exit only when the
// ladder lists it; it is never appended as a last resort, so a ladder of
// exits that all fail means no internet, not a leak through the own uplink.
func Resolve(l Levels, def []string) Resolution {
	for _, c := range []struct {
		src   string
		nodes []string
	}{{SourceTile, l.Tile}, {SourceEnv, l.Env}, {SourceStack, l.Stack}, {SourceOrg, l.Org}, {SourceDefault, def}} {
		if n := dedupe(c.nodes); len(n) > 0 {
			return Resolution{Source: c.src, Nodes: n}
		}
	}
	return Resolution{Source: SourceNone}
}

// Routed reports whether the tile on node self needs exit machinery: some
// rung is not the own uplink.
func (r Resolution) Routed(self string) bool {
	for _, n := range r.Nodes {
		if n != self {
			return true
		}
	}
	return false
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range in {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}
