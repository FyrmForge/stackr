// Package envcompare answers "how do this stack's environments differ from
// the first one": one row per tile slug, one cell per environment, the keys
// that differ, and which of those someone has marked as on purpose. It reads
// the same serialized form the config planner does, so the keys are the
// config file's keys.
package envcompare

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/FyrmForge/stackr/internal/stackrd/config/stackconf"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Key is one differing key on one tile in one env.
type Key struct {
	Name     string
	Ref      string // the reference env's value ("" = not set there)
	Val      string // this env's value
	Intended bool
}

// Cell is one tile in one env, compared to the reference.
type Cell struct {
	EnvID string
	State string // ref | same | differs | missing | intended
	Keys  []Key  // set for differs and intended
}

// Row is one tile slug across the environments.
type Row struct {
	Slug  string
	Cells []Cell // one per env, in Result.Envs order; Cells[0] is the reference
}

// Result is the whole comparison.
type Result struct {
	Envs    []repo.Environment // static envs in ladder order; [0] is the reference
	Rows    []Row
	Differs int // cells that differ and are not intended, across every env
	Missing int // cells whose tile the env does not have
}

// Compare builds the result. state is the planner's Snapshot; intended is
// the intended rows per env id.
func Compare(stackName string, envs []repo.Environment, state stackconf.State, intended map[string][]repo.Intended) Result {
	res := Result{Envs: envs}
	if len(envs) == 0 {
		return res
	}
	// Stack-scoped instances live in exactly one env by design: not a
	// difference. Drop them before serializing.
	for slug, es := range state.Envs {
		kept := map[string]stackconf.TileState{}
		for name, ts := range es.Tiles {
			if ts.Tile.ScopeKind != "stack" {
				kept[name] = ts
			}
		}
		es.Tiles = kept
		state.Envs[slug] = es
	}
	r := stackconf.StateToResolved(stackName, state)
	ref := envs[0]
	refTiles := r.Envs[ref.Slug].Tiles
	refFlat := map[string]map[string]string{}
	slugs := map[string]bool{}
	for slug, tc := range refTiles {
		refFlat[slug] = flatten(tc)
		slugs[slug] = true
	}
	envFlat := map[string]map[string]map[string]string{} // env id → slug → keys
	for _, e := range envs[1:] {
		envFlat[e.ID] = map[string]map[string]string{}
		for slug, tc := range r.Envs[e.Slug].Tiles {
			envFlat[e.ID][slug] = flatten(tc)
			slugs[slug] = true
		}
	}
	marks := map[string]map[string]string{} // env id → slug\x00key → value
	for id, rows := range intended {
		marks[id] = map[string]string{}
		for _, it := range rows {
			marks[id][it.TileSlug+"\x00"+it.Key] = it.Value
		}
	}
	// declared by the file on the reference env: on purpose everywhere
	isIntended := func(envID, slug, key, val string) bool {
		if v, ok := marks[envID][slug+"\x00"+key]; ok && (v == "" || v == val) {
			return true
		}
		if v, ok := marks[ref.ID][slug+"\x00"+key]; ok && v == "" {
			return true
		}
		return false
	}

	for _, slug := range sortedKeys(slugs) {
		row := Row{Slug: slug}
		refState := "ref"
		if _, ok := refFlat[slug]; !ok {
			refState = "missing"
		}
		row.Cells = append(row.Cells, Cell{EnvID: ref.ID, State: refState})
		for _, e := range envs[1:] {
			cell := Cell{EnvID: e.ID}
			mine, ok := envFlat[e.ID][slug]
			if !ok {
				cell.State = "missing"
				res.Missing++
				row.Cells = append(row.Cells, cell)
				continue
			}
			theirs := refFlat[slug]
			if theirs == nil {
				// only this env has the tile: a difference in itself
				theirs = map[string]string{}
			}
			var diff bool
			for _, k := range sortedKeys(union(mine, theirs)) {
				if mine[k] == theirs[k] {
					continue
				}
				key := Key{Name: k, Ref: theirs[k], Val: mine[k], Intended: isIntended(e.ID, slug, k, mine[k])}
				if !key.Intended {
					diff = true
				}
				cell.Keys = append(cell.Keys, key)
			}
			switch {
			case len(cell.Keys) == 0:
				cell.State = "same"
			case diff:
				cell.State = "differs"
				res.Differs++
			default:
				cell.State = "intended"
			}
			row.Cells = append(row.Cells, cell)
		}
		res.Rows = append(res.Rows, row)
	}
	return res
}

// Fields is the top-level config fields behind a cell's keys, the form the
// applier's update path takes ("env.PORT" → "env").
func (c Cell) Fields() map[string]bool {
	out := map[string]bool{}
	for _, k := range c.Keys {
		f, _, _ := strings.Cut(k.Name, ".")
		out[f] = true
	}
	return out
}

// Value is the current value of one key in the cell, "" when it is not
// among the differing keys.
func (c Cell) Value(key string) string {
	for _, k := range c.Keys {
		if k.Name == key {
			return k.Val
		}
	}
	return ""
}

// flatten turns a tile's config into dotted keys. Domains are left out:
// hostnames differ per environment by construction. A value that references
// a secret compares by presence, never by what it resolves to.
func flatten(tc stackconf.TileConf) map[string]string {
	b, _ := json.Marshal(tc)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	delete(m, "domains")
	// a slice's database name carries the env in it (cart, cart_stg_3)
	if _, slice := m["from"]; slice {
		delete(m, "name")
	}
	out := map[string]string{}
	walk("", m, out)
	return out
}

func walk(prefix string, v any, out map[string]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, sub := range x {
			name := k
			if prefix != "" {
				name = prefix + "." + k
			}
			walk(name, sub, out)
		}
	case []any:
		parts := make([]string, 0, len(x))
		for _, e := range x {
			parts = append(parts, scalar(e))
		}
		out[prefix] = strings.Join(parts, ", ")
	default:
		out[prefix] = scalar(x)
	}
}

func scalar(v any) string {
	s := fmt.Sprint(v)
	if m, ok := v.(map[string]any); ok {
		b, _ := json.Marshal(m)
		s = string(b)
	}
	if strings.Contains(s, "${{") {
		return "set"
	}
	return s
}

func union(a, b map[string]string) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
