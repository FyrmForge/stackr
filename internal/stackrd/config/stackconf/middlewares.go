package stackconf

// The stack file's proxy.middlewares: raw traefik middleware bodies, written
// as one dynamic file per stack (proxy.WriteStackMiddlewares). A domain names
// its own stack's by bare name and another stack's in the same org by
// stack/name.

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	yaml "go.yaml.in/yaml/v3"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// ProxyConf is the proxy: block.
type ProxyConf struct {
	Middlewares map[string]RawMap `yaml:"middlewares,omitempty"`
}

var middlewareNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func validateMiddlewares(m map[string]RawMap) error {
	for name, body := range m {
		if !middlewareNameRe.MatchString(name) {
			return fmt.Errorf("proxy.middlewares.%s: name takes letters, digits, - and _", name)
		}
		if len(body) != 1 {
			return fmt.Errorf("proxy.middlewares.%s: want exactly one middleware type, such as forwardAuth", name)
		}
	}
	return nil
}

// RenderMiddlewares is the stack row's form of proxy.middlewares, "" for none.
// yaml sorts map keys, so the same file always renders the same text.
func RenderMiddlewares(m map[string]RawMap) string {
	if len(m) == 0 {
		return ""
	}
	b, _ := yaml.Marshal(m)
	return string(b)
}

// ParseMiddlewares reads the stack row's form back.
func ParseMiddlewares(s string) map[string]RawMap {
	var out map[string]RawMap
	_ = yaml.Unmarshal([]byte(s), &out)
	return out
}

func renderBody(b RawMap) string {
	out, _ := yaml.Marshal(b)
	return strings.TrimSpace(string(out))
}

// middlewareSnapshot fills the State fields proxy.middlewares plans against:
// every other stack's names in the org, and the domain rows in other stacks
// that name one of this stack's.
func (pl Planner) middlewareSnapshot(ctx context.Context, stack *repo.Stack, s *State) error {
	s.StackSlug = stack.Slug
	s.Middlewares = stack.ProxyMiddlewares
	s.OrgMiddlewares = map[string]map[string]bool{}
	s.MiddlewareUsers = map[string][]string{}
	stacks, err := pl.Store.ListStacksByOrg(ctx, stack.OrgID)
	if err != nil {
		return err
	}
	for _, other := range stacks {
		if other.ID == stack.ID {
			continue
		}
		names := map[string]bool{}
		for n := range ParseMiddlewares(other.ProxyMiddlewares) {
			names[n] = true
		}
		s.OrgMiddlewares[other.Slug] = names
		tiles, err := pl.Store.ListTilesByStack(ctx, other.ID)
		if err != nil {
			return err
		}
		for _, t := range tiles {
			ds, err := pl.Store.ListDomainsByTile(ctx, t.ID)
			if err != nil {
				return err
			}
			for _, d := range ds {
				for _, ref := range d.MiddlewareList() {
					if st, name, ok := strings.Cut(ref, "/"); ok && st == stack.Slug {
						s.MiddlewareUsers[name] = append(s.MiddlewareUsers[name], other.Slug+"/"+t.Slug)
					}
				}
			}
		}
	}
	return nil
}

// diffMiddlewares plans the stack's proxy.middlewares. Stack-level, like the
// domain resources: an env-scoped plan leaves it alone.
func (p *Plan) diffMiddlewares(r *Resolved, s State) {
	have := ParseMiddlewares(s.Middlewares)
	for _, name := range sortedRawKeys(r.Middlewares) {
		want := renderBody(r.Middlewares[name])
		cur, ok := have[name]
		switch {
		case !ok:
			p.Changes = append(p.Changes, Change{Kind: "create", Env: "stack", Field: "middleware", New: name})
		case renderBody(cur) != want:
			p.Changes = append(p.Changes, Change{Kind: "update", Env: "stack", Field: "middleware", Old: name + ": " + renderBody(cur), New: name + ": " + want})
		}
	}
	// traefik disables a router naming a middleware that does not exist, and
	// says so only in its own log, so a name still in use cannot go.
	local := fileMiddlewareRefs(r, s.StackSlug)
	for _, name := range sortedRawKeys(have) {
		if _, ok := r.Middlewares[name]; ok {
			continue
		}
		users := append(append([]string(nil), s.MiddlewareUsers[name]...), local[name]...)
		if len(users) > 0 {
			sort.Strings(users)
			p.Errors = append(p.Errors, fmt.Sprintf("middleware %s is still used by %s; drop those references first", name, strings.Join(users, ", ")))
			continue
		}
		p.Changes = append(p.Changes, Change{Kind: "delete", Env: "stack", Field: "middleware", Old: name})
	}
}

