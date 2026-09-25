package params

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/slug"
)

// The one resolver behind deploy, previews and CLI export:
//
//	${{ params.<collection>.<name> }}      env then stack, nearest wins, stops at stack
//	${{ org.params.<collection>.<name> }}  org, deliberately
//	${{ self.<output> }}                   the consumer's own outputs
//	${{ tile.<slug>.<output> }}            sibling tile or managed instance, same env
//	${{ stack.<slug>.<output> }}           shared tile at stack scope
//	${{ org.<slug>.<output> }}             shared tile at org scope
//	${{ stackr.<NAME> }}                   what the server says about itself
//	${{ org.backups.<name> }}              a backup destination
//	${{ env.name }}                        the consumer's env slug (provision_from only)
//
// An unset param is errs.Unset (the job parks); every other failure is a
// hard error carrying the ref as written. Nothing unresolved ever reaches a
// container.

// refRe captures the body loosely: a malformed body must be an error, never
// left in place as literal text.
var refRe = regexp.MustCompile(`\$\{\{([^}]*)\}\}`)

func HasRef(s string) bool { return refRe.MatchString(s) }

// Refs are the bodies in a value, in order.
func Refs(s string) []string {
	var out []string
	for _, m := range refRe.FindAllStringSubmatch(s, -1) {
		out = append(out, strings.TrimSpace(m[1]))
	}
	return out
}

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
	KindEnv      Kind = "env"
)

type Ref struct {
	Kind   Kind
	Slug   string // tile slug, or the param collection
	Name   string // output, param name, stackr NAME, backup name
	Source string // the ref as written; every error carries it
}

// The closed set of stackr.<NAME>: an unknown name is a typo, never "".
const (
	ProxyIP   = "PROXY_IP"
	ProxyCIDR = "PROXY_CIDR"
)

var platformNames = map[string]bool{ProxyIP: true, ProxyCIDR: true}

// Parse decodes one body (the text between ${{ and }}).
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
		r.Kind, r.Name = KindBackup, p[2]
		if !slug.Valid(r.Name) {
			return r, fmt.Errorf("%s: %q is not a valid name", r.Source, r.Name)
		}
		return r, nil
	case len(p) == 2 && p[0] == "env":
		if p[1] != "name" {
			return r, fmt.Errorf("%s: the only env ref is ${{ env.name }}", r.Source)
		}
		r.Kind, r.Name = KindEnv, p[1]
		return r, nil
	case len(p) == 2 && p[0] == "self":
		r.Kind, r.Name = KindSelf, p[1]
	case len(p) == 2 && p[0] == "stackr":
		if !platformNames[p[1]] {
			return r, fmt.Errorf("%s: %q is not a stackr value (want %s or %s)", r.Source, p[1], ProxyCIDR, ProxyIP)
		}
		r.Kind, r.Name = KindStackr, p[1]
		return r, nil
	case len(p) == 3 && (p[0] == "tile" || p[0] == "stack" || p[0] == "org"):
		// Without this, ${{ org.params.x.y }} and a tile named params read alike.
		if p[1] == "params" {
			return r, fmt.Errorf("%s: params is reserved; write ${{ %s.params.<collection>.<name> }}", r.Source, p[0])
		}
		r.Kind, r.Slug, r.Name = Kind(p[0]), p[1], p[2]
		if !slug.Valid(r.Slug) {
			return r, fmt.Errorf("%s: %q is not a valid tile name", r.Source, r.Slug)
		}
	default:
		return r, fmt.Errorf("%s: want params.<collection>.<name>, org.params.<collection>.<name>, "+
			"self.<output>, tile|stack|org.<slug>.<output>, stackr.<NAME> or org.backups.<name>", r.Source)
	}
	if (r.Kind == KindParam || r.Kind == KindOrgParam) && !slug.ValidName(r.Slug) {
		return r, fmt.Errorf("%s: %q is not a valid collection name", r.Source, r.Slug)
	}
	if !slug.ValidName(r.Name) {
		return r, fmt.Errorf("%s: %q is not a valid name", r.Source, r.Name)
	}
	return r, nil
}

// Where a ref sits decides which kinds it may use.
type Where string

const (
	InEnv        Where = "env"
	InCommand    Where = "command"
	InDomain     Where = "domain"      // params only, never a secret, never a tile output (a cycle)
	InBackupDest Where = "backup_dest" // org.backups only
	// InProvisionFrom is a slice tile's target: params and env.name only.
	InProvisionFrom Where = "provision_from"
)

func allowed(w Where, k Kind) bool {
	switch w {
	case InEnv, InCommand:
		// ponytail: env.name is for provision_from; an env value could take
		// it too once someone needs the env's name in a container.
		return k != KindBackup && k != KindEnv
	case InProvisionFrom:
		return k == KindParam || k == KindEnv
	case InDomain:
		return k == KindParam || k == KindOrgParam
	case InBackupDest:
		return k == KindBackup
	}
	return false
}

// Value is one param value and whether it is a secret.
type Value struct {
	V      string
	Secret bool
}

