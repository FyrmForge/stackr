package service

import (
	"context"
	"errors"
	"strings"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Tenancy — the one read authorization makes.
//
// Point 18 moves the permission check onto the route, which means it runs
// before the handler has loaded anything. To ask "may this principal do this
// verb in this org" the check first has to answer "which org is this route
// about", and today that answer is re-derived inside each of the ~35 gate
// helpers: the panel walks tile -> stack -> org in one shape, the API walks it
// in another, and a backup reaches its org through its tile in a third.
//
// One resolver instead. Kind says what the route addresses; TenancyOf walks it
// up to the org that owns it.

// Kind names what a route addresses. It is declared per route beside the verb
// (see the tenancy table in the panel's and the API's route registration),
// because a bare ":id" does not say whether it is a tile or a stack.
type Kind string

const (
	KindOrg            Kind = "org"
	KindStack          Kind = "stack"
	KindEnv            Kind = "env"
	KindTile           Kind = "tile"
	KindBackup         Kind = "backup"
	KindDeployment     Kind = "deployment"
	KindStackPlan      Kind = "stackplan"
	KindOrgPlan        Kind = "orgplan"
	KindDestination    Kind = "destination"
	KindDomain         Kind = "domain"
	KindDomainResource Kind = "domainresource"
	KindProvision      Kind = "provision"
	KindStorage        Kind = "storage"
	KindConnector      Kind = "connector"
	// KindNone is a route that is not org content at all: a node, a
	// container, the proxy, the server's own defaults. These carry an
	// admin-level verb, and AccessService.Require never looks at the org for
	// one, so resolving a tenancy for them would be inventing one.
	KindNone Kind = "none"
	// KindDeferred is the one bucket where the check stays in the handler,
	// because the org is not addressable before the handler runs: it arrives
	// in the request body, or it is what the request is asking to resolve.
	//
	// Three routes, and they are the whole list:
	//   POST /projects            the panel's stack create (org from the form)
	//   POST /api/v1/stacks       the API's, via orgForCreate (org_id in body)
	//   POST /api/v1/resolve      a lookup; the tenancy is its ANSWER
	//
	// The middleware still requires authentication and still reads the verb;
	// what it cannot do is resolve a tenancy that does not exist yet. Adding
	// a fourth route here is a decision, not a default — everything else has
	// its org in the path.
	KindDeferred Kind = "deferred"
)

// TenancyOf resolves the org that owns whatever ref addresses.
//
// ref is an id or a slug path (`acme`, `acme:shop`, `acme:shop:prod`,
// `acme:shop:prod:api`). Id first, path only when that misses — an id is
// still an id, which is the rule slugpath.go already established for the API.
//
// A KindNone route, and a resource that is server-wide rather than an org's
// (an unshared backup destination, a shared registry), resolve to "". That is
// not a failure: it is the answer, and the verb on such a route is
// admin-level, which Require answers without an org.
//
// Not found is ("", nil), not an error. The caller turns that into the
// refusal its surface uses — 404 in the panel, 403 in the API — and the two
// are deliberately different (see 06-points-18-20.md, "Surface error policy").
// ErrServerOwned marks a row that exists and is owned by the installation
// rather than by an org: the panel's own backup is the only one today. The
// gates translate it into "admin only", which is what a resource with no
// tenancy can mean and all it can mean.
var ErrServerOwned = errors.New("resource belongs to the server, not an org")

func (s *AccessService) TenancyOf(ctx context.Context, kind Kind, ref string) (string, error) {
	if ref == "" || kind == KindNone || kind == KindDeferred {
		return "", nil
	}
	switch kind {
	case KindOrg:
		if o, err := s.store.GetOrg(ctx, ref); err != nil || o != nil {
			return orgID(o), err
		}
		o, err := s.store.GetOrgBySlug(ctx, ref)
		return orgID(o), err

	case KindStack:
		st, err := s.store.GetStack(ctx, ref)
		if err != nil {
			return "", err
		}
		if st == nil {
			if st, err = s.stackByPath(ctx, ref); err != nil || st == nil {
				return "", err
			}
		}
		return st.OrgID, nil

	case KindEnv:
		e, err := s.store.GetEnvironment(ctx, ref)
		if err != nil {
			return "", err
		}
		if e == nil {
			if e, err = s.envByPath(ctx, ref); err != nil || e == nil {
				return "", err
			}
		}
		return s.TenancyOf(ctx, KindStack, e.StackID)

	case KindTile:
		t, err := s.store.GetTile(ctx, ref)
		if err != nil {
			return "", err
		}
		if t == nil {
			if t, err = s.tileByPath(ctx, ref); err != nil || t == nil {
				return "", err
			}
		}
		return s.TenancyOf(ctx, KindStack, t.StackID)

	case KindBackup:
		b, err := s.store.GetBackup(ctx, ref)
		if err != nil || b == nil {
			return "", err
		}
		// The panel's own database backup hangs off no tile and so belongs to
		// no org. Answering "" for it would be indistinguishable from "no such
		// backup", and the gate would 404 the admins who are the only people
		// it was ever for — which is exactly what happened when the mutating
		// backup routes were gated. It is a different answer, so it gets one.
		if !b.TileID.Valid || b.TileID.String == "" {
			return "", ErrServerOwned
		}
		return s.TenancyOf(ctx, KindTile, b.TileID.String)

	case KindDeployment:
		d, err := s.store.GetDeployment(ctx, ref)
		if err != nil || d == nil {
			return "", err
		}
		return s.TenancyOf(ctx, KindTile, d.TileID)

	case KindDomain:
		d, err := s.store.GetDomain(ctx, ref)
		if err != nil || d == nil {
			return "", err
		}
		return s.TenancyOf(ctx, KindTile, d.TileID)

	case KindOrgPlan:
		// An org plan's org id rides in ConfigPlan.StackID — the column is
		// shared with stack plans and the name is the stack table's, not this
		// row's meaning. Copied from requireOrgPlan (api/v1/orgconfig.go),
		// which is the only place that knew it.
		p, err := s.store.GetOrgConfigPlan(ctx, ref)
		if err != nil || p == nil {
			return "", err
		}
		return p.StackID, nil

	case KindStackPlan:
		p, err := s.store.GetConfigPlan(ctx, ref)
		if err != nil || p == nil {
			return "", err
		}
		return s.TenancyOf(ctx, KindStack, p.StackID)

	case KindProvision:
		p, err := s.store.GetProvision(ctx, ref)
		if err != nil || p == nil {
			return "", err
		}
		return s.TenancyOf(ctx, KindEnv, p.EnvID)

	case KindDestination:
		d, err := s.store.GetBackupDestination(ctx, ref)
		if err != nil || d == nil {
			return "", err
		}
		return d.OrgID.String, nil // NULL = server-wide, admin's

	case KindStorage:
		st, err := s.store.GetStorage(ctx, ref)
		if err != nil || st == nil {
			return "", err
		}
		return st.OrgID, nil // "" = a node's own volume, admin's

	case KindConnector:
		cn, err := s.store.GetConnector(ctx, ref)
		if err != nil || cn == nil {
			return "", err
		}
		return cn.OrgID, nil

	case KindDomainResource:
		// The only kind that carries its own owner kind in a column.
		// ponytail: listed and scanned, because the store has no getter and
		// this table holds one row per distinct domain a server answers on.
		// Add GetDomainResource if it ever grows past that.
		rs, err := s.store.ListDomainResources(ctx)
		if err != nil {
			return "", err
		}
		for i := range rs {
			if rs[i].ID != ref {
				continue
			}
			switch rs[i].Level {
			case "org":
				return rs[i].OwnerID, nil
			case "stack":
				return s.TenancyOf(ctx, KindStack, rs[i].OwnerID)
			}
			return "", nil // instance level: the server's, not an org's
		}
		return "", nil
	}
	return "", nil
}

func orgID(o *repo.Org) string {
	if o == nil {
		return ""
	}
	return o.ID
}

// Slug paths. Moved off the API (slugpath.go) so both surfaces resolve a
// reference the same way: the panel's routes address by id today, but the
// check now runs in middleware for both, and two resolvers is how the two
// surfaces disagreed about everything else.
//
// A bare stack, env or tile slug is not accepted. Slugs repeat across orgs,
// so a one-segment reference below org level would have to guess a tenant,
// and guessing is how one org's script quietly edits another's stack.
func pathParts(ref string, want int) ([]string, bool) {
	parts := strings.FieldsFunc(ref, func(r rune) bool { return r == ':' || r == '/' })
	if len(parts) != want {
		return nil, false
	}
	for _, p := range parts {
		if p == "" {
			return nil, false
		}
	}
	return parts, true
}

func (s *AccessService) stackByPath(ctx context.Context, ref string) (*repo.Stack, error) {
	parts, ok := pathParts(ref, 2)
	if !ok {
		return nil, nil
	}
	org, err := s.store.GetOrgBySlug(ctx, parts[0])
	if err != nil || org == nil {
		return nil, err
	}
	return s.store.GetStackBySlug(ctx, org.ID, parts[1])
}

func (s *AccessService) envByPath(ctx context.Context, ref string) (*repo.Environment, error) {
	parts, ok := pathParts(ref, 3)
	if !ok {
		return nil, nil
	}
	st, err := s.stackByPath(ctx, parts[0]+":"+parts[1])
	if err != nil || st == nil {
		return nil, err
	}
	return s.store.GetEnvironmentBySlug(ctx, st.ID, parts[2])
}

func (s *AccessService) tileByPath(ctx context.Context, ref string) (*repo.Tile, error) {
	parts, ok := pathParts(ref, 4)
	if !ok {
		return nil, nil
	}
	env, err := s.envByPath(ctx, strings.Join(parts[:3], ":"))
	if err != nil || env == nil {
		return nil, err
	}
	return s.store.GetTileBySlug(ctx, env.ID, parts[3])
}

// Resolvers: load the thing a route addresses, by id or by slug path.
//
// These carry NO authorization. That is the point of them: once the route's
// verb and Kind are checked in middleware, a handler needs the row and
// nothing else, and the gate helpers it used to call returned the row as a
// side effect of checking. Splitting the two is what lets the check be
// deleted from the body without deleting the load with it.
//
// They live here rather than on each surface because the id-then-path rule
// and the refusal to resolve a tenantless slug are the same rule TenancyOf
// applies, and two copies of it is how the surfaces drifted before.

func (s *AccessService) ResolveOrg(ctx context.Context, ref string) (*repo.Org, error) {
	o, err := s.store.GetOrg(ctx, ref)
	if err != nil || o != nil {
		return o, err
	}
	return s.store.GetOrgBySlug(ctx, ref)
}

func (s *AccessService) ResolveStack(ctx context.Context, ref string) (*repo.Stack, error) {
	st, err := s.store.GetStack(ctx, ref)
	if err != nil || st != nil {
		return st, err
	}
	return s.stackByPath(ctx, ref)
}

func (s *AccessService) ResolveEnv(ctx context.Context, ref string) (*repo.Environment, error) {
	e, err := s.store.GetEnvironment(ctx, ref)
	if err != nil || e != nil {
		return e, err
	}
	return s.envByPath(ctx, ref)
}

func (s *AccessService) ResolveTile(ctx context.Context, ref string) (*repo.Tile, error) {
	t, err := s.store.GetTile(ctx, ref)
	if err != nil || t != nil {
		return t, err
	}
	return s.tileByPath(ctx, ref)
}
