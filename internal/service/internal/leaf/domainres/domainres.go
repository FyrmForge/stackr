// Package domainres owns domain_resources: the hosts stackr names tiles
// under, at instance, org or stack level (REWRITE.md "Domain resources").
// Facts from other tables (the orgs for the squat check, the stack's org,
// how many tile domains a resource names) come in as arguments.
package domainres

import (
	"cmp"
	"context"
	"net/mail"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/slug"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// The levels, nearest first.
const (
	Stack    = "stack"
	Org      = "org"
	Instance = "instance"
)

// Spec is a resource someone asks for. OwnerID is the org or stack id; ""
// at instance level.
type Spec struct {
	Level               string
	OwnerID             string
	Host                string
	IncludeEnvOnDefault bool
	ACMEEmail           string
}

type Leaf struct{ rows store.DomainResourceStore }

func New(rows store.DomainResourceStore) *Leaf {
	return &Leaf{rows: rows}
}

func (l *Leaf) Get(ctx context.Context, id string) (store.DomainResource, error) {
	return l.rows.Get(ctx, id)
}

// ListAll is every resource on the server. What a caller may see is the
// surface's job.
func (l *Leaf) ListAll(ctx context.Context) ([]store.DomainResource, error) {
	return l.rows.List(ctx)
}

// Create adds a declared resource: someone asked for it. ownOrgID is the
// org the host counts against for the squat check (the owner at org level,
// the stack's org at stack level, "" at instance level); orgs is every org.
// A taken host is the unique index's answer, not a pre-check.
func (l *Leaf) Create(ctx context.Context, s Spec, ownOrgID string, orgs []store.Org) (store.DomainResource, error) {
	r := store.DomainResource{
		ID:                  uuid.NewString(),
		Level:               s.Level,
		IncludeEnvOnDefault: s.IncludeEnvOnDefault,
		ACMEEmail:           strings.ToLower(strings.TrimSpace(s.ACMEEmail)),
		Declared:            true,
		CreatedAt:           time.Now().UTC(),
	}
	switch s.Level {
	case Instance:
	case Org:
		r.OrgID = &s.OwnerID
	case Stack:
		r.StackID = &s.OwnerID
	default:
		return r, errs.Invalidf("level", "A domain resource is instance, org or stack level.")
	}
	if s.Level != Instance && s.OwnerID == "" {
		return r, errs.Invalidf("owner", "Say which %s this domain belongs to.", s.Level)
	}
	host, err := checkHost(s.Host)
	if err != nil {
		return r, err
	}
	r.Host = host
	if err := checkEmail(r.ACMEEmail); err != nil {
		return r, err
	}
	if err := CheckOrgSquat(host, ownOrgID, orgs); err != nil {
		return r, err
	}
	err = l.rows.Create(ctx, r)
	_, taken := errs.IsConflict(err)
	if taken {
		return r, errs.Conflictf("%s is already a domain resource.", host)
	}
	return r, err
}

// SetACME changes the account the resource's certificates are issued on.
// "" puts them back on the instance account.
func (l *Leaf) SetACME(ctx context.Context, r store.DomainResource, email string) (store.DomainResource, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if err := checkEmail(email); err != nil {
		return r, err
	}
	r.ACMEEmail = email
	return r, l.rows.Update(ctx, r)
}

// Delete removes the resource. named is how many tile domains carry its id;
// the caller counts them. The foreign key is the backstop.
func (l *Leaf) Delete(ctx context.Context, r store.DomainResource, named int) error {
	switch {
	case named == 1:
		return errs.Conflictf("1 tile domain is named by this resource.")
	case named > 1:
		return errs.Conflictf("%d tile domains are named by this resource.", named)
	}
	return l.rows.Delete(ctx, r.ID)
}

// Visible is what a stack in org orgID may name tiles under, nearest first:
// its own rows, its org's, the instance's; declared before undeclared, then
// oldest first.
// ponytail: scans every row on the server; a WHERE on the three owners when
// resources run to thousands.
func (l *Leaf) Visible(ctx context.Context, stackID, orgID string) ([]store.DomainResource, error) {
	all, err := l.rows.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []store.DomainResource
	for _, r := range all {
		if owns(r.StackID, stackID) || owns(r.OrgID, orgID) || r.Level == Instance {
			out = append(out, r)
		}
	}
	slices.SortStableFunc(out, func(a, b store.DomainResource) int {
		return cmp.Or(
			cmp.Compare(rank(a), rank(b)),
			a.CreatedAt.Compare(b.CreatedAt),
			strings.Compare(a.Host, b.Host),
		)
	})
	return out, nil
}

func owns(owner *string, id string) bool {
	return owner != nil && *owner == id
}

func rank(r store.DomainResource) int {
	n := 2 // instance
	switch r.Level {
	case Stack:
		n = 0
	case Org:
		n = 1
	}
	n *= 2
	if !r.Declared {
		n++
	}
	return n
}

// AutoHost is the name a tile gets under res: tile[.env].stack.org.<instance
// host>, tile[.env].stack.<org host> or tile[.env].<stack host>. The env
// label is dropped on the default env (the ladder's top rung, DECIDE 192;
// the caller knows which) unless the resource says include_env_on_default.
func AutoHost(res store.DomainResource, orgSlug, stackSlug, envSlug, tileSlug string, isDefaultEnv bool) string {
	segs := []string{tileSlug}
	if !isDefaultEnv || res.IncludeEnvOnDefault {
		segs = append(segs, envSlug)
	}
	switch res.Level {
	case Org:
		segs = append(segs, stackSlug)
	case Instance:
		segs = append(segs, stackSlug, orgSlug)
	}
	segs = append(segs, res.Host)
	return strings.Join(segs, ".")
}

// CheckOrgSquat refuses a host whose first label is another org's slug:
// generated names nest under an org's slug, so orgb.example.com held by org
// A would shadow org B's. ownOrgID never counts against itself, nor do its
// stacks (the caller passes the stack's org); "" at instance level means
// every org is someone else's. The reverse check is leaf/org's Rename, on
// the same label (slug.OfHost).
func CheckOrgSquat(host, ownOrgID string, orgs []store.Org) error {
	label := slug.OfHost(host)
	if label == "" {
		return nil
	}
	squats := slices.ContainsFunc(orgs, func(o store.Org) bool {
		return o.Slug == label && o.ID != ownOrgID
	})
	if squats {
		return errs.Conflictf("That hostname starts with another organization's slug.")
	}
	return nil
}

// SeedInstance makes the instance row from the installer's root domain,
// once: nothing when root is empty or an instance row exists, whatever its
// host. The row is undeclared: stackr made it, not a person.
func (l *Leaf) SeedInstance(ctx context.Context, root string) error {
	if strings.TrimSpace(root) == "" {
		return nil
	}
	all, err := l.rows.List(ctx)
	if err != nil {
		return err
	}
	hasInstance := slices.ContainsFunc(all, func(r store.DomainResource) bool {
		return r.Level == Instance
	})
	if hasInstance {
		return nil
	}
	host, err := installspec.CheckRoot(root)
	if err != nil {
		return err
	}
	return l.rows.Create(ctx, store.DomainResource{
		ID:        uuid.NewString(),
		Level:     Instance,
		Host:      strings.TrimPrefix(host, "*."),
		CreatedAt: time.Now().UTC(),
	})
}

// checkHost is v0's resource grammar: a bare hostname (no scheme, slash,
// port or space) the installer would take as a root, lower-cased. No "*.":
// tiles get names under the host, and a name under a wildcard is no name.
func checkHost(host string) (string, error) {
	host = strings.ToLower(strings.TrimSpace(host))
	switch {
	case host == "":
		return "", errs.Invalidf("host", "Give the domain a host.")
	case strings.ContainsAny(host, "/: "):
		return "", errs.Invalidf("host", "Give a bare hostname: no scheme, slash or port.")
	case strings.HasPrefix(host, "*."):
		return "", errs.Invalidf("host", "Give the host without *.; tiles get names under it.")
	}
	clean, err := installspec.CheckRoot(host)
	if err != nil {
		return "", errs.Invalidf("host", "%s", err.Error())
	}
	return clean, nil
}

// checkEmail takes "" (the instance account) or one bare address.
func checkEmail(email string) error {
	if email == "" {
		return nil
	}
	a, err := mail.ParseAddress(email)
	if err != nil || a.Address != email {
		return errs.Invalidf("acme_email", "%s is not an email address.", email)
	}
	return nil
}
