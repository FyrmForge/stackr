# config/varref

- **Source:** `config/varref/` — `varref.go`, `catalogue.go`, `usage.go`, `varref_test.go`, `endpoint_test.go`, `platform_test.go`, `usage_test.go`
- **Commit:** c2423f0
- **Taken:** the `${{ }}` parser, the never-run-unresolved contract, `UnsetError` vs hard error, binding-gated managed outputs, the endpoint outputs, cross-scope network joining
- **Cut:** `vars`/`secrets` buckets, tile-owned variables as outputs, nested refs + cycle detection, literal-URL dependency sniffing, `Mode`/`Scoped` secret gating, `Catalogue`, `ScopeVarsUsed`, `org.storage`, the store-bound `Resolve`/`ExpandStrings`/`EnvLines` wrappers
- **Cuts belong to:** `leaf/params` (the store), `leaf/tile` (env block, storage key), `flow/deploy` (reading the snapshot, injecting env), `leaf/graph` (canvas edges), the API layer (autocomplete, secrets permission)

Target package `service/internal/leaf/params`. Everything below is rewritten
against the new grammar; the old two-part `stack.NAME` / `${secret.*}` migration
errors are gone with it (nothing is deployed anywhere, so nothing needs the
upgrade path).

## Parser

```go
// Package params resolves ${{ ... }} references. One resolver behind deploy,
// previews and CLI export: a second implementation would be a second set of
// rules about who may read what.
//
//	${{ params.<collection>.<name> }}      env then stack, nearest wins, stops at stack
//	${{ org.params.<collection>.<name> }}  org, deliberately
//	${{ self.<output> }}                   the consumer's own outputs
//	${{ tile.<slug>.<output> }}            sibling tile or managed instance, same env
//	${{ stack.<slug>.<output> }}           shared tile at stack scope
//	${{ org.<slug>.<output> }}             shared tile at org scope
//	${{ stackr.<NAME> }}                   what the server says about itself
//	${{ org.backups.<name> }}              a backup destination
//
// Nothing here silently degrades. An unset param parks the tile as waiting
// (UnsetError); every other failure fails the deploy with its message. Nothing
// unresolved ever reaches a container.
package params

// refRe matches a reference anywhere in a value. The body is captured loosely
// on purpose: a malformed body must produce a clear error rather than being
// left in place as literal text.
var refRe = regexp.MustCompile(`\$\{\{([^}]*)\}\}`)

// HasRef reports whether a value contains a reference.
func HasRef(s string) bool { return refRe.MatchString(s) }

// Refs returns the reference bodies in a value, in order of appearance. The
// config planner and anything drawing edges parse with this and never touch the
// resolver.
func Refs(s string) []string {
	var out []string
	for _, m := range refRe.FindAllStringSubmatch(s, -1) {
		out = append(out, strings.TrimSpace(m[1]))
	}
	return out
}

// Kind is which of the eight forms a ref is. Callers gate on it: where a ref may
// appear is their rule, not the resolver's (see "Domain rules").
type Kind string

const (
	KindParam    Kind = "param"
	KindOrgParam Kind = "org_param"
	KindSelf     Kind = "self"
	KindTile     Kind = "tile"
	KindStack    Kind = "stack"
	KindOrg      Kind = "org"
	KindStackr   Kind = "stackr"
	KindBackup   Kind = "backup"
)

// Ref is a parsed reference body.
type Ref struct {
	Kind   Kind
	Slug   string // tile slug, or the param collection
	Name   string // output, param name, stackr NAME, backup name
	Source string // the ref exactly as written; every error message carries it,
	              // which is how a failure points at the line that caused it
}

// Dots are separators, never part of a name.
var (
	nameRe = regexp.MustCompile(`^[a-z0-9_]+$`)   // collections, param names, outputs
	slugRe = regexp.MustCompile(`^[a-z0-9_-]+$`)  // tile slugs, backup names
)

// Values the platform supplies about itself. Closed set, checked at parse time:
// an unknown name under this scope can only ever be a typo, and it is better to
// say so than to resolve empty. The one caller that matters is TRUSTED_PROXIES,
// where a blank does not fail — the tile just stops trusting the forwarded
// header and silently credits every request to the proxy.
const (
	ProxyIP   = "PROXY_IP"
	ProxyCIDR = "PROXY_CIDR"
)

var platformNames = map[string]bool{ProxyIP: true, ProxyCIDR: true}

// Parse decodes one reference body (the text between ${{ and }}).
// extract: rewritten for new grammar — switches on the leading token instead of
// on part count, because org.params.<c>.<n> is four parts and self.<output> two.
func Parse(body string) (Ref, error) {
	b := strings.TrimSpace(body)
	p := strings.Split(b, ".")
	r := Ref{Source: "${{ " + b + " }}"}
	switch {
	case len(p) == 3 && p[0] == "params":
		r.Kind, r.Slug, r.Name = KindParam, p[1], p[2]
	case len(p) == 4 && p[0] == "org" && p[1] == "params":
		r.Kind, r.Slug, r.Name = KindOrgParam, p[2], p[3]
	case len(p) == 3 && p[0] == "org" && p[1] == "backups":
		r.Kind, r.Slug, r.Name = KindBackup, "", p[2]
	case len(p) == 2 && p[0] == "self":
		r.Kind, r.Name = KindSelf, p[1]
	case len(p) == 2 && p[0] == "stackr":
		if !platformNames[p[1]] {
			return r, fmt.Errorf("%s: %q is not a stackr value (want %s or %s)", r.Source, p[1], ProxyCIDR, ProxyIP)
		}
		r.Kind, r.Name = KindStackr, p[1]
		return r, nil
	case len(p) == 3 && (p[0] == "tile" || p[0] == "stack" || p[0] == "org"):
		// params is the only reserved slug: without this, ${{ org.params.x.y }}
		// and a tile named params are the same sentence.
		if p[1] == "params" {
			return r, fmt.Errorf("%s: params is reserved; write ${{ %s.params.<collection>.<name> }}", r.Source, p[0])
		}
		r.Kind, r.Slug, r.Name = Kind(p[0]), p[1], p[2]
	default:
		return r, fmt.Errorf("%s: want params.<collection>.<name>, org.params.<collection>.<name>, "+
			"self.<output>, tile|stack|org.<slug>.<output>, stackr.<NAME> or org.backups.<name>", r.Source)
	}
	if r.Slug != "" && !slugRe.MatchString(r.Slug) {
		return r, fmt.Errorf("%s: %q is not a valid name", r.Source, r.Slug)
	}
	if !nameRe.MatchString(r.Name) && !slugRe.MatchString(r.Name) {
		return r, fmt.Errorf("%s: %q is not a valid name", r.Source, r.Name)
	}
	return r, nil
}
```

