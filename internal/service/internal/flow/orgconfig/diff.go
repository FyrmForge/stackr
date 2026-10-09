package orgconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/connector"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domainres"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/org"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/route"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/settings"
	"github.com/FyrmForge/stackr/internal/service/internal/planfile"
	"github.com/FyrmForge/stackr/internal/service/internal/slug"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Change is one line of a plan, planfile's shape. Kind is org, param,
// param-update, defaults, colors, create, rebind, rename, domain,
// domain-update, share, share-update or share-delete; create, domain, param
// and share add, the rest change. Tile names the stack or host the line is
// about.
type Change = planfile.Change

// Plan is what applying the file would do, in the order apply walks it:
// moved: renames, the org, params, defaults, colors, stacks, domains, shares.
// Blockers refuse the apply; Notes do not.
type Plan struct{ planfile.Plan }

func (p *Plan) block(format string, a ...any) {
	p.Blockers = append(p.Blockers, fmt.Sprintf(format, a...))
}

func (p *Plan) add(c Change) { p.Changes = append(p.Changes, c) }

// Summary is v0's one line: "2 to add, 1 to change, 1 blocker". The file
// never deletes: a removal row is one to review, ticked or not at approve.
func (p *Plan) Summary() string {
	return p.Plan.Summary(func(kind string) bool {
		switch kind {
		case "create", "domain", "param", "share":
			return true
		}
		return false
	})
}

// Live is what the file is diffed against, gathered by the orchestrator.
// The org's settings and env colors are read off Org.
type Live struct {
	Org        store.Org
	Orgs       []store.Org             // every org: the rename and squat checks
	Claims     []org.Claim             // every domain on the server and its org: the rename check
	Stacks     []StackLive             // the org's stacks
	Params     map[string]params.Value // the org's own params, keyed collection.name
	Domains    []store.DomainResource  // every domain resource on the server
	Routes     []store.Route           // every external route: a domain resource may not sit on one
	Connectors []store.Connector       // the org's connected connectors
	Shares     []store.Share           // the org's network shares
	ShareUsers map[string][]string     // share slug -> slugs of the tiles whose lines mount it
}

// StackLive is one stack.
type StackLive struct {
	Stack store.Stack
}

// Diff is the plan for f against live. The file creates and updates, never
// deletes: a stack, param or domain it no longer names is left as it is
// (DECIDE 185, 188, 191). Shares: when the file has a shares: block, a share
// it does not name is an unticked removal row, applied only when ticked.
func Diff(f *File, live Live) Plan {
	var p Plan
	stacks := map[string]StackLive{}
	for _, s := range live.Stacks {
		stacks[s.Stack.Slug] = s
	}
	p.moved(f.Moved, stacks)
	p.org(f.Org, live)
	p.params(f.Params, live.Params)
	if f.Defaults != nil {
		have, _ := settings.Parse(live.Org.Settings)
		// Applied from f too, so an omitted secret pair keeps its live value.
		planfile.KeepSecretPair(&f.Defaults.ProtectUser, &f.Defaults.ProtectPassword, have.ProtectUser, have.ProtectPassword)
		if jsonOf(*f.Defaults) != canonSettings(live.Org.Settings) {
			// values never shown: protect_password is a credential
			p.add(Change{
				Kind:  "defaults",
				Field: "defaults",
			})
		}
	}
	if f.EnvColors != nil {
		have := map[string]string{}
		_ = json.Unmarshal([]byte(live.Org.EnvColors), &have)
		if !maps.Equal(have, f.EnvColors) {
			p.add(Change{
				Kind:  "colors",
				Field: "env_colors",
				Old:   jsonOf(have),
				New:   jsonOf(f.EnvColors),
			})
		}
	}
	p.stacks(f.Stacks, live, stacks)
	p.domains(f.Domains, live)
	p.shares(f.Shares, live)
	return p
}

// moved runs first: a rename moves the stack in stacks, so the rest of the
// diff sees it under its new slug (DECIDE 184).
func (p *Plan) moved(moves []Move, stacks map[string]StackLive) {
	for _, m := range moves {
		_, from, _ := strings.Cut(m.From, ".")
		_, to, _ := strings.Cut(m.To, ".")
		_, hasFrom := stacks[from]
		_, hasTo := stacks[to]
		switch {
		case hasFrom && hasTo:
			p.block("moved: %s and %s both exist; delete one by hand first", m.From, m.To)
			continue
		case !hasFrom && !hasTo:
			p.block("moved: neither %s nor %s exists", m.From, m.To)
			continue
		case !hasFrom:
			continue // already moved; the entry may stay
		}
		p.add(Change{
			Kind: "rename",
			Old:  from,
			New:  to,
		})
		s := stacks[from]
		s.Stack.Slug = to
		stacks[to] = s
		delete(stacks, from)
	}
}

