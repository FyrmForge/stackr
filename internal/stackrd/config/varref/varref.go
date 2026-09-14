// Package varref resolves ${{ ... }} references in tile variables. It is the
// single resolver behind deploys, compose runs, database tiles, cron and
// config-plan validation, API previews and CLI export, a
// second implementation would be a second set of rules about who may read what.
//
// Reference forms:
//
//	${{ tile.<slug>.<output> }}   tile or managed resource in the consumer's env
//	${{ stack.<slug>.<output> }}  stack-scoped singleton
//	${{ org.<slug>.<output> }}    org-scoped singleton
//	${{ stack.vars.<name> }}      stack-wide plain variable
//	${{ stack.secrets.<name> }}   stack-wide secret
//	${{ org.vars.<name> }}        org-wide plain variable
//	${{ org.secrets.<name> }}     org-wide secret
//	${{ stackr.<NAME> }}          value the server supplies about itself
//	                              (PROXY_IP, PROXY_CIDR, see platformVar)
//
// vars, secrets and backups are reserved slugs: a tile or shared instance may
// not take one, or ${{ stack.vars.X }} would be ambiguous. The two-part
// ${{ stack.<name> }} / ${{ org.<name> }} forms are gone; they are parse errors
// naming the replacement.
//
// Nothing here silently degrades: a reference that is missing, malformed,
// ambiguous, cyclic, cross-org, unbound or (in Scoped mode) secret is an error.
// An unresolved reference must never reach a container.
package varref

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/envnet"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Mode picks how much the caller is allowed to see. Scoping rules, env
// containment, stack/org membership, resource bindings, cross-org rejection,
// apply in BOTH modes; the only difference is secret values.
type Mode int

const (
	// System is the deploy path: containers need real values, so secrets resolve.
	System Mode = iota
	// Scoped is an external read (API preview, CLI export, autocomplete) by a
	// caller without secrets:read. A secret-derived value is an error, not an
	// omission, silently dropping it hands out a half-built environment that
	// looks complete.
	Scoped
)

// Dep is one thing a tile's variables reference. Drives canvas edges.
type Dep struct {
	Kind string // tile | resource
	ID   string
	Slug string
}

// Resolved is a consumer tile's fully-expanded environment plus what the
// deploy engine needs to make those values reachable.
type Resolved struct {
	Vars     map[string]string
	Deps     []Dep
	Networks []string // shared docker networks the consumer must join
}