// fileMiddlewareRefs maps this stack's middleware names to the tiles in the
// file that use them.
func fileMiddlewareRefs(r *Resolved, ownSlug string) map[string][]string {
	out := map[string][]string{}
	for env, re := range r.Envs {
		for slug, tc := range re.Tiles {
			for _, d := range tc.Domains {
				for _, ref := range d.Middlewares {
					st, name, cross := strings.Cut(ref, "/")
					if !cross {
						name = st
					} else if st != ownSlug {
						continue
					}
					out[name] = append(out[name], env+"/"+slug)
				}
			}
		}
	}
	return out
}

// checkMiddlewareRefs refuses a domain naming a middleware that does not
// exist: this file's for a bare name, the named stack's otherwise.
func (p *Plan) checkMiddlewareRefs(r *Resolved, s State, opts DiffOpts) {
	for _, env := range sortedEnvKeys(r.Envs) {
		if opts.OnlyEnv != "" && env != opts.OnlyEnv {
			continue
		}
		for _, slug := range sortedTileKeys(r.Envs[env].Tiles) {
			for _, d := range r.Envs[env].Tiles[slug].Domains {
				for _, ref := range d.Middlewares {
					st, name, cross := strings.Cut(ref, "/")
					ok := false
					switch {
					case !cross:
						_, ok = r.Middlewares[st]
					case st == s.StackSlug:
						_, ok = r.Middlewares[name]
					default:
						ok = s.OrgMiddlewares[st][name]
					}
					if !ok {
						p.Errors = append(p.Errors, fmt.Sprintf("env %s: tile %s: no middleware %s", env, slug, ref))
					}
				}
			}
		}
	}
}

func isMiddleware(c Change) bool { return c.Env == "stack" && c.Tile == "" && c.Field == "middleware" }

// applyMiddlewares lands proxy.middlewares on the stack row and traefik. It
// writes the union of old and new first, so a router the walk is about to
// repoint never loses its middleware, and returns the call that writes the
// final set once the walk is done.
func (a Applier) applyMiddlewares(ctx context.Context, stack *repo.Stack, r *Resolved, p *Plan) (func(), error) {
	var kept []Change
	changed := false
	for _, c := range p.Changes {
		if isMiddleware(c) {
			changed = true
		} else {
			kept = append(kept, c)
		}
	}
	p.Changes = kept
	if !changed {
		return func() {}, nil
	}
	union := ParseMiddlewares(stack.ProxyMiddlewares)
	if union == nil {
		union = map[string]RawMap{}
	}
	for n, b := range r.Middlewares {
		union[n] = b
	}
	write := func(m map[string]RawMap) error {
		stack.ProxyMiddlewares = RenderMiddlewares(m)
		if err := a.Planner.Store.UpdateStack(ctx, stack); err != nil {
			return err
		}
		if a.Ops.PX != nil {
			return a.Ops.PX.WriteStackMiddlewares(stack)
		}
		return nil
	}
	if err := write(union); err != nil {
		return nil, err
	}
	return func() {
		if err := write(r.Middlewares); err != nil {
			warn("write stack middlewares", &repo.Tile{Slug: stack.Slug}, err)
		}
	}, nil
}

func sortedRawKeys(m map[string]RawMap) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedEnvKeys(m map[string]ResolvedEnv) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedTileKeys(m map[string]TileConf) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
