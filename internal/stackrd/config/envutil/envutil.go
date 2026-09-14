// Package envutil parses KEY=VALUE env blobs shared by the deploy engine,
// databases, and the web Variables tab. Values may span multiple lines when
// wrapped in single or double quotes.
package envutil

import (
	"fmt"
	"strings"
)

// Var is one parsed KEY=VALUE pair, in source order.
type Var struct {
	Key   string
	Value string
}

// Parse splits a KEY=VALUE-per-line blob into pairs, skipping blanks and
// comments. A value opening with ' or " may span lines until the closing
// quote; surrounding quotes are stripped.
// no escape sequences (\n, \") inside quotes, literal text only.
func Parse(s string) []Var {
	var out []Var
	lines := strings.Split(s, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) > 0 && (v[0] == '"' || v[0] == '\'') {
			q := v[0]
			rest := v[1:]
			if end := strings.IndexByte(rest, q); end >= 0 {
				v = rest[:end] // closed on the same line
			} else {
				parts := []string{rest}
				for i++; i < len(lines); i++ {
					if end := strings.IndexByte(lines[i], q); end >= 0 {
						parts = append(parts, lines[i][:end])
						break
					}
					parts = append(parts, lines[i])
				}
				v = strings.Join(parts, "\n")
			}
		}
		out = append(out, Var{Key: strings.TrimSpace(k), Value: v})
	}
	return out
}

// Inject sets key=val in a KEY=VALUE blob and returns the new blob plus the var
// name actually used. If key already holds val it's a no-op (idempotent). If key
// holds a DIFFERENT value it is NOT clobbered, the var is added under a free
// suffix (key_2, key_3…) so an existing value for another consumer survives.
func Inject(env, key, val string) (string, string) {
	keys := map[string]string{} // name -> value, for collision checks
	for _, ln := range strings.Split(env, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(ln), "="); ok {
			keys[k] = v
		}
	}
	if keys[key] == val {
		return env, key // already set to this exact value
	}
	name := key
	for n := 2; ; n++ {
		cur, exists := keys[name]
		if !exists || cur == val {
			break
		}
		name = fmt.Sprintf("%s_%d", key, n)
	}
	return appendOrReplaceLine(env, name, val), name
}

// appendOrReplaceLine replaces the line for key (if any) with key=val, else
// appends it.
func appendOrReplaceLine(env, key, val string) string {
	var kept []string
	for _, ln := range strings.Split(env, "\n") {
		if k := strings.SplitN(strings.TrimSpace(ln), "=", 2)[0]; k != key {
			kept = append(kept, ln)
		}
	}
	out := strings.TrimRight(strings.Join(kept, "\n"), "\n")
	if out != "" {
		out += "\n"
	}
	return out + key + "=" + val
}

// Lines returns the pairs as "KEY=VALUE" strings for container specs.
func Lines(s string) []string {
	vars := Parse(s)
	out := make([]string, len(vars))
	for i, v := range vars {
		out[i] = v.Key + "=" + v.Value
	}
	return out
}