// Lines renders the variables as sorted KEY=VALUE strings for container specs.
func (r Resolved) Lines() []string {
	out := make([]string, 0, len(r.Vars))
	for k, v := range r.Vars {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// EnvLines is the shorthand for callers that run a container they cannot attach
// to extra networks, compose projects, db tiles.
// It therefore refuses to return values that need one: those hostnames provably
// won't resolve inside that container, and starting it anyway just moves the
// failure somewhere harder to read. Callers that can attach (the deploy engine)
// use Resolve directly and honour Networks.
func EnvLines(ctx context.Context, store repo.Store, t *repo.Tile) ([]string, error) {
	res, err := New(store).Resolve(ctx, t.ID, System)
	if err != nil {
		return nil, err
	}
	if len(res.Networks) > 0 {
		return nil, fmt.Errorf("%s references a source reachable only over %s, which this run mode can't join; move it into this environment or reference it from a service tile",
			t.Slug, strings.Join(res.Networks, ", "))
	}
	return res.Lines(), nil
}

// Resolver resolves references against the store.
type Resolver struct{ store repo.Store }

func New(store repo.Store) *Resolver { return &Resolver{store: store} }

// refRe matches a reference expression anywhere in a value. The body is
// captured loosely on purpose: a malformed body must produce a clear error
// rather than being left in place as literal text.
var refRe = regexp.MustCompile(`\$\{\{([^}]*)\}\}`)

// HasRef reports whether a value contains a reference expression.
func HasRef(s string) bool { return refRe.MatchString(s) }

// Refs returns the reference bodies in a value, in order of appearance.
func Refs(s string) []string {
	var out []string
	for _, m := range refRe.FindAllStringSubmatch(s, -1) {
		out = append(out, strings.TrimSpace(m[1]))
	}
	return out
}

// Ref is a parsed reference body.
type Ref struct {
	Scope  string // tile | stack | org | stackr
	Slug   string // source slug, or a reserved bucket (vars, secrets, backups)
	Name   string // output or variable name
	Source string // original text, for error messages
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

// Reserved slugs. They sit where a source slug would, so nothing else may be
// named one: Reserved gates tile and shared-instance names at validation time.
const (
	BucketVars    = "vars"
	BucketSecrets = "secrets"
	BucketBackups = "backups"
)

// Reserved reports whether a slug is one of the reference buckets. The list
// itself lives in repo so handlers can gate a name without importing the
// resolver.
func Reserved(slug string) bool { return repo.ReservedSlug(slug) }

// IsBucket reports whether the ref points at a scope's own values rather than
// at a source in that scope.
func (r Ref) IsBucket() bool { return Reserved(r.Slug) }

// WantSecret reports whether a bucket ref asked for the secret namespace.
func (r Ref) WantSecret() bool { return r.Slug == BucketSecrets }

// Values the platform supplies about itself, as opposed to anything a user
// stored. Closed set, checked at parse time: an unknown name under this scope
// can only ever be a typo, and it is better to say so than to resolve empty.
const (
	// PlatformProxyIP is traefik's own address on the consumer env's network,
	// exact, but only until traefik is recreated and docker hands it another.
	PlatformProxyIP = "PROXY_IP"
	// PlatformProxyCIDR is that network's whole subnet. Survives any traefik
	// restart, at the cost of trusting every container on the network.
	PlatformProxyCIDR = "PROXY_CIDR"
)

var platformNames = map[string]bool{PlatformProxyIP: true, PlatformProxyCIDR: true}

func sortedPlatformNames() []string {
	out := make([]string, 0, len(platformNames))
	for n := range platformNames {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Parse decodes one reference body (the text between ${{ and }}).
func Parse(body string) (Ref, error) {
	b := strings.TrimSpace(body)
	parts := strings.Split(b, ".")
	r := Ref{Source: "${{ " + b + " }}"}
	switch len(parts) {
	case 2:
		r.Scope, r.Name = parts[0], parts[1]
		// stack./org. used to take a bare name. Both namespaces are explicit
		// now, so the old form resolves to nothing and must say which one it
		// meant rather than reading as a singleton with a missing output.
		if r.Scope == "stack" || r.Scope == "org" {
			return r, fmt.Errorf("%s: %s.%s is no longer a reference; write ${{ %s.vars.%s }} or ${{ %s.secrets.%s }}",
				r.Source, r.Scope, r.Name, r.Scope, r.Name, r.Scope, r.Name)
		}
		if r.Scope != "stackr" {
			return r, fmt.Errorf("%s: %q is not a variable scope (want stackr, or scope.vars.NAME)", r.Source, r.Scope)
		}
		if !platformNames[r.Name] {
			return r, fmt.Errorf("%s: %q is not a stackr value (want %s)", r.Source, r.Name, strings.Join(sortedPlatformNames(), " or "))
		}
	case 3:
		r.Scope, r.Slug, r.Name = parts[0], parts[1], parts[2]
		if r.Scope != "tile" && r.Scope != "stack" && r.Scope != "org" && r.Scope != "stackr" {
			return r, fmt.Errorf("%s: %q is not a scope (want tile, stack, org or stackr)", r.Source, r.Scope)
		}
		switch {
		case r.Slug == BucketVars || r.Slug == BucketSecrets:
			if r.Scope != "stack" && r.Scope != "org" {
				return r, fmt.Errorf("%s: only stack and org carry %s", r.Source, r.Slug)
			}
		case r.Slug == BucketBackups:
			if r.Scope != "org" && r.Scope != "stackr" {
				return r, fmt.Errorf("%s: backups live under org or stackr", r.Source)
			}
		case r.Scope == "stackr":
			return r, fmt.Errorf("%s: stackr has no sources; write ${{ stackr.<NAME> }}", r.Source)
		}
	default:
		return r, fmt.Errorf("%s: expected scope.name or scope.source.output", r.Source)
	}
	for _, p := range parts[1:] {
		if !nameRe.MatchString(p) {
			return r, fmt.Errorf("%s: %q is not a valid name", r.Source, p)
		}
	}
	return r, nil
}

// Resolve expands every variable owned by the consumer tile.
func (rr *Resolver) Resolve(ctx context.Context, consumerTileID string, mode Mode) (Resolved, error) {
	out := Resolved{Vars: map[string]string{}}
	consumer, err := rr.store.GetTile(ctx, consumerTileID)
	if err != nil {
		return out, err
	}
	if consumer == nil {
		return out, fmt.Errorf("tile %s not found", consumerTileID)
	}
	stack, err := rr.store.GetStack(ctx, consumer.StackID)
	if err != nil || stack == nil {
		return out, fmt.Errorf("stack for tile %s: %w", consumer.Slug, errOrMissing(err, "not found"))
	}
	s := &session{r: rr, ctx: ctx, mode: mode, consumer: consumer, stack: stack,
		deps: map[string]Dep{}, nets: map[string]bool{}, active: map[string]bool{}}

	vars, err := rr.store.ListVariables(ctx, repo.OwnerTile, consumer.ID)
	if err != nil {
		return out, err
	}
	for _, v := range vars {
		val, err := s.expand("tile:"+consumer.ID+":"+v.Name, v.Value, v.Secret)
		if err != nil {
			return out, fmt.Errorf("%s: %w", v.Name, err)
		}
		out.Vars[v.Name] = val
	}
	// The tile's endpoint port, injected under the name the app expects. An
	// explicit variable of the same name wins, this is a convenience, not a
	// lock the operator can't override.
	if consumer.EndpointPortVar != "" && consumer.ContainerPort != 0 {
		if _, taken := out.Vars[consumer.EndpointPortVar]; !taken {
			out.Vars[consumer.EndpointPortVar] = strconv.Itoa(consumer.ContainerPort)
		}
	}
	s.sniffLiteralDeps(out.Vars)
	for _, d := range s.deps {
		out.Deps = append(out.Deps, d)
	}
	sort.Slice(out.Deps, func(i, j int) bool { return out.Deps[i].ID < out.Deps[j].ID })
	for n := range s.nets {
		// Blank means the provider has never deployed, so it holds no pooled
		// overlay yet (infra/netpool). Joining "" would fail the deploy on a
		// dependency that simply is not up.
		if n == "" {
			continue
		}
		out.Networks = append(out.Networks, n)
	}
	sort.Strings(out.Networks)
	return out, nil
}

// ExpandStrings resolves references inside arbitrary strings the consumer tile
// owns, volume lines and command overrides, which are not variables and so
// never pass through Resolve. Strings without references come back untouched;
// any failure is an error under the same never-run-unresolved contract as
// Resolve. Network/dependency side effects are not reported: a path or argv
// fragment is not an address the container must reach.
func (rr *Resolver) ExpandStrings(ctx context.Context, consumerTileID string, mode Mode, in []string) ([]string, error) {
	out := make([]string, len(in))
	copy(out, in)
	any := false
	for _, s := range in {
		if HasRef(s) {
			any = true
			break
		}
	}
	if !any {
		return out, nil
	}
	consumer, err := rr.store.GetTile(ctx, consumerTileID)
	if err != nil {
		return nil, err
	}
	if consumer == nil {
		return nil, fmt.Errorf("tile %s not found", consumerTileID)
	}
	stack, err := rr.store.GetStack(ctx, consumer.StackID)
	if err != nil || stack == nil {
		return nil, fmt.Errorf("stack for tile %s: %w", consumer.Slug, errOrMissing(err, "not found"))
	}
	s := &session{r: rr, ctx: ctx, mode: mode, consumer: consumer, stack: stack,
		deps: map[string]Dep{}, nets: map[string]bool{}, active: map[string]bool{}}
	for i, v := range in {
		ev, err := s.expand("tile:"+consumer.ID+":<inline>", v, false)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", v, err)
		}
		out[i] = ev
	}
	return out, nil
}

// session carries one Resolve call's context and caches.
type session struct {
	r        *Resolver
	ctx      context.Context
	mode     Mode
	consumer *repo.Tile
	stack    *repo.Stack

	deps   map[string]Dep
	nets   map[string]bool
	active map[string]bool // variables currently being expanded, cycle detection

	envTiles  []repo.Tile
	envRes    []repo.ManagedResource
	loadedEnv bool
	bound     map[string]bool
}

// expand substitutes every reference in one variable's value. key identifies
// the variable being expanded so a reference chain that comes back to it is
// caught instead of recursing forever. secret marks the value's own
// sensitivity: a secret variable's value is refused in Scoped mode even when
// it holds no references at all.
func (s *session) expand(key, val string, secret bool) (string, error) {
	if secret && s.mode == Scoped {
		return "", errSecret(key)
	}
	// The old ${secret.name} grammar is gone. It doesn't match refRe, so left
	// alone it would sail into the container as literal text, fail loudly
	// instead, naming the fix.
	if strings.Contains(val, "${secret.") {
		return "", fmt.Errorf("%s still uses the removed ${secret.*} syntax; rewrite it as ${{ stack.secrets.<name> }} or ${{ tile.<slug>.<output> }}", key)
	}
	if !HasRef(val) {
		return val, nil
	}
	if s.active[key] {
		return "", fmt.Errorf("reference cycle through %s", key)
	}
	s.active[key] = true
	defer delete(s.active, key)

	var firstErr error
	out := refRe.ReplaceAllStringFunc(val, func(m string) string {
		body := refRe.FindStringSubmatch(m)[1]
		r, err := Parse(body)
		if err == nil {
			var v string
			v, err = s.lookup(r)
			if err == nil {
				return v
			}
		}
		if firstErr == nil {
			firstErr = err
		}
		return m
	})
	if firstErr != nil {
		return "", firstErr
	}
	return out, nil
}

// lookup resolves one reference to its value, recording dependency and network
// metadata on the way.
func (s *session) lookup(r Ref) (string, error) {
	switch r.Slug {
	case "":
		return s.platformVar(r) // Parse leaves only stackr.<NAME> here
	case BucketVars, BucketSecrets:
		return s.scopeVar(r)
	case BucketBackups:
		// A destination holds credentials, so it is reference-only and only
		// from a tile's backup: block. Resolving one into a container
		// environment would hand them out.
		return "", fmt.Errorf("%s: a backup destination is referenced from a tile's backup: block, not from a variable", r.Source)
	}
	switch r.Scope {
	case "tile":
		return s.envSource(r)
	case "stack", "org":
		return s.singleton(r)
	}
	return "", fmt.Errorf("%s: unsupported reference", r.Source)
}

// platformVar resolves ${{ stackr.<NAME> }}, what the stackr server can say
// about itself, read from the consumer's own environment row. The values are
// per-env because traefik joins every environment network and holds a different
// address on each; the scope is named for the server because the server, not
// the user, is what supplies them.
//
// Never expanded: these are recorded by the proxy, not authored, so they cannot
// themselves contain a reference. Not secret either, a proxy address is not a
// credential, so Scoped mode reads them like anything else.
//
// An unset value is an error rather than "". The one caller that matters is
// TRUSTED_PROXIES, where a blank does not fail: the app simply stops trusting
// the forwarded header and silently attributes every request to the proxy.
func (s *session) platformVar(r Ref) (string, error) {
	env, err := s.r.store.GetEnvironment(s.ctx, s.consumer.EnvironmentID)
	if err != nil {
		return "", err
	}
	if env == nil {
		return "", fmt.Errorf("%s: consumer tile has no environment", r.Source)
	}
	var v string
	switch r.Name {
	case PlatformProxyIP:
		v = env.ProxyIP
	case PlatformProxyCIDR:
		v = env.ProxyCIDR
	default: // Parse rejects anything else; kept so a new name cannot resolve empty.
		return "", fmt.Errorf("%s: unknown stackr value %q", r.Source, r.Name)
	}
	if v == "" {
		return "", fmt.Errorf("%s: not known yet; the proxy records it when it attaches to this environment's network, which has not happened since %s was created", r.Source, env.Slug)
	}
	return v, nil
}

// scopeVar resolves ${{ stack.vars.<name> }}, ${{ stack.secrets.<name> }} and
// their org counterparts. The bucket in the reference must match the row's
// secret flag: a mismatch is a config fault, not an unset value.
// UnsetError is the one resolution failure that is not a fault in the config:
// the file names a value and nobody has set it yet. Everything else here is a
// typo or a broken wire; this is a stack that is simply not configured, which
// the plan already said it was allowed to be. Typed so the deploy engine can
// park the tile as waiting instead of painting the card red.
type UnsetError struct {
	Source string // the reference as written
	Scope  string // stack | org
	Bucket string // vars | secrets, "" when the caller didn't come from a ref
	Name   string
}

func (e *UnsetError) Error() string {
	what := "var"
	if e.Bucket == BucketSecrets {
		what = "secret"
	}
	return fmt.Sprintf("%s: no %s %s named %q", e.Source, e.Scope, what, e.Name)
}

// errBucket is a name that exists in the other namespace. Deliberately NOT an
// UnsetError: the value is set, the reference is wrong, so parking the tile as
// waiting would wait forever. A plain error fails the deploy loudly instead.
func errBucket(r Ref) error {
	other := BucketVars
	if r.Slug == BucketVars {
		other = BucketSecrets
	}
	return fmt.Errorf("%s: %s is a %s at %s level; write ${{ %s.%s.%s }}",
		r.Source, r.Name, strings.TrimSuffix(other, "s"), r.Scope, r.Scope, other, r.Name)
}

// Unset reports whether err is (or wraps) an unset reference, and which name.
func Unset(err error) (string, bool) {
	var ue *UnsetError
	if errors.As(err, &ue) {
		return ue.Name, true
	}
	return "", false
}

func (s *session) scopeVar(r Ref) (string, error) {
	want := r.WantSecret()
	ownerKind, ownerID := repo.OwnerStack, s.stack.ID
	if r.Scope == "org" {
		ownerKind, ownerID = repo.OwnerOrg, s.stack.OrgID
	} else {
		// A secret is declared at stack level but valued per env: the
		// consumer's env row shadows the stack-wide value, so staging and
		// production never share a session key by accident. Same reference
		// syntax, the indirection is resolution-time.
		envVars, err := s.r.store.ListVariables(s.ctx, repo.OwnerEnv, s.consumer.EnvironmentID)
		if err != nil {
			return "", err
		}
		for _, v := range envVars {
			if v.Name != r.Name {
				continue
			}
			if v.Secret != want {
				return "", errBucket(r)
			}
			return s.expand(repo.OwnerEnv+":"+s.consumer.EnvironmentID+":"+v.Name, v.Value, v.Secret)
		}
	}
	vars, err := s.r.store.ListVariables(s.ctx, ownerKind, ownerID)
	if err != nil {
		return "", err
	}
	for _, v := range vars {
		if v.Name != r.Name {
			continue
		}
		if v.Secret != want {
			return "", errBucket(r)
		}
		// Recurse: a stack variable may itself reference another.
		return s.expand(ownerKind+":"+ownerID+":"+v.Name, v.Value, v.Secret)
	}
	return "", &UnsetError{Source: r.Source, Scope: r.Scope, Bucket: r.Slug, Name: r.Name}
}

// envSource resolves ${{ tile.<slug>.<output> }} against the consumer's own
// environment. Tiles and managed resources share one slug namespace, so a
// collision is an error naming both candidates rather than a coin flip.
func (s *session) envSource(r Ref) (string, error) {
	if err := s.loadEnv(); err != nil {
		return "", err
	}
	var tile *repo.Tile
	for i := range s.envTiles {
		if s.envTiles[i].Slug == r.Slug {
			tile = &s.envTiles[i]
			break
		}
	}
	var res *repo.ManagedResource
	for i := range s.envRes {
		if s.envRes[i].Slug == r.Slug {
			res = &s.envRes[i]
			break
		}
	}
	switch {
	case tile != nil && res != nil:
		return "", fmt.Errorf("%s: %q is both a tile and a managed resource in this environment; rename one", r.Source, r.Slug)
	case tile != nil:
		return s.tileOutput(tile, r)
	case res != nil:
		return s.resourceOutput(res, r)
	}
	return "", fmt.Errorf("%s: no tile or resource named %q in this environment", r.Source, r.Slug)
}

// singleton resolves ${{ stack.<slug>.<output> }} / ${{ org.<slug>.<output> }}
// against tiles shared at that scope. Scope membership is what makes this
// safe: a stack-scoped tile in another stack, or an org-scoped tile in another
// org, is not a candidate at all.
func (s *session) singleton(r Ref) (string, error) {
	tiles, err := s.scopeTiles(r.Scope)
	if err != nil {
		return "", err
	}
	var match *repo.Tile
	for i := range tiles {
		t := &tiles[i]
		if t.ScopeKind != r.Scope || t.Slug != r.Slug {
			continue
		}
		if match != nil {
			return "", fmt.Errorf("%s: %q matches more than one %s-scoped tile", r.Source, r.Slug, r.Scope)
		}
		match = t
	}
	if match == nil {
		return "", fmt.Errorf("%s: no %s-scoped source named %q", r.Source, r.Scope, r.Slug)
	}
	return s.tileOutput(match, r)
}

// scopeTiles lists the tiles that could be a stack- or org-scoped singleton.
// scans the stack (or every stack in the org), tens of rows, and
// scope_kind isn't indexed. Add a store query if an org ever grows big enough
// to notice.
func (s *session) scopeTiles(scope string) ([]repo.Tile, error) {
	if scope == "stack" {
		return s.r.store.ListTilesByStack(s.ctx, s.stack.ID)
	}
	stacks, err := s.r.store.ListStacksByOrg(s.ctx, s.stack.OrgID)
	if err != nil {
		return nil, err
	}
	var out []repo.Tile
	for _, st := range stacks {
		ts, err := s.r.store.ListTilesByStack(s.ctx, st.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, ts...)
	}
	return out, nil
}

// tileOutput reads one output off a tile: its own variables today, plus the
// STACKR_* endpoint outputs R3 adds.
func (s *session) tileOutput(t *repo.Tile, r Ref) (string, error) {
	if t.ID != s.consumer.ID { // a tile reading its own outputs isn't a dependency
		s.deps["tile:"+t.ID] = Dep{Kind: "tile", ID: t.ID, Slug: t.Slug}
	}
	if t.EnvironmentID != s.consumer.EnvironmentID {
		// Reaching across environments only works over the shared network.
		s.nets[t.SharedNet()] = true
	}
	eps, err := s.endpointOutputs(t)
	if err != nil {
		return "", err
	}
	if v, ok := eps[r.Name]; ok {
		return v, nil
	}
	vars, err := s.r.store.ListVariables(s.ctx, repo.OwnerTile, t.ID)
	if err != nil {
		return "", err
	}
	for _, v := range vars {
		if v.Name != r.Name {
			continue
		}
		return s.expand("tile:"+t.ID+":"+v.Name, v.Value, v.Secret)
	}
	return "", fmt.Errorf("%s: %s publishes no output named %q", r.Source, t.Slug, r.Name)
}

// resourceOutput reads one output off a managed resource. Referencing is not
// access: without an active binding the reference fails.
func (s *session) resourceOutput(res *repo.ManagedResource, r Ref) (string, error) {
	if err := s.loadBindings(); err != nil {
		return "", err
	}
	if !s.bound[res.ID] {
		return "", fmt.Errorf("%s: %s is not attached to %s; attach it first", r.Source, res.Slug, s.consumer.Slug)
	}
	outs, err := s.r.store.ListOutputs(s.ctx, res.ID)
	if err != nil {
		return "", err
	}
	for _, o := range outs {
		if o.Name != r.Name {
			continue
		}
		if o.Secret && s.mode == Scoped {
			return "", errSecret(res.Slug + "." + o.Name)
		}
		s.deps["resource:"+res.ID] = Dep{Kind: "resource", ID: res.ID, Slug: res.Slug}
		if o.RequiresNetwork {
			prov, err := s.r.store.GetTile(s.ctx, res.ProviderTileID)
			if err != nil {
				return "", err
			}
			if prov == nil {
				return "", fmt.Errorf("%s: %s has no provider tile", r.Source, res.Slug)
			}
			// Only cross-env needs the shared network: an instance in the
			// consumer's own env already answers to this alias there.
			if prov.EnvironmentID != s.consumer.EnvironmentID {
				s.nets[prov.SharedNet()] = true
			}
		}
		return o.Value, nil
	}
	return "", fmt.Errorf("%s: %s publishes no output named %q", r.Source, res.Slug, r.Name)
}

// literalURLRe matches scheme://host[:port], the shape that turns a bare
// slug mention into a real address. Capture group 1 is the host.
var literalURLRe = regexp.MustCompile(`\b[a-z][a-z0-9+.-]*://([A-Za-z0-9._-]+)(?::\d+)?`)

// sniffLiteralDeps records sibling tiles addressed by literal URL
// (ORDERS_API_URL: http://orders-api:8181) as dependencies: same edge on the
// canvas as a ${{ }} reference, no injection or rewriting. Bare slug mentions
// deliberately do NOT match, the scheme is the false-positive guard.
func (s *session) sniffLiteralDeps(vals map[string]string) {
	if err := s.loadEnv(); err != nil {
		return
	}
	bySlug := map[string]*repo.Tile{}
	for i := range s.envTiles {
		bySlug[s.envTiles[i].Slug] = &s.envTiles[i]
	}
	for _, v := range vals {
		for _, m := range literalURLRe.FindAllStringSubmatch(v, -1) {
			t := bySlug[m[1]]
			if t == nil || t.ID == s.consumer.ID || t.IsVolume() {
				continue
			}
			if _, seen := s.deps["tile:"+t.ID]; !seen {
				s.deps["tile:"+t.ID] = Dep{Kind: "tile", ID: t.ID, Slug: t.Slug}
			}
		}
	}
}

func (s *session) loadEnv() error {
	if s.loadedEnv {
		return nil
	}
	ts, err := s.r.store.ListTilesByEnv(s.ctx, s.consumer.EnvironmentID)
	if err != nil {
		return err
	}
	rs, err := s.r.store.ListResourcesByEnv(s.ctx, s.consumer.EnvironmentID)
	if err != nil {
		return err
	}
	s.envTiles, s.envRes, s.loadedEnv = ts, rs, true
	return nil
}

func (s *session) loadBindings() error {
	if s.bound != nil {
		return nil
	}
	bs, err := s.r.store.BindingsForConsumer(s.ctx, s.consumer.ID)
	if err != nil {
		return err
	}
	s.bound = map[string]bool{}
	for _, b := range bs {
		s.bound[b.ResourceID] = true
	}
	return nil
}

// endpointOutputs is a service tile's virtual endpoint: the address siblings
// reach it on. The endpoint is stackr's truth, not the app's, the app's own
// port variable (EndpointPortVar) is only an injection mechanism.
//
// The private domain is the tile alias rather than the slug: it resolves on the
// shared network too, so a cross-env reference keeps working.
func (s *session) endpointOutputs(t *repo.Tile) (map[string]string, error) {
	if t.Kind != "service" || t.ContainerPort == 0 {
		return nil, nil
	}
	host := envnet.TileAlias(t.ID)
	port := strconv.Itoa(t.ContainerPort)
	proto := t.EndpointProtocol
	if proto == "" {
		proto = "http"
	}
	out := map[string]string{
		"STACKR_PRIVATE_DOMAIN": host,
		"STACKR_INTERNAL_PORT":  port,
	}
	if proto == "http" || proto == "https" { // tcp has no URL form
		out["STACKR_INTERNAL_URL"] = proto + "://" + host + ":" + port
	}
	doms, err := s.r.store.ListDomainsByTile(s.ctx, t.ID)
	if err != nil {
		return nil, err
	}
	for _, d := range doms {
		if d.RedirectTo != "" { // a redirect isn't an address for this tile
			continue
		}
		scheme := "http"
		if d.HTTPS {
			scheme = "https"
		}
		out["STACKR_PUBLIC_URL"] = scheme + "://" + d.Host
		// The bare host, for consumers that want no scheme (cookie domains,
		// CORS lists, --host flags), mangling the URL back apart is worse.
		out["STACKR_PUBLIC_DOMAIN"] = d.Host
		break
	}
	return out, nil
}

func errSecret(what string) error {
	return fmt.Errorf("%s is a secret; this caller needs secrets:read to resolve it", what)
}

func errOrMissing(err error, msg string) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("%s", msg)
}