## What the resolver is allowed to read

```go
// Snapshot is everything the resolver may see, gathered by the caller before it
// starts. The resolver makes no store call: it is a pure function of this, which
// is what lets previews, CLI export and deploy share one set of rules.
// extract: rewritten for new grammar — replaces repo.Store + the session's
// lazy loads; the four-owner variable cascade is gone with the buckets.
type Snapshot struct {
	EnvParams   map[string]string // "<collection>.<name>" -> value, consumer's env
	StackParams map[string]string
	OrgParams   map[string]string
	Stackr      map[string]string // PROXY_IP / PROXY_CIDR, read off the consumer's env row
	Backups     map[string]string // backup name -> destination
	Self        Source            // the consumer's own outputs
	Tiles       map[string]Source // by slug, the consumer's env
	Stack       map[string]Source // stack-scoped shared tiles
	Org         map[string]Source // org-scoped shared tiles
}

// Source is one referenceable tile as the caller found it. Outputs are the
// built-ins only — nothing reads another tile's env.
type Source struct {
	Outputs  map[string]string
	Managed  bool
	Attached bool   // managed only: the consumer has a binding
	Network  string // shared network the consumer must join; "" when the source
	                // already answers on the consumer's own env network
}

// Resolver expands refs against one snapshot and records what the deploy engine
// must do to make the values reachable.
type Resolver struct {
	snap Snapshot
	deps map[string]bool
	nets map[string]bool
}

func New(s Snapshot) *Resolver {
	return &Resolver{snap: s, deps: map[string]bool{}, nets: map[string]bool{}}
}

// Deps are the slugs this tile ended up referencing (canvas edges).
// Networks are the shared docker networks the consumer must join, sorted.
func (rr *Resolver) Deps() []string     { return sortedKeys(rr.deps) }
func (rr *Resolver) Networks() []string { return sortedKeys(rr.nets) }
```