// org renames when the name differs. A slug move is blocked when another
// org holds the slug or a foreign domain leads with it (leaf/org.Rename's
// refusals, checked before apply).
func (p *Plan) org(name string, live Live) {
	o := live.Org
	if name == o.Name {
		return
	}
	s := slug.Make(name)
	p.add(Change{
		Kind:  "org",
		Field: "name",
		Old:   o.Name,
		New:   name,
	})
	if s == o.Slug {
		return
	}
	if slices.ContainsFunc(live.Orgs, func(x store.Org) bool { return x.Slug == s && x.ID != o.ID }) {
		p.block("org: another organization already uses the slug %q", s)
	}
	for _, c := range live.Claims {
		if slug.OfHost(c.Host) == s && c.OrgID != o.ID {
			p.block("org: another organization's domain %s leads with %q", c.Host, s)
			break
		}
	}
}

// params creates (param) and updates (param-update), never deletes (DECIDE
// 185); a secret is never turned back into a param. A param's row carries
// its new value; a secret's value is never in the file, so never in a row.
func (p *Plan) params(decls map[string]map[string]Param, have map[string]params.Value) {
	for _, c := range slices.Sorted(maps.Keys(decls)) {
		for _, n := range slices.Sorted(maps.Keys(decls[c])) {
			decl, key := decls[c][n], c+"."+n
			old, ok := have[key]
			switch {
			case decl.Type == params.Param && ok && old.Secret:
				p.block("params.%s is a secret; a secret is never turned back into a param", key)
			case decl.Type == params.Secret && ok && !old.Secret:
				p.add(Change{
					Kind:  "param-update",
					Field: key,
					Note:  "becomes a secret",
				})
			case decl.Type == params.Secret && !ok:
				p.Notes = append(p.Notes, "params."+key+" is declared and not set; tiles that read it wait until it is")
			case decl.Type == params.Param && decl.Value != nil && !ok:
				p.add(Change{
					Kind:  "param",
					Field: key,
					New:   *decl.Value,
				})
			case decl.Type == params.Param && decl.Value != nil && old.V != *decl.Value:
				p.add(Change{
					Kind:  "param-update",
					Field: key,
					New:   *decl.Value,
				})
			}
		}
	}
}

// stacks creates or rebinds each stack the file names; one it does not
// name is left alone, hand-made or file-made (DECIDE 188).
func (p *Plan) stacks(refs map[string]StackRef, live Live, stacks map[string]StackLive) {
	for _, s := range slices.Sorted(maps.Keys(refs)) {
		want := refs[s].Binding(live.Org)
		if want.Repo == "" {
			p.block("stacks.%s: path alone is a file in the org repo, and this org is bound to none", s)
			continue
		}
		cur, ok := stacks[s]
		if !ok {
			p.add(Change{
				Kind: "create",
				Tile: s,
				New:  want.Repo,
				Note: "file " + pathOf(want.Path),
			})
			p.needConnector(s, want, live.Connectors)
			continue
		}
		st := cur.Stack
		have := Binding{
			ConnectorID: st.ConfigConnectorID,
			Repo:        st.ConfigRepo,
			Branch:      st.ConfigBranch,
			Path:        st.ConfigPath,
		}
		// ponytail: repos compare as stored; a .git suffix or a case change
		// reads as a rebind once, and the rebind stores the file's spelling.
		diffs := []Change{
			rebind(s, "repo", have.Repo, want.Repo),
			rebind(s, "branch", have.Branch, want.Branch),
			rebind(s, "path", pathOf(have.Path), pathOf(want.Path)),
			rebind(s, "connector", connectorOf(have, live.Connectors), connectorOf(want, live.Connectors)),
		}
		diffs = slices.DeleteFunc(diffs, func(c Change) bool { return c.Old == c.New })
		if len(diffs) > 0 {
			p.Changes = append(p.Changes, diffs...)
			p.needConnector(s, want, live.Connectors)
		}
	}
}

func rebind(s, field, from, to string) Change {
	return Change{
		Kind:  "rebind",
		Tile:  s,
		Field: field,
		Old:   from,
		New:   to,
	}
}

// needConnector blocks a binding nothing can clone: the named connector is
// not one the org can use, the repo's host has none, or several shared ones
// serve it.
func (p *Plan) needConnector(s string, b Binding, conns []store.Connector) {
	_, err := connector.Resolve(conns, b.ConnectorID, connector.Host(b.Repo))
	switch {
	case err == nil:
	case errors.As(err, &connector.Ambiguous{}):
		p.block("stacks.%s: %v", s, err)
	case b.ConnectorID != "":
		p.block("stacks.%s: connector %s is not one of this org's connected connectors", s, b.ConnectorID)
	default:
		p.block("stacks.%s: no connected connector for %s; connect one first", s, b.Repo)
	}
}

