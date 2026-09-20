package service

import (
	"context"
	"sort"
	"strings"

	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Access is "may this principal do this verb on this organization", and it is
// the only place in the product that decides it.
//
// Two surfaces each grew their own gate vocabulary — `ownedOrg`,
// `ownedSettingsOrg`, `requireOrgOwner`, `CanWriteOrg`, `requireOrgWrite`,
// `orgMember`, `orgAllowed`, two `adminOnly` closures — and wherever a verb
// appeared on both, the level was chosen twice. Where the two disagreed the
// lower one was never a considered grant, it was a forgotten guard: an org
// registry push credential that the panel made an owner mint could be minted
// by any member over the API, and an org config plan that renames the org and
// creates and deletes stacks could be approved the same way.
//
// So: one ladder, one table, and the rule that where the surfaces disagreed
// the higher level wins.

// Level is the ladder. Higher is stricter, so a check is a comparison.
type Level int

const (
	// LevelRead is any member of the org, including a viewer.
	LevelRead Level = iota
	// LevelWrite is owner or member — the content-write right.
	LevelWrite
	// LevelOwner is the org's owner. Server admins are owner everywhere.
	LevelOwner
	// LevelAdmin is a server admin, for things that are not org content at
	// all: nodes, containers, the proxy, the server's own defaults.
	LevelAdmin
)

func (l Level) String() string {
	switch l {
	case LevelRead:
		return "read"
	case LevelWrite:
		return "write"
	case LevelOwner:
		return "owner"
	case LevelAdmin:
		return "admin"
	}
	return "unknown"
}

// KnownVerb reports whether a verb has a level in the table.
//
// It exists for the route-verb test (point 18's safety net), which walks the
// registered routes and asks whether each mutating one names a verb. An
// unregistered verb needs Admin, which fails closed at request time but tells
// a test nothing — so "is this a verb at all" is a separate question.
func KnownVerb(v Verb) bool {
	_, ok := verbLevels[v]
	return ok
}

// Principal is who is asking, resolved once per request from either surface:
// a browser session or an API key.
type Principal struct {
	// UserID is empty for an unauthenticated request.
	UserID string
	// Admin is the server-admin flag. An admin is owner in every org.
	Admin bool
	// Roles is this user's role per org id ("owner", "member", "viewer").
	// An admin's is empty and never read.
	Roles map[string]string
	// Scopes is an API key's scope list. A browser session carries none, and
	// a principal with no key is not scope-checked: scopes narrow a key below
	// its user's rights, they never widen one.
	Scopes []string
	// Keyed marks a request that arrived with an API key, which is what makes
	// the empty Scopes above unambiguous.
	Keyed bool
}

// Level is this principal's standing in one org.
func (p Principal) Level(orgID string) (Level, bool) {
	if p.UserID == "" {
		return LevelRead, false
	}
	if p.Admin {
		return LevelAdmin, true
	}
	return RoleLevel(p.Roles[orgID])
}

// RoleLevel is the ladder rung an org role sits on. Exported because
// revocation compares the rung somebody had against the one they have now,
// and "did they drop below write" must be the same comparison the gate makes.
func RoleLevel(role string) (Level, bool) {
	switch role {
	case "owner":
		return LevelOwner, true
	case "member":
		return LevelWrite, true
	case "viewer":
		return LevelRead, true
	}
	return LevelRead, false
}

// Verb names one operation. The strings are the wire names used in the gate
// matrix in docs/plans/service-extraction/shards/g-access.md; nothing parses
// them, they are there so a refusal can say what was refused.
type Verb string

// The verb catalogue. One entry per operation both surfaces can reach, plus
// the org-level ones only one surface has today — those are here so that when
// the second surface lands it inherits a level instead of picking one.
const (
	// Reads. Uncontested: every read is any member of the org.
	VerbOrgRead        Verb = "org.read"
	VerbMemberList     Verb = "member.list"
	VerbRegistryList   Verb = "registry.image.list"
	VerbConfigExport   Verb = "org.config.export"
	VerbDeploymentRead Verb = "deployment.read"
	VerbTileRead       Verb = "tile.read"

	// Reads only an org owner may make. One verb rather than one per page:
	// what these have in common is the level, and the pages themselves are
	// unrelated — the invite list (an invite link is a credential), the org's
	// config-as-code plans, the setup wizard, and the wizard's repo picker.
	//
	// Decided when the reads were gated, 2026-09-20: the panel required an
	// owner for all four and the API let any member list an org's plans, the
	// same shape as points 7, 10 and 11, so the owner level wins. It replaces
	// VerbOrgPlanList, which named the read half of that disagreement and was
	// never wired to a route.
	VerbOrgOwnerRead Verb = "org.owner.read"

	// Reads that are not org content at all: the admin area, the server list,
	// the container console. The adminOnly middleware still guards them; this
	// is what puts the level in the one table with everything else.
	VerbAdminRead Verb = "admin.read"

	// Content writes: owner or member.
	VerbStackCreate      Verb = "stack.create"
	VerbStackWrite       Verb = "stack.write"
	VerbEnvWrite         Verb = "env.write"
	VerbTileWrite        Verb = "tile.write"
	VerbVariableWrite    Verb = "variable.write"
	VerbDomainWrite      Verb = "domain.write"
	VerbBackupWrite      Verb = "backup.write"
	VerbDestinationWrite Verb = "destination.write"
	VerbShareLinkMint    Verb = "sharelink.mint"
	VerbStackPlan        Verb = "stackplan.plan"
	// The org canvas: annotations, groups and node positions on the org's
	// shared graph. Write, not owner — that is what the panel has always
	// enforced, and point 18 records levels, it does not change them. Not to
	// be confused with the same edits on the HOME canvas, which write the
	// caller's own layout (repo.ScopeUser) and are personal routes.
	VerbOrgGraphWrite Verb = "org.graph.write"
	// Connecting and disconnecting a git provider on an org. Write on the
	// panel today; the API has no equivalent yet, so it inherits this level
	// instead of picking a second one when it lands.
	VerbConnectorWrite Verb = "connector.write"
	VerbStackPlanApprove Verb = "stackplan.approve"
	// Decided in point 7: the API gated this on read, so a viewer whose key
	// carried apps:deploy could cancel a production deployment.
	VerbDeploymentCancel Verb = "deployment.cancel"

	// Owner.
	VerbOrgWrite      Verb = "org.write"
	VerbMemberManage  Verb = "member.manage"
	// Setting inherited defaults. The route's Kind says at which scope —
	// server, org, stack or environment — and the level is the same at every
	// one of them: resolveSettingsTarget already required an owner for all
	// four. One operation, one verb, scope carried by the tenancy.
	VerbOrgDefaults   Verb = "orgdefaults.set"
	VerbOrgConfigBind Verb = "org.config.bind"
	// Decided in point 11: the API let any member approve, and an org apply
	// renames the org and creates and deletes stacks.
	VerbOrgPlanApprove Verb = "orgplan.approve"
	// Decided in point 10: the panel has always required an owner for both.
	VerbRegistryCredential Verb = "registry.credential.write"
	VerbRegistryTagDelete  Verb = "registry.tag.delete"
	// The wizard. Every other step is owner; these two were member, and the
	// setup allow-list waives the draft gate for the whole prefix, so a member
	// who could not see the wizard could still post its domain and connector
	// steps. A draft org has no members who need them.
	VerbSetupStep      Verb = "org.setup.step"
	VerbSetupDomain    Verb = "org.setup.domain"
	VerbSetupConnector Verb = "org.setup.connector"

	// Server admin: not org content.
	VerbServerDefaults Verb = "serverdefaults.set"
	VerbNodeManage     Verb = "node.manage"
	VerbContainerAdmin Verb = "container.admin"
	VerbProxyAdmin     Verb = "proxy.admin"
	VerbUserAdmin      Verb = "user.admin"
	VerbOrgCreate      Verb = "org.create"
)

// verbLevels is the table. Where the two surfaces disagreed, the higher level
// is the entry, and the comment on the constant says which point decided it.
var verbLevels = map[Verb]Level{
	VerbOrgRead:        LevelRead,
	VerbMemberList:     LevelRead,
	VerbRegistryList:   LevelRead,
	VerbConfigExport:   LevelRead,
	VerbDeploymentRead: LevelRead,
	VerbTileRead:       LevelRead,

	VerbOrgOwnerRead: LevelOwner,
	VerbAdminRead:    LevelAdmin,

	VerbStackCreate:      LevelWrite,
	VerbStackWrite:       LevelWrite,
	VerbEnvWrite:         LevelWrite,
	VerbTileWrite:        LevelWrite,
	VerbVariableWrite:    LevelWrite,
	VerbDomainWrite:      LevelWrite,
	VerbBackupWrite:      LevelWrite,
	VerbDestinationWrite: LevelWrite,
	VerbShareLinkMint:    LevelWrite,
	VerbStackPlan:        LevelWrite,
	VerbOrgGraphWrite:    LevelWrite,
	VerbConnectorWrite:   LevelWrite,
	VerbStackPlanApprove: LevelWrite,
	VerbDeploymentCancel: LevelWrite,

	VerbOrgWrite:           LevelOwner,
	VerbMemberManage:       LevelOwner,
	VerbOrgDefaults:        LevelOwner,
	VerbOrgConfigBind:      LevelOwner,
	VerbOrgPlanApprove:     LevelOwner,
	VerbRegistryCredential: LevelOwner,
	VerbRegistryTagDelete:  LevelOwner,
	VerbSetupStep:          LevelOwner,
	VerbSetupDomain:        LevelOwner,
	VerbSetupConnector:     LevelOwner,

	VerbServerDefaults: LevelAdmin,
	VerbNodeManage:     LevelAdmin,
	VerbContainerAdmin: LevelAdmin,
	VerbProxyAdmin:     LevelAdmin,
	VerbUserAdmin:      LevelAdmin,
	VerbOrgCreate:      LevelAdmin,
}

// LevelOf is the level a verb needs. An unknown verb is LevelAdmin: a new
// operation that forgets to register itself fails closed rather than open.
func LevelOf(v Verb) Level {
	if l, ok := verbLevels[v]; ok {
		return l
	}
	return LevelAdmin
}

// Verbs lists the catalogue, sorted. For tests and for the docs.
func Verbs() []Verb {
	out := make([]Verb, 0, len(verbLevels))
	for v := range verbLevels {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// AccessService answers the one question. It holds the store because a
// principal's roles are rows, and resolving them is its job too.
type AccessService struct{ store repo.Store }

// NewAccessService creates a new access service.
func NewAccessService(store repo.Store) *AccessService { return &AccessService{store: store} }

// Principal resolves who is asking. scopes is an API key's list; pass nil and
// keyed=false for a browser session.
func (s *AccessService) Principal(ctx context.Context, u *repo.User, scopes []string, keyed bool) (Principal, error) {
	if u == nil {
		return Principal{}, nil
	}
	p := Principal{UserID: u.ID, Admin: u.Role == "admin", Scopes: scopes, Keyed: keyed}
	if p.Admin {
		return p, nil
	}
	orgs, err := s.store.ListOrgsForUser(ctx, u.ID)
	if err != nil {
		return Principal{}, err
	}
	p.Roles = make(map[string]string, len(orgs))
	for _, o := range orgs {
		m, err := s.store.GetOrgMember(ctx, o.ID, u.ID)
		if err != nil || m == nil {
			continue
		}
		p.Roles[o.ID] = m.Role
	}
	return p, nil
}

// Require answers "may this principal do this verb in this org".
//
// Not-a-member and not-at-this-level are deliberately two different answers
// only when the caller is already known to be in the org: a principal outside
// the org gets NotFound, because a 403 there confirms the id exists to someone
// who should not know it.
func (s *AccessService) Require(p Principal, v Verb, orgID string) error {
	need := LevelOf(v)
	if need == LevelAdmin {
		if !p.Admin {
			return svcerr.ErrForbidden
		}
		return nil
	}
	have, member := p.Level(orgID)
	if !member {
		return svcerr.ErrNotFound
	}
	if have < need {
		// Bare, like every other ErrForbidden: the level a verb needs is not
		// a secret, but spelling it per refusal is how two surfaces end up
		// with two wordings again. Handlers that have better words for their
		// own screen keep them; this is the decision, not the sentence.
		return svcerr.ErrForbidden
	}
	return nil
}

// RequireScope is the second layer, checked at use time rather than only at
// mint time.
//
// Point 11 left this owed. API key scopes are granted against whichever org
// the minting browser's cookie happened to point at, and keys carry no org of
// their own, so a user who writes in org A and only views org B could mint
// write scopes on A's page and send them at B. The role check above is what
// actually stops that — which is why it has to run on every write and not be
// something a handler can forget. This call runs both, in that order, so a
// route that asks for one gets the other.
func (s *AccessService) RequireScope(p Principal, scope string, v Verb, orgID string) error {
	if err := s.Require(p, v, orgID); err != nil {
		return err
	}
	if scope == "" || !p.Keyed {
		return nil
	}
	for _, have := range p.Scopes {
		if have == scope || strings.EqualFold(have, scope) {
			return nil
		}
	}
	return svcerr.Forbiddenf("api key is missing the %s scope", scope)
}