## Expansion and the never-run-unresolved contract

```go
// Expand substitutes every reference in one string. Callers loop it over env
// values, the command override, domains and a volume's backup dest; a string
// with no reference comes back untouched, and any failure is an error — never a
// value left half-expanded.
// extract: rewritten for new grammar — no recursion and no cycle detection,
// because a param value is literal.
func (rr *Resolver) Expand(val string) (string, error) {
	if !HasRef(val) {
		return val, nil
	}
	var firstErr error
	out := refRe.ReplaceAllStringFunc(val, func(m string) string {
		ref, err := Parse(refRe.FindStringSubmatch(m)[1])
		if err == nil {
			var v string
			if v, err = rr.lookup(ref); err == nil {
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

func (rr *Resolver) lookup(r Ref) (string, error) {
	switch r.Kind {
	case KindParam:
		// env, then stack, nearest wins. Stops at stack: org is a trust boundary
		// and is never reached by accident, only by an explicit org.params ref.
		if v, ok := rr.snap.EnvParams[r.Slug+"."+r.Name]; ok {
			return v, nil
		}
		if v, ok := rr.snap.StackParams[r.Slug+"."+r.Name]; ok {
			return v, nil
		}
		return "", &UnsetError{Source: r.Source, Scope: "stack", Collection: r.Slug, Name: r.Name}
	case KindOrgParam:
		if v, ok := rr.snap.OrgParams[r.Slug+"."+r.Name]; ok {
			return v, nil
		}
		return "", &UnsetError{Source: r.Source, Scope: "org", Collection: r.Slug, Name: r.Name}
	case KindStackr:
		// Recorded by the proxy, not authored, so it can hold no reference and is
		// no credential either. Unset is an error, never "".
		if v := rr.snap.Stackr[r.Name]; v != "" {
			return v, nil
		}
		return "", fmt.Errorf("%s: not known yet; the proxy records it when it attaches to this environment's network", r.Source)
	case KindBackup:
		if v, ok := rr.snap.Backups[r.Name]; ok {
			return v, nil
		}
		return "", fmt.Errorf("%s: this org has no backup destination named %q", r.Source, r.Name)
	case KindSelf:
		// Its own outputs are not a dependency and need no network.
		v, ok := rr.snap.Self.Outputs[r.Name]
		if !ok {
			return "", fmt.Errorf("%s: this tile publishes no output named %q", r.Source, r.Name)
		}
		return v, nil
	case KindTile:
		return rr.source(r, rr.snap.Tiles, "tile or managed instance in this environment")
	case KindStack:
		return rr.source(r, rr.snap.Stack, "stack-scoped source")
	case KindOrg:
		return rr.source(r, rr.snap.Org, "org-scoped source")
	}
	return "", fmt.Errorf("%s: unsupported reference", r.Source)
}

func (rr *Resolver) source(r Ref, in map[string]Source, what string) (string, error) {
	src, ok := in[r.Slug]
	if !ok {
		return "", fmt.Errorf("%s: no %s named %q", r.Source, what, r.Slug)
	}
	// Referencing is not access: a managed instance answers only to a consumer
	// that is attached to it.
	if src.Managed && !src.Attached {
		return "", fmt.Errorf("%s: %s is not attached to this tile; attach it first", r.Source, r.Slug)
	}
	v, ok := src.Outputs[r.Name]
	if !ok {
		return "", fmt.Errorf("%s: %s publishes no output named %q", r.Source, r.Slug, r.Name)
	}
	rr.deps[r.Slug] = true
	// A shared tile sits outside the consumer's env, so reaching it works only
	// over its own network: referencing one joins it. Blank means the source has
	// never deployed and holds no network yet — joining "" would fail the deploy
	// on a dependency that is simply not up.
	if src.Network != "" {
		rr.nets[src.Network] = true
	}
	return v, nil
}
```