// connectorOf is the connector a clone of b uses (connector.Resolve's rule);
// "" when there is none or the choice is ambiguous.
func connectorOf(b Binding, conns []store.Connector) string {
	c, err := connector.Resolve(conns, b.ConnectorID, connector.Host(b.Repo))
	if err != nil {
		return ""
	}
	return c.ID
}

func pathOf(path string) string {
	if path == "" {
		return stackFilePath
	}
	return path
}

// domains creates or updates the org's domain resources (DECIDE 191); one
// the file no longer names is left alone. Promote's planResources, at org
// level.
func (p *Plan) domains(rs []Reservation, live Live) {
	for _, r := range rs {
		spec := domainres.Spec{
			Level:               domainres.Org,
			OwnerID:             live.Org.ID,
			Host:                r.Host,
			IncludeEnvOnDefault: r.IncludeEnvOnDefault,
			ACMEEmail:           r.ACMEEmail,
		}
		row, err := domainres.Prepare(spec, live.Org.ID, live.Orgs)
		if err != nil {
			p.block("domains: %s: %v", r.Host, err)
			continue
		}
		i := slices.IndexFunc(live.Domains, func(d store.DomainResource) bool { return d.Host == row.Host })
		if i < 0 {
			if route.Holds(live.Routes, row.Host) {
				p.block("domains: %s is an external route", row.Host)
				continue
			}
			p.add(Change{
				Kind: "domain",
				New:  row.Host,
			})
			continue
		}
		have := live.Domains[i]
		if have.OrgID == nil || *have.OrgID != live.Org.ID {
			p.block("domains: %s is already a domain resource", row.Host)
			continue
		}
		if have.IncludeEnvOnDefault != row.IncludeEnvOnDefault {
			p.add(Change{
				Kind:  "domain-update",
				Tile:  row.Host,
				Field: "include_env_on_default",
				Old:   strconv.FormatBool(have.IncludeEnvOnDefault),
				New:   strconv.FormatBool(row.IncludeEnvOnDefault),
			})
		}
		if have.ACMEEmail != row.ACMEEmail {
			p.add(Change{
				Kind:  "domain-update",
				Tile:  row.Host,
				Field: "acme_email",
				Old:   have.ACMEEmail,
				New:   row.ACMEEmail,
			})
		}
	}
}

// canonSettings folds a stored settings blob to the form jsonOf writes.
func canonSettings(blob string) string {
	s, err := settings.Parse(blob)
	if err != nil {
		return blob
	}
	return jsonOf(Defaults(s))
}

func jsonOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// shares creates, updates and, when the file has a shares: block, offers to
// delete the org's network shares: a share the block no longer names is an
// unticked removal row. A share a tile line mounts gets a note, not a row:
// the verb would refuse it, and a blocker would make the plan unapprovable.
func (p *Plan) shares(decls map[string]Share, live Live) {
	if decls == nil {
		return
	}
	have := map[string]store.Share{}
	for _, s := range live.Shares {
		have[s.Slug] = s
	}
	for _, sl := range slices.Sorted(maps.Keys(decls)) {
		d := decls[sl]
		cur, ok := have[sl]
		if !ok {
			p.add(Change{
				Kind: "share",
				Tile: sl,
				New:  d.Source,
				Note: d.Kind,
			})
			continue
		}
		for _, f := range []struct{ name, old, new string }{
			{"kind", cur.Kind, d.Kind},
			{"source", cur.Source, d.Source},
			{"options", cur.Options, d.Options},
			{"user", cur.User, d.User},
			{"password", cur.PasswordRef, d.Password},
		} {
			if f.old != f.new {
				p.add(Change{
					Kind:  "share-update",
					Tile:  sl,
					Field: f.name,
					Old:   f.old,
					New:   f.new,
				})
			}
		}
	}
	for _, s := range live.Shares {
		if _, ok := decls[s.Slug]; ok {
			continue
		}
		if by := live.ShareUsers[s.Slug]; len(by) > 0 {
			// no row: a removal the verb would refuse must not make the plan unapprovable
			p.Notes = append(p.Notes, fmt.Sprintf("shares.%s stays: %s still mount it; drop the lines first", s.Slug, strings.Join(by, ", ")))
			continue
		}
		p.add(Change{
			Kind:     "share-delete",
			Tile:     s.Slug,
			Old:      s.Source,
			Optional: true,
			Key:      "share:" + s.Slug,
		})
	}
}
