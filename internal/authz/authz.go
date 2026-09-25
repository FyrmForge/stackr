// Package authz decides who may do what: facts in, verdict out. It never
// touches the store; the middleware loads the principal and resolves the
// resource once per request and calls Can. Handlers never re-decide, leaves
// and flows know nothing about auth.
package authz

import "github.com/FyrmForge/stackr/internal/service/errs"

// Verb names one operation, "area.action".
type Verb string

// Level is the ladder read < write < owner < admin; a check is a comparison.
type Level int

const (
	LevelRead Level = iota
	LevelWrite
	LevelOwner
	LevelAdmin
)

// verbLevels is the one table. v1 has two roles (org owner, stackr admin),
// so only the owner/admin split bites today; the ladder is kept as the
// record of which contested verbs were decided which way. Where the old
// panel and API disagreed, the higher level is the entry.
var verbLevels = map[Verb]Level{
	"org.read":            LevelRead,
	"member.list":         LevelRead,
	"registry.image.list": LevelRead,
	"org.config.export":   LevelRead,
	"deployment.read":     LevelRead,
	"tile.read":           LevelRead,

	"stack.create":      LevelWrite,
	"stack.write":       LevelWrite,
	"env.write":         LevelWrite,
	"tile.write":        LevelWrite,
	"variable.write":    LevelWrite,
	"domain.write":      LevelWrite,
	"backup.write":      LevelWrite,
	"destination.write": LevelWrite,
	"sharelink.mint":    LevelWrite,
	"stackplan.plan":    LevelWrite,
	"stackplan.approve": LevelWrite,
	"org.graph.write":   LevelWrite,
	"connector.write":   LevelWrite,
	"deployment.cancel": LevelWrite,

	"org.owner.read":            LevelOwner,
	"org.write":                 LevelOwner,
	"member.manage":             LevelOwner,
	"org.config.bind":           LevelOwner,
	"orgdefaults.set":           LevelOwner,
	"orgplan.approve":           LevelOwner,
	"registry.credential.write": LevelOwner,
	"registry.tag.delete":       LevelOwner,
	"domain.resource":           LevelOwner,
	"managed.allow":             LevelOwner, // who outside the env may cut slices
	"org.setup.step":            LevelOwner,
	"org.setup.domain":          LevelOwner,
	"org.setup.connector":       LevelOwner,

	"admin.read":         LevelAdmin,
	"serverdefaults.set": LevelAdmin,
	"node.manage":        LevelAdmin,
	"container.admin":    LevelAdmin,
	"proxy.admin":        LevelAdmin,
	"user.admin":         LevelAdmin,
	"org.create":         LevelAdmin,
}

// User is the request's principal, loaded once per request from live rows:
// a key carries no rights of its own, so a demotion is felt at the next
// request.
type User struct {
	ID     string
	Admin  bool              // stackr admin: owner in every org
	Active bool              // a disabled account may do nothing
	Roles  map[string]string // org id -> role
	Key    bool              // authenticated by an API key
	KeyOrg string            // the org the key is bound to, "" = unbound
}

// Resource is what the URL resolved to. Only the org matters in v1.
type Resource struct{ OrgID string }

// Can is the only place in the product that decides this. errs.ErrNotFound
// for outside the org (a 403 would confirm the id exists), errs.ErrRefused
// for inside the org below the level.
func Can(u User, v Verb, r Resource) error {
	if err := Self(u); err != nil {
		return err
	}
	need := LevelOf(v)
	if need == LevelAdmin {
		if !u.Admin {
			return errs.ErrRefused
		}
		return nil
	}
	// B36: a key is bound to the org it was minted in and does not travel.
	if u.KeyOrg != "" && u.KeyOrg != r.OrgID {
		return errs.ErrNotFound
	}
	have, member := u.level(r.OrgID)
	if !member {
		return errs.ErrNotFound
	}
	if have < need {
		return errs.ErrRefused
	}
	return nil
}

// Self gates the routes about the caller alone (/me, their org list): a
// live, active account, and an unbound key only for a stackr admin
// (DECIDE 13).
func Self(u User) error {
	if u.ID == "" || !u.Active {
		return errs.ErrRefused
	}
	if u.Key && u.KeyOrg == "" && !u.Admin {
		return errs.ErrRefused
	}
	return nil
}

func (u User) level(orgID string) (Level, bool) {
	if u.Admin {
		return LevelAdmin, true
	}
	return RoleLevel(u.Roles[orgID])
}

// RoleLevel is exported because revocation compares the rung somebody had
// against the one they have now with the same comparison the gate makes.
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

// LevelOf: an unregistered verb needs admin, so a verb that forgets to name
// itself fails closed.
func LevelOf(v Verb) Level {
	if l, ok := verbLevels[v]; ok {
		return l
	}
	return LevelAdmin
}

// KnownVerb backs the route test: every mutating route names a known verb.
func KnownVerb(v Verb) bool {
	_, ok := verbLevels[v]
	return ok
}

// StandingChanged is one rule for demote, remove and disable. to is "" when
// the membership is gone; orgID is "" when the account was disabled. A drop
// below write closes the user's sessions and keys (B16); scope is the org
// whose keys go, "" for every org.
func StandingChanged(orgID, from, to string) (close bool, scope string) {
	fromLevel, _ := RoleLevel(from)
	toLevel, ok := RoleLevel(to)
	if !ok {
		toLevel = LevelRead
	}
	if orgID != "" && (fromLevel < LevelWrite || toLevel >= LevelWrite) {
		return false, ""
	}
	return true, orgID
}