## Unset parks, everything else fails

```go
// UnsetError is the one resolution failure that is not a fault in the config:
// the file names a param and nobody has set it yet. Typed so the deploy engine
// can park the tile as waiting instead of painting the card red; setting the
// param triggers the redeploy. A typo, an unknown tile, an unattached managed
// instance, an output that does not exist — all of those are wires that will
// never resolve, so they fail the deploy loudly rather than waiting forever.
// extract: rewritten for new grammar — Bucket becomes Collection; the
// "set, but in the other namespace" hard error goes with the buckets.
type UnsetError struct {
	Source     string // the reference as written
	Scope      string // stack | org
	Collection string
	Name       string
}

func (e *UnsetError) Error() string {
	return fmt.Sprintf("%s: no %s param named %s.%s", e.Source, e.Scope, e.Collection, e.Name)
}

// Unset reports whether err is (or wraps) an unset param, and which one.
func Unset(err error) (string, bool) {
	var ue *UnsetError
	if errors.As(err, &ue) {
		return ue.Collection + "." + ue.Name, true
	}
	return "", false
}
```

## Endpoint outputs

```go
// Endpoint is a service tile's address as stackr knows it. The address is
// stackr's truth, not the tile's: the tile is told, it does not declare.
type Endpoint struct {
	// Alias is the tile's network alias, not its slug: it resolves on a shared
	// network as well as the env's, so a stack-/org-scoped reference keeps
	// working, and it survives a rename.
	Alias    string
	Port     int
	Protocol string   // http | https | tcp
	Domains  []Domain // in the tile's own order
}

type Domain struct {
	Host     string
	HTTPS    bool
	Redirect bool // a redirect is not an address for this tile
}

// Outputs is the built-in set a service tile publishes. An absent name is the
// contract, not an oversight: tcp has no url, and public_* is absent until a
// non-redirect domain exists — a ref to an absent output fails the deploy
// instead of resolving to "".
// extract: rewritten for new grammar — STACKR_PRIVATE_DOMAIN -> host,
// STACKR_INTERNAL_PORT -> port, STACKR_INTERNAL_URL -> url,
// STACKR_PUBLIC_URL -> public_url, STACKR_PUBLIC_DOMAIN -> public_domain.
func (e Endpoint) Outputs() map[string]string {
	if e.Port == 0 {
		return nil
	}
	port := strconv.Itoa(e.Port)
	proto := e.Protocol
	if proto == "" {
		proto = "http"
	}
	out := map[string]string{"host": e.Alias, "port": port}
	if proto == "http" || proto == "https" { // tcp has no URL form
		out["url"] = proto + "://" + e.Alias + ":" + port
	}
	for _, d := range e.Domains {
		if d.Redirect {
			continue
		}
		scheme := "http"
		if d.HTTPS {
			scheme = "https"
		}
		out["public_url"] = scheme + "://" + d.Host
		// The bare host too, for consumers that want no scheme (cookie domains,
		// CORS lists, --host flags): mangling the URL back apart is worse.
		out["public_domain"] = d.Host
		break
	}
	return out
}
```

## Cuts

