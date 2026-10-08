package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

// dotenvEntry is one KEY=VALUE of a .env file, the key already lowercased.
type dotenvEntry struct{ name, value string }

// paramNameRe is the param name rule (leaf/params): lower-case letters, digits and _.
var paramNameRe = regexp.MustCompile(`^[a-z0-9_]+$`)

// parseDotenv reads KEY=VALUE lines: # comments, an export prefix, single
// and double quotes, no interpolation, no multi-line values. A later
// duplicate replaces the earlier value. refused names the keys that cannot
// become a param name (or "line N" when there is no key); never a value.
func parseDotenv(src string) (entries []dotenvEntry, refused []string) {
	at := map[string]int{}
	for i, l := range strings.Split(src, "\n") {
		l = strings.TrimSpace(strings.TrimSuffix(l, "\r"))
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		l = strings.TrimSpace(strings.TrimPrefix(l, "export "))
		k, v, ok := strings.Cut(l, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			refused = append(refused, fmt.Sprintf("line %d", i+1))
			continue
		}
		name := strings.ToLower(k)
		val, ok := dotenvValue(strings.TrimSpace(v))
		if !paramNameRe.MatchString(name) || !ok {
			refused = append(refused, k)
			continue
		}
		if j, dup := at[name]; dup {
			entries[j].value = val
			continue
		}
		at[name] = len(entries)
		entries = append(entries, dotenvEntry{name, val})
	}
	return entries, refused
}

// dotenvValue unquotes one value; false for an unterminated quote.
func dotenvValue(v string) (string, bool) {
	if v == "" {
		return "", true
	}
	switch q := v[0]; q {
	case '\'':
		s, _, ok := strings.Cut(v[1:], "'")
		return s, ok
	case '"':
		var sb strings.Builder
		for i := 1; i < len(v); i++ {
			switch c := v[i]; {
			case c == '"':
				return sb.String(), true
			case c == '\\' && i+1 < len(v):
				i++
				switch v[i] {
				case 'n':
					sb.WriteByte('\n')
				case 't':
					sb.WriteByte('\t')
				case 'r':
					sb.WriteByte('\r')
				case '"', '\\':
					sb.WriteByte(v[i])
				default:
					sb.WriteByte('\\')
					sb.WriteByte(v[i])
				}
			default:
				sb.WriteByte(c)
			}
		}
		return "", false
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v), true
}

func (a *app) paramsImport() *cobra.Command {
	var scope, collection string
	var secret, dry bool
	c := leaf("import <file>", "org.params-set,stack.params-set,env.params-set,admin.params-set",
		"Set a param per KEY=VALUE line of a .env file, in one collection; others at the level stay", exact(1),
		func(c *cobra.Command, args []string) error {
			if collection == "" || !paramNameRe.MatchString(collection) {
				return usage("--collection is required: lower-case letters, digits and _")
			}
			if scope != "" {
				if err := c.Flags().Set("level", scope); err != nil {
					return err
				}
			}
			p, err := a.levelPath(c)
			if err != nil {
				return err
			}
			b, err := a.readSecretFile(args[0])
			if err != nil {
				return err
			}
			es, refused := parseDotenv(string(b))
			if len(refused) > 0 {
				return fmt.Errorf("cannot map to a param name (lower-case letters, digits and _): %s", strings.Join(refused, ", "))
			}
			if len(es) == 0 {
				return usage("%s has no KEY=VALUE lines", args[0])
			}
			kind := "param"
			if secret {
				kind = "secret"
			}
			body := make([]map[string]string, 0, len(es))
			for _, e := range es {
				if dry {
					_, _ = fmt.Fprintf(a.out, "%s.%s\t%s\n", collection, e.name, kind)
				}
				body = append(body, map[string]string{
					"collection": collection, "name": e.name, "kind": kind, "value": e.value,
				})
			}
			if dry {
				return nil
			}
			v, err := a.call(PATCH, p+"/params", body)
			a.redeploying(v)
			return err
		})
	c.Example = "  stackr params import .env --scope env --stack shop --env dev --collection app --secret"
	c.Flags().StringVar(&scope, "scope", "", "org, stack or env (same as --level)")
	c.Flags().StringVar(&collection, "collection", "", "the collection every name lands in")
	c.Flags().BoolVar(&secret, "secret", false, "store the values as secrets")
	c.Flags().BoolVar(&dry, "dry-run", false, "print the names that would be set, never the values")
	return c
}

// readSecretFile reads a file of values. A world-readable file is read
// with a warning; an unreadable one is an error.
func (a *app) readSecretFile(name string) ([]byte, error) {
	fi, err := os.Stat(name)
	if err != nil {
		return nil, fmt.Errorf("%s is not readable", name)
	}
	if fi.Mode().Perm()&0o004 != 0 {
		_, _ = fmt.Fprintf(a.errw, "warning: %s is world-readable (chmod 600 it)\n", name)
	}
	b, err := os.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("%s is not readable", name)
	}
	return b, nil
}