// Snapshot is everything the resolver may see, gathered by the caller first:
// the resolver makes no store call. Secret filtering happens while filling
// it (Values with secrets=false), never in here.
type Snapshot struct {
	Env         string           // the consumer's env slug, for ${{ env.name }}
	EnvParams   map[string]Value // "<collection>.<name>", the consumer's env
	StackParams map[string]Value
	OrgParams   map[string]Value
	Stackr      map[string]string // PROXY_IP / PROXY_CIDR
	Backups     map[string]string // backup name -> destination
	Self        Source
	Tiles       map[string]Source // by slug, the consumer's env
	Stack       map[string]Source // stack-scoped shared tiles
	Org         map[string]Source // org-scoped shared tiles
}

// Source is one referenceable tile. Outputs are the built-ins only.
type Source struct {
	Outputs  map[string]string
	Managed  bool
	Attached bool   // managed only: the consumer has a binding
	Network  string // shared network the consumer must join; "" on its own env network
}

type Resolver struct {
	snap Snapshot
	deps map[string]bool
	nets map[string]bool
}

func NewResolver(s Snapshot) *Resolver {
	return &Resolver{snap: s, deps: map[string]bool{}, nets: map[string]bool{}}
}

// Deps are the slugs referenced; Networks the shared networks to join.
func (rr *Resolver) Deps() []string {
	return slices.Sorted(maps.Keys(rr.deps))
}

func (rr *Resolver) Networks() []string {
	return slices.Sorted(maps.Keys(rr.nets))
}

// Expand substitutes every ref in one string sitting at w. Any failure is an
// error and no value, never a half-expanded string.
func (rr *Resolver) Expand(w Where, val string) (string, error) {
	if !HasRef(val) {
		return val, nil
	}
	var firstErr error
	out := refRe.ReplaceAllStringFunc(val, func(m string) string {
		v, err := rr.one(w, refRe.FindStringSubmatch(m)[1])
		if err != nil && firstErr == nil {
			firstErr = err
		}
		return v
	})
	if firstErr != nil {
		return "", firstErr
	}
	return out, nil
}

func (rr *Resolver) one(w Where, body string) (string, error) {
	r, err := Parse(body)
	if err != nil {
		return "", err
	}
	if !allowed(w, r.Kind) {
		return "", fmt.Errorf("%s: a %s ref is not allowed in %s", r.Source, r.Kind, w)
	}
	return rr.lookup(w, r)
}

func (rr *Resolver) lookup(w Where, r Ref) (string, error) {
	key := r.Slug + "." + r.Name
	param := func(v Value, ok bool, unset string) (string, error) {
		switch {
		case !ok:
			return "", errs.Unset{Param: unset}
		case v.Secret && w == InDomain:
			return "", fmt.Errorf("%s: %s is a secret, and a domain is public", r.Source, key)
		}
		return v.V, nil
	}
	switch r.Kind {
	case KindParam:
		// Env, then stack. Stops at stack: org is a trust boundary.
		if v, ok := rr.snap.EnvParams[key]; ok {
			return param(v, ok, key)
		}
		v, ok := rr.snap.StackParams[key]
		return param(v, ok, key)
	case KindOrgParam:
		v, ok := rr.snap.OrgParams[key]
		return param(v, ok, "org."+key)
	case KindStackr:
		if v := rr.snap.Stackr[r.Name]; v != "" {
			return v, nil
		}
		return "", fmt.Errorf("%s: not known yet; the proxy records it when it attaches to this environment's network", r.Source)
	case KindEnv:
		if rr.snap.Env == "" {
			return "", fmt.Errorf("%s: no environment here", r.Source)
		}
		return rr.snap.Env, nil
	case KindBackup:
		if v, ok := rr.snap.Backups[r.Name]; ok {
			return v, nil
		}
		return "", fmt.Errorf("%s: this org has no backup destination named %q", r.Source, r.Name)
	case KindSelf:
		if v, ok := rr.snap.Self.Outputs[r.Name]; ok {
			return v, nil
		}
		return "", fmt.Errorf("%s: this tile publishes no output named %q", r.Source, r.Name)
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
	// Referencing is not access.
	if src.Managed && !src.Attached {
		return "", fmt.Errorf("%s: %s is not attached to this tile; attach it first", r.Source, r.Slug)
	}
	v, ok := src.Outputs[r.Name]
	if !ok {
		return "", fmt.Errorf("%s: %s publishes no output named %q", r.Source, r.Slug, r.Name)
	}
	rr.deps[r.Slug] = true
	// Blank: the shared source never deployed; joining "" would fail on the
	// network instead of on the dependency.
	if src.Network != "" {
		rr.nets[src.Network] = true
	}
	return v, nil
}

// Endpoint is a service tile's address as stackr knows it.
type Endpoint struct {
	Alias    string // network alias (the slug today), resolves on shared networks too
	Port     int
	Protocol string // http | https | tcp
	Domains  []Domain
}

type Domain struct {
	Host     string
	HTTPS    bool
	Redirect bool
}

// Outputs is the built-in set a service tile publishes. An absent name is the
// contract: tcp has no url; public_* is absent until a non-redirect domain
// exists, so a ref to it fails the deploy instead of resolving to "".
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
	if proto == "http" || proto == "https" {
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
		out["public_url"], out["public_domain"] = scheme+"://"+d.Host, d.Host
		break
	}
	return out
}