```go
// extract: dropped the vars/secrets buckets, scopeVar, errBucket, Reserved and
//   the four-owner cascade, belongs in leaf/params (one store, kind fixed at creation).
// extract: dropped tile-owned variables as outputs (tileOutput's ListVariables
//   fallback), belongs nowhere — nothing reads another tile's env.
// extract: dropped nested refs, the recursive re-expand and the `active` cycle
//   set, belongs nowhere — a param value is literal.
// extract: dropped literalURLRe / sniffLiteralDeps and Dep{Kind,ID}, belongs in
//   leaf/graph; Deps() returns slugs.
// extract: dropped Mode/System/Scoped and errSecret, belongs in leaf/params plus
//   the API permission check — the resolver holds no auth decision.
// extract: dropped EndpointPortVar injection (the container port written into the
//   tile's own env under the tile's chosen key, explicit value wins), belongs in
//   flow/deploy where the env block is built.
// extract: dropped Catalogue/Source/Output (autocomplete listing), belongs in the
//   API layer over leaf/params; the output half is now Endpoint.Outputs' key set.
// extract: dropped ScopeVarsUsed, belongs in leaf/graph — Refs+Parse is all it used.
// extract: dropped org.storage and OrgStorageRef, belongs in leaf/tile: an org
//   share is its own tile key, not a ref in a volume line.
// extract: dropped Resolve/ExpandStrings/EnvLines and repo.Store, belongs in
//   flow/deploy: it reads the snapshot and loops Expand over its own strings.
// extract: dropped the ${secret.*} and two-part stack.NAME migration errors,
//   belongs nowhere — nothing is deployed, so nothing needs an upgrade path.
```

## Domain rules seen (spec)

- Precedence: `params.<c>.<n>` reads env, then stack, nearest wins, and **stops at stack**; org is reached only by `org.params.<c>.<n>`, which takes no override.
- Parks (`UnsetError`, tile goes `waiting`, setting the param redeploys): a `params.`/`org.params.` name nobody has set yet. Nothing else parks.
- Fails the deploy: a malformed body, an unknown scope, an unknown tile/stack/org slug, an output the source does not publish, a managed instance the consumer is not attached to, an unset `stackr.` value, an unknown backup name.
- Never-run-unresolved: a failed expansion returns an error and no value; a ref is never left as literal text in what reaches a container.
- Error text always carries the reference as written (`${{ … }}`), which is how a failure points back at the line that caused it.
- Binding gate: a managed ref fails unless the consumer is attached — referencing is not access, and the gate is on the source, not on the output name.
- Service outputs, built-ins only: `host` (network alias, not slug), `port` (container port), `url` (`proto://host:port`, absent for tcp), `public_url` (`scheme://host` of the first non-redirect domain, https when the domain has TLS), `public_domain` (that domain's bare host). Absent output = hard error, never `""`.
- Network joining: a `stack.`/`org.` ref (and a cross-env managed instance) makes the consumer join the shared tile's network; a blank network means the source has never deployed and is skipped, so the deploy fails on the dependency being down rather than on a nonexistent network.
- Reserved slug: `params` only. A tile may not take it.
- Where refs may appear is the caller's rule, gated on `Ref.Kind`: env values and the command override take everything but `KindBackup`; a domain takes `KindParam`/`KindOrgParam` only (a tile output there is a cycle) and never a secret-kind entry; a volume's backup `dest` takes `KindBackup` only.
- Name grammar: dots are separators. Collections, param names and outputs are `[a-z0-9_]+`; `stackr.` names are the closed uppercase set, checked at parse time so a typo cannot resolve empty.

## Notes for the builder

- `Snapshot` is the whole layering trick: `flow/deploy` reads the store once, fills it, and the resolver stays a pure function — the same call shape then serves previews and CLI export without a second set of read rules.
- Secret filtering happens while filling `Snapshot`, not in here. A caller without the secrets permission builds a snapshot whose secret entries are absent, which surfaces as a hard error (not a silent blank) the moment one is referenced. That is deliberate: a half-built env is indistinguishable from a complete one.
- `Expand` is the only entry point; there is no map or slice variant. Callers own their own strings and decide which `Ref.Kind` is legal where.
- The old code joined a network only when the source lived in another env. Here the caller decides by setting `Source.Network`, so a same-env source leaves it blank.
- Tile slugs kept hyphens in the old code (`orders-db`) while REWRITE spells slugs `[a-z0-9_]+`; `slugRe` allows both. Tighten it if the slug validator lands without hyphens.
- Run modes that cannot attach extra networks (compose runs, one-shot jobs) must refuse when `Networks()` is non-empty rather than start a container whose hostname provably will not resolve. That check is the caller's, one `if` at the run site.

Size: source 1538 lines, extract 433 lines
