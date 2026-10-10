package planfile

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/FyrmForge/stackr/internal/service/internal/slug"
)

// PR is the env key of the PR envs' block, in a stack file and an org file.
const PR = "pr"

// Entry is one name in an env block: a plain value, or a secret by name only
// (the file is in git). Written `host: x` or `pw: {type: secret, generate: 32}`.
type Entry struct {
	Secret bool
	Value  string
	// Generate is a secret's length: stackr makes the value once, the first
	// time the env has none, and never changes it.
	Generate int
}

func (e *Entry) UnmarshalYAML(n *yaml.Node) error {
	switch {
	case n.Kind == yaml.ScalarNode:
		*e = Entry{Value: n.Value}
		return nil
	case n.Kind != yaml.MappingNode:
		return fmt.Errorf("line %d: a param is a value, or {type: secret}", n.Line)
	}
	*e = Entry{}
	var typ string
	for i := 0; i < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		var err error
		switch k.Value {
		case "type":
			err = v.Decode(&typ)
		case "generate":
			err = v.Decode(&e.Generate)
		default:
			err = fmt.Errorf("line %d: unknown key %s (a secret is {type: secret, generate: N})", k.Line, k.Value)
		}
		if err != nil {
			return err
		}
	}
	if typ != "secret" {
		return fmt.Errorf("line %d: type must be secret; a plain param is just its value", n.Line)
	}
	e.Secret = true
	return nil
}

func (e Entry) MarshalYAML() (any, error) {
	switch {
	case !e.Secret:
		return e.Value, nil
	case e.Generate > 0:
		return struct {
			Type     string `yaml:"type"`
			Generate int    `yaml:"generate"`
		}{"secret", e.Generate}, nil
	}
	return struct {
		Type string `yaml:"type"`
	}{"secret"}, nil
}

// Group is one params: collection as written: env key -> name -> entry. An
// env key is one env slug or several joined by "|" (dev|staging|pr).
type Group map[string]map[string]Entry

// Expand is the groups as one block per env: env -> "group.name" -> entry.
// Two keys setting one name for one env are an error naming both. Env keys
// are checked as slugs only; the file checks them against its own envs.
func Expand(ps map[string]Group) (map[string]map[string]Entry, error) {
	out := map[string]map[string]Entry{}
	from := map[string]string{} // env + "\x00" + full name -> the key that set it
	for _, c := range slices.Sorted(maps.Keys(ps)) {
		for _, key := range slices.Sorted(maps.Keys(ps[c])) {
			envs := strings.Split(key, "|")
			for i, env := range envs {
				if !slug.Valid(env) {
					return nil, fmt.Errorf("params: %s: %q is not an env slug (join several with |)", c, key)
				}
				if slices.Contains(envs[:i], env) {
					return nil, fmt.Errorf("params: %s: %q names %s twice", c, key, env)
				}
			}
			for n, e := range ps[c][key] {
				full := c + "." + n
				for _, env := range envs {
					if prev, ok := from[env+"\x00"+full]; ok {
						return nil, fmt.Errorf("params: %s is set for %s by both %q and %q", full, env, prev, key)
					}
					from[env+"\x00"+full] = key
					if out[env] == nil {
						out[env] = map[string]Entry{}
					}
					out[env][full] = e
				}
			}
		}
	}
	return out, nil
}

// CheckParams is the per-env params: grammar the stack file and a tiered org
// file share. generate: is acted on only where allowGenerate.
func CheckParams(ps map[string]Group, allowGenerate bool) error {
	for c, g := range ps {
		if !slug.ValidName(c) {
			return fmt.Errorf("params: collection %q is lower-case letters, digits and _", c)
		}
		for _, entries := range g {
			for n, e := range entries {
				switch {
				case !slug.ValidName(n):
					return fmt.Errorf("params: %s.%s: a name is lower-case letters, digits and _", c, n)
				case e.Generate != 0 && !allowGenerate:
					return fmt.Errorf("params: %s.%s: generate is only read in a stack file", c, n)
				case e.Generate != 0 && (e.Generate < 16 || e.Generate > 128):
					return fmt.Errorf("params: %s.%s: generate is a length from 16 to 128", c, n)
				}
			}
		}
	}
	_, err := Expand(ps)
	return err
}

// CheckEnvKeys refuses a block for an env that is not one of allowed or pr.
func CheckEnvKeys(blocks map[string]map[string]Entry, allowed []string, what string) error {
	for _, env := range slices.Sorted(maps.Keys(blocks)) {
		if env != PR && !slices.Contains(allowed, env) {
			return fmt.Errorf("params: %q is not %s (or pr)", env, what)
		}
	}
	return nil
}

// Collapse is Expand's inverse for export: per group, envs holding the same
// names and entries share one "a|b" key. order is the key's env order; envs
// it does not name come after, sorted. A secret is name and type only, so
// envs with the same secret names still share a key (each keeps its value).
func Collapse(blocks map[string]map[string]Entry, order []string) map[string]Group {
	envs := slices.Clone(order)
	for _, e := range slices.Sorted(maps.Keys(blocks)) {
		if !slices.Contains(envs, e) {
			envs = append(envs, e)
		}
	}
	type bucket struct {
		envs    []string
		entries map[string]Entry
	}
	out := map[string]Group{}
	sigs := map[string]map[string]*bucket{} // group -> signature -> bucket
	for _, env := range envs {
		perGroup := map[string]map[string]Entry{}
		for full, e := range blocks[env] {
			c, n, _ := strings.Cut(full, ".")
			if perGroup[c] == nil {
				perGroup[c] = map[string]Entry{}
			}
			perGroup[c][n] = e
		}
		for c, entries := range perGroup {
			var sig strings.Builder
			for _, n := range slices.Sorted(maps.Keys(entries)) {
				e := entries[n]
				fmt.Fprintf(&sig, "%q %v %d %q\n", n, e.Secret, e.Generate, e.Value)
			}
			if sigs[c] == nil {
				sigs[c] = map[string]*bucket{}
			}
			b := sigs[c][sig.String()]
			if b == nil {
				b = &bucket{entries: entries}
				sigs[c][sig.String()] = b
			}
			b.envs = append(b.envs, env)
		}
	}
	for c, bs := range sigs {
		out[c] = Group{}
		for _, b := range bs {
			out[c][strings.Join(b.envs, "|")] = b.entries
		}
	}
	return out
}
