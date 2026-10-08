package serverconfig

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domainres"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/org"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/route"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/settings"
	"github.com/FyrmForge/stackr/internal/service/internal/slug"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Live is what the file is diffed against, gathered by the orchestrator.
// Every field is a fact read from the store; nothing here is a decision.
type Live struct {
	// Settings is the effective value of every Flat knob that is not
	// ConfigOnly (row, then boot value, then catalogue default), by key.
	// panel_backup_dest holds the destination's id.
	Settings map[string]string
	Defaults settings.Settings // the server rung of the cascade
	Tiles    int               // tiles a cascade change redeploys

	Params map[string]params.Value // server scope, secrets decrypted, keyed collection.name

	Routes        []store.Route
	Dests         []store.BackupDest // global only (OrgID nil), local included, keys filled
	DestSchedules map[string]int     // dest id -> volume schedules that name it

	Domains     []store.DomainResource // every resource on the server; the file owns the instance ones
	DomainHosts map[string]int         // resource id -> tile domains named under it
	Taken       []string               // every host the tile domains and domain resources hold

	Connectors []ConnectorLive // server connectors (org_id NULL)
	Orgs       []store.Org     // every org
	Claims     []org.Claim     // every domain on the server and its org: the new-org slug check

	TLSOff bool // STACKR_TLS=off: pass-through routes need TLS
}

// ConnectorLive is a server connector with the orgs it is shared with.
type ConnectorLive struct {
	Connector store.Connector
	OrgIDs    []string // shared with these orgs (ShareAll aside)
	Bound     []string // orgs with a binding (org or stack) that names it: its share to them cannot be revoked
}

// Diff is the plan for f against live, in the order apply walks it: params,
// connector shares, settings and the cascade rung, backup dests, domains,
// routes, orgs, then the removal rows. The file never deletes on its own:
// what a present block no longer names is an unticked removal row, and a
// removal the verb would refuse (a dest a schedule uses, a resource tiles
// sit under) gets a note instead of a row, so a blocker never makes the plan
// unapprovable. Orgs and params are never removal rows.
func Diff(f *File, live Live) Plan {
	d := &differ{f: f, live: live}
	d.dnsProvider = d.final("dns_provider")
	d.panelDomain = d.final("panel_domain")
	d.panelDest = d.finalPanelDest()
	d.destRefs()
	d.params()
	d.connectors()
	d.settings()
	d.defaults()
	d.dests()
	d.domains()
	d.routes()
	d.orgs()
	d.p.Changes = append(d.p.Changes, d.removals...)
	return d.p
}

type differ struct {
	p        Plan
	f        *File
	live     Live
	removals []Change

	dnsProvider string // after the file
	panelDomain string
	panelDest   string // name; "" = local

	waits   map[string]bool // file dests that wait for a server param that is not set
	checked map[string]bool // rename targets already checked
}

func (d *differ) add(c Change) { d.p.Add(c) }

func (d *differ) remove(c Change) {
	c.Optional = true
	d.removals = append(d.removals, c)
}

func (d *differ) note(format string, a ...any) {
	d.p.Notes = append(d.p.Notes, fmt.Sprintf(format, a...))
}

func (d *differ) block(format string, a ...any) { d.p.Block(format, a...) }

// ---- facts ----

func (l Live) eff(key string) string {
	if v, ok := l.Settings[key]; ok {
		return v
	}
	return settings.Default(key)
}

// final is a knob's value after the file applies: the file's when it names
// the key, else what is live. ponytail: a reset ("") to a value the boot env
// supplies reads as the default here and re-plans until the row is gone.
func (d *differ) final(key string) string {
	if v, ok := d.f.setting(key); ok {
		if v == "" {
			return settings.Default(key)
		}
		return v
	}
	return d.live.eff(key)
}

func (d *differ) destByName(name string) (store.BackupDest, bool) {
	i := slices.IndexFunc(d.live.Dests, func(x store.BackupDest) bool { return x.Name == name && x.Kind == backup.S3 })
	if i < 0 {
		return store.BackupDest{}, false
	}
	return d.live.Dests[i], true
}

// destName is the file's spelling of panel_backup_dest: the destination's
// name; "" for local, unset or a destination that is gone.
func (d *differ) destName(id string) string {
	i := slices.IndexFunc(d.live.Dests, func(x store.BackupDest) bool { return x.ID == id })
	if i < 0 || d.live.Dests[i].Kind == backup.Local {
		return ""
	}
	return d.live.Dests[i].Name
}

func (d *differ) finalPanelDest() string {
	if v, ok := d.f.setting("panel_backup_dest"); ok {
		if v == backup.Local {
			return ""
		}
		return v
	}
	return d.destName(d.live.eff("panel_backup_dest"))
}

func orgSlug(orgs []store.Org, id string) string {
	i := slices.IndexFunc(orgs, func(o store.Org) bool { return o.ID == id })
	if i < 0 {
		return ""
	}
	return orgs[i].Slug
}

func plural(n int, one string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %ss", n, one)
}

func show(v string) string {
	if len(v) > 120 {
		return v[:117] + "..."
	}
	return v
}

// ---- params ----

// params creates and updates the server scope's params, never deletes; a
// secret is never turned back into a param, and one declared but not set is
// a note (an item that needs it blocks on its own). ponytail: the same walk
// as flow/orgconfig's, flows do not import flows; planfile takes it when a
// third file needs it.
func (d *differ) params() {
	for _, c := range slices.Sorted(maps.Keys(d.f.Params)) {
		for _, n := range slices.Sorted(maps.Keys(d.f.Params[c])) {
			decl, key := d.f.Params[c][n], c+"."+n
			old, ok := d.live.Params[key]
			switch {
			case decl.Type == params.Param && ok && old.Secret:
				d.block("params.%s is a secret; a secret is never turned back into a param", key)
			case decl.Type == params.Secret && ok && !old.Secret:
				d.add(Change{Kind: "param-update", Field: key, Note: "becomes a secret"})
			case decl.Type == params.Secret && (!ok || old.V == ""):
				d.note("params.%s is declared and not set; anything that reads it waits until it is", key)
			case decl.Type == params.Param && decl.Value != nil && !ok:
				d.add(Change{Kind: "param", Field: key, New: *decl.Value})
			case decl.Type == params.Param && decl.Value != nil && old.V != *decl.Value:
				d.add(Change{Kind: "param-update", Field: key, New: *decl.Value})
			}
		}
	}
}

// param is a server param's value: the file's own non-secret declaration
// when it has one (the walk stores params first), else live. ok is false
// when no value is set.
func (d *differ) param(key string) (string, bool) {
	c, n, _ := strings.Cut(key, ".")
	if decl := d.f.Params[c][n]; decl.Type == params.Param && decl.Value != nil && *decl.Value != "" {
		return *decl.Value, true
	}
	v := d.live.Params[key]
	return v.V, v.V != ""
}

// destRefs finds the file's dests whose key params have no value. Only that
// item waits (rule 5): a note, no rows, and whatever names it waits too.
func (d *differ) destRefs() {
	d.waits = map[string]bool{}
	for _, fd := range d.f.BackupDests {
		var missing []string
		for _, ref := range []string{fd.AccessKey, fd.SecretKey, fd.ArchiveKey} {
			if key, ok := ServerRef(ref); ok {
				if _, set := d.param(key); !set {
					missing = append(missing, "server.params."+key)
				}
			}
		}
		if len(missing) > 0 {
			d.waits[fd.Name] = true
			d.note("backup_dests.%s waits for %s, which is not set", fd.Name, strings.Join(missing, ", "))
		}
	}
}

// ---- connector shares ----

// A share row is told apart by Field, never by its value: Field "share" is
// share-all (New or Old "all"), Field "org" is one org by slug. The removal
// keys differ the same way, so an org whose slug is "all" cannot be taken
// for the switch.
func shareKey(conn, org string) string { return "connector-share:" + conn + "/org:" + org }

func shareAllKey(conn string) string { return "connector-share:" + conn + "/all" }

func (d *differ) connectorLive(name string) (ConnectorLive, bool) {
	i := slices.IndexFunc(d.live.Connectors, func(c ConnectorLive) bool { return c.Connector.Name == name })
	if i < 0 {
		return ConnectorLive{}, false
	}
	return d.live.Connectors[i], true
}

// orgKnown says the slug is an org now or one the file creates.
func (d *differ) orgKnown(s string) bool {
	return slices.ContainsFunc(d.live.Orgs, func(o store.Org) bool { return o.Slug == s }) ||
		slices.ContainsFunc(d.f.Orgs, func(o Org) bool { return o.Slug == s })
}

// shared says connector c reaches org slug s once the plan applies: live
// shares stay unless ticked off, the file's list adds.
func (d *differ) shared(c ConnectorLive, s string) bool {
	if c.Connector.ShareAll {
		return true
	}
	for _, id := range c.OrgIDs {
		if orgSlug(d.live.Orgs, id) == s {
			return true
		}
	}
	for _, fc := range d.f.Connectors {
		if fc.Name == c.Connector.Name && fc.Share != nil && (fc.Share.All || slices.Contains(fc.Share.Orgs, s)) {
			return true
		}
	}
	return false
}

// uses says a binding names the connector for the org: live (the verb would
// refuse the revoke) or in this file.
func (d *differ) uses(c ConnectorLive, id, s string) bool {
	if slices.Contains(c.Bound, id) {
		return true
	}
	return slices.ContainsFunc(d.f.Orgs, func(o Org) bool { return o.Slug == s && o.Connector == c.Connector.Name })
}

// losesAll lists the orgs (by slug) a binding names the connector for that
// the file's list does not keep: leaving share-all would strand them, and
// the verb refuses it.
func (d *differ) losesAll(c ConnectorLive, keep []string) []string {
	var lost []string
	for _, id := range c.Bound {
		if s := orgSlug(d.live.Orgs, id); !slices.Contains(keep, s) {
			lost = append(lost, s)
		}
	}
	for _, o := range d.f.Orgs {
		if o.Connector == c.Connector.Name && !slices.Contains(keep, o.Slug) && !slices.Contains(lost, o.Slug) {
			lost = append(lost, o.Slug)
		}
	}
	slices.Sort(lost)
	return lost
}

func shareText(c ConnectorLive, orgs []store.Org) string {
	if c.Connector.ShareAll {
		return "all"
	}
	var s []string
	for _, id := range c.OrgIDs {
		s = append(s, orgSlug(orgs, id))
	}
	slices.Sort(s)
	if len(s) == 0 {
		return "none"
	}
	return strings.Join(s, ", ")
}

// connectors shares the server connectors the file names. It never creates
// one: a GitHub App is made in the panel. Turning "all orgs" on carries an
// impact line; a share that is gone from a present list is a removal row,
// unless a binding uses it (then a note).
func (d *differ) connectors() {
	for _, fc := range d.f.Connectors {
		lc, ok := d.connectorLive(fc.Name)
		if !ok {
			d.block("connectors.%s: no server connector has that name; make it in the panel first", fc.Name)
			continue
		}
		want := fc.Share
		if want == nil {
			continue
		}
		name := fc.Name
		if want.All {
			if !lc.Connector.ShareAll {
				d.add(Change{
					Kind:   "connector-share",
					Tile:   name,
					Field:  "share",
					Old:    shareText(lc, d.live.Orgs),
					New:    "all",
					Impact: fmt.Sprintf("connector %s is shared with every org: every org owner can clone every repo its App is installed on", name),
				})
			}
			continue
		}
		have := map[string]string{} // slug -> org id
		for _, id := range lc.OrgIDs {
			have[orgSlug(d.live.Orgs, id)] = id
		}
		for _, s := range slices.Sorted(slices.Values(want.Orgs)) {
			if !d.orgKnown(s) {
				d.block("connectors.%s: no organization %q", name, s)
				continue
			}
			if _, ok := have[s]; !ok {
				d.add(Change{Kind: "connector-share", Tile: name, Field: "org", New: s})
			}
		}
		if lc.Connector.ShareAll {
			if lost := d.losesAll(lc, want.Orgs); len(lost) > 0 {
				d.note("connectors.%s stays shared with all: a binding in %s names it", name, strings.Join(lost, ", "))
			} else {
				d.remove(Change{Kind: "connector-share-delete", Tile: name, Field: "share", Old: "all", Key: shareAllKey(name)})
			}
		}
		for _, s := range slices.Sorted(maps.Keys(have)) {
			if slices.Contains(want.Orgs, s) {
				continue
			}
			if d.uses(lc, have[s], s) {
				d.note("connectors.%s stays shared with %s: a binding there names it", name, s)
				continue
			}
			d.remove(Change{Kind: "connector-share-delete", Tile: name, Field: "org", Old: s, Key: shareKey(name, s)})
		}
	}
}

// ---- settings ----

// norm folds a value to the form both sides compare in.
func norm(key, v string) string {
	v = strings.TrimSpace(v)
	k, _ := knob(key)
	switch {
	case k.Type == settings.TBool:
		if b, err := strconv.ParseBool(v); err == nil {
			return strconv.FormatBool(b)
		}
	case k.Type == settings.TInt:
		if n, err := strconv.Atoi(v); err == nil {
			return strconv.Itoa(n)
		}
	case key == "panel_domain" || key == "root_domain":
		return installspec.CleanHost(v)
	case key == "trusted_proxies":
		return strings.Join(strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }), ",")
	}
	return v
}

// settings compares each key the file names with its effective value. An
// omitted key is not touched. Risky ones carry an impact line.
func (d *differ) settings() {
	for _, key := range slices.Sorted(maps.Keys(d.f.Settings)) {
		want, _ := d.f.setting(key)
		have := d.live.eff(key)
		if key == "panel_backup_dest" {
			have = d.destName(have)
			if want == backup.Local {
				want = ""
			}
			if want != "" && !d.knowsDest(want) {
				d.block("settings.panel_backup_dest: no backup destination named %q", want)
				continue
			}
			if _, live := d.destByName(want); d.waits[want] && !live {
				d.note("settings.panel_backup_dest waits for backup_dests.%s", want)
				continue
			}
		}
		cmp := want
		if want == "" {
			cmp = settings.Default(key)
		}
		if norm(key, have) == norm(key, cmp) {
			continue
		}
		switch {
		case key == "root_domain" && want == "":
			d.block("settings.root_domain: the root domain cannot be cleared once set")
			continue
		case key == "root_domain" && rootHost(want) != rootHost(have):
			d.renameTarget("settings.root_domain", rootHost(want))
		case key == "panel_domain" && want != "" && slices.Contains(d.live.Taken, want):
			d.block("settings.panel_domain: %s is already a tile domain or domain resource; the panel would shadow it", want)
		}
		c := Change{Kind: "settings", Field: key, Old: show(have), New: show(want), Impact: d.settingImpact(key, have, want)}
		if want == "" {
			c.Note = "back to the default"
		}
		d.add(c)
	}
}

// knowsDest says name is a global S3 destination now or one the file makes.
func (d *differ) knowsDest(name string) bool {
	if _, ok := d.destByName(name); ok {
		return true
	}
	return slices.ContainsFunc(d.f.BackupDests, func(x Dest) bool { return x.Name == name })
}

func (d *differ) settingImpact(key, have, want string) string {
	switch key {
	case "panel_domain":
		if want == "" {
			return "the panel falls back to the installer's host; point DNS at this box"
		}
		s := "panel moves to " + want
		if have != "" {
			s += " and stops answering on " + have + " at once"
		}
		s += "; point DNS at this box first."
		if have != "" {
			s += " CLI logins, API keys and GitHub webhooks still point at " + have
		}
		return s
	case "root_domain":
		from := rootHost(have)
		n := 0
		for _, r := range d.live.Domains {
			if r.Level == domainres.Instance && r.Host == from {
				n = d.live.DomainHosts[r.ID]
			}
		}
		return d.renameImpact("the root domain", from, rootHost(want), n)
	case "dns_provider":
		if want == "" {
			return "wildcard certificates stop being issued; wildcard routes and domains need a provider"
		}
		return "wildcard certificates need the DNS token on the proxy"
	case "proxy_custom":
		return "replaces the extra Caddy routes"
	case "trusted_proxies":
		return "X-Forwarded-For is trusted from these addresses; a wrong list lets clients spoof their IP"
	case "acme_email":
		return "certificates are issued under this contact address from now on"
	}
	return ""
}

// rootHost is the host of the instance resource a root domain seeds.
func rootHost(root string) string { return strings.TrimPrefix(installspec.CleanHost(root), "*.") }

// renameImpact is the line for a domain resource rename: what moves and what
// Let's Encrypt has to issue.
func (d *differ) renameImpact(what, from, to string, hosts int) string {
	s := fmt.Sprintf("%s moves from %s to %s: moves %s, issues %s; point DNS first",
		what, from, to, plural(hosts, "host"), plural(hosts, "certificate"))
	if hosts > 50 && d.dnsProvider == "" {
		s += ". Let's Encrypt allows about 50 certificates a week per domain"
	}
	return s
}

// ---- the cascade rung ----

func (d *differ) defaults() {
	if d.f.Defaults == nil {
		return
	}
	want, have := settings.Settings(*d.f.Defaults), d.live.Defaults
	var rows []Change
	num := func(v *float64) string {
		if v == nil {
			return ""
		}
		return strconv.FormatFloat(*v, 'f', -1, 64)
	}
	field := func(name, old, new string, differs bool) {
		if differs {
			rows = append(rows, Change{Kind: "defaults", Field: name, Old: old, New: new})
		}
	}
	field("cpu_limit", num(have.CPULimit), num(want.CPULimit), !eqPtr(have.CPULimit, want.CPULimit))
	field("mem_limit_mb", intText(have.MemLimitMB), intText(want.MemLimitMB), !eqPtr(have.MemLimitMB, want.MemLimitMB))
	field("protect", boolText(have.Protect), boolText(want.Protect), !eqPtr(have.Protect, want.Protect))
	field("protect_user", strText(have.ProtectUser), strText(want.ProtectUser), !eqPtr(have.ProtectUser, want.ProtectUser))
	// values never shown: protect_password is a credential
	field("protect_password", "", "", !eqPtr(have.ProtectPassword, want.ProtectPassword))
	if len(rows) == 0 {
		return
	}
	for i := range rows {
		if rows[i].Field == "protect_password" {
			rows[i].Note = "changed"
		}
	}
	var parts []string
	if d.live.Tiles > 0 {
		parts = append(parts, "redeploys "+plural(d.live.Tiles, "tile"))
	}
	if slices.ContainsFunc(rows, func(c Change) bool { return strings.HasPrefix(c.Field, "protect") }) {
		parts = append(parts, "changes the basic auth in front of every URL")
	}
	rows[0].Impact = strings.Join(parts, "; ")
	for _, c := range rows {
		d.add(c)
	}
}

func eqPtr[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func intText(v *int) string {
	if v == nil {
		return ""
	}
	return strconv.Itoa(*v)
}

func boolText(v *bool) string {
	if v == nil {
		return ""
	}
	return strconv.FormatBool(*v)
}

func strText(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// ---- backup dests ----

// dests creates and updates the global S3 destinations the file names. The
// three keys are server param refs resolved against the file's params, then
// live; a missing one holds that destination back (destRefs). The archive
// key only applies at creation: on an existing one a different key would make
// its archives unreadable, so it is refused. Local is never declared and
// never offered for removal.
func (d *differ) dests() {
	for _, fd := range d.f.BackupDests {
		if d.waits[fd.Name] {
			continue
		}
		item := "backup_dests." + fd.Name
		key := func(ref string) string { // set: destRefs held the dest back otherwise
			k, _ := ServerRef(ref)
			v, _ := d.param(k)
			return v
		}
		access, secret, archive := key(fd.AccessKey), key(fd.SecretKey), ""
		if fd.ArchiveKey != "" {
			archive = key(fd.ArchiveKey)
		}
		cur, ok := d.destByName(fd.Name)
		if !ok {
			note := "s3"
			if fd.Endpoint != "" {
				note += " " + fd.Endpoint
			}
			d.add(Change{Kind: "dest", Tile: fd.Name, New: fd.Bucket, Note: note})
			continue
		}
		for _, f := range []struct{ name, old, new string }{
			{"endpoint", cur.Endpoint, fd.Endpoint},
			{"region", cur.Region, fd.Region},
			{"bucket", cur.Bucket, fd.Bucket},
			{"shared", strconv.FormatBool(cur.Shared), strconv.FormatBool(fd.Shared)},
		} {
			if f.old != f.new {
				d.add(Change{Kind: "dest-update", Tile: fd.Name, Field: f.name, Old: f.old, New: f.new})
			}
		}
		// keys: never shown
		for _, f := range []struct{ name, old, new string }{
			{"access_key", cur.AccessKey, access},
			{"secret_key", cur.SecretKey, secret},
		} {
			if f.old != f.new {
				d.add(Change{Kind: "dest-update", Tile: fd.Name, Field: f.name, Note: "changed"})
			}
		}
		if archive != "" && archive != cur.ArchiveKey {
			d.block("%s: the archive key of an existing destination cannot change; its archives would no longer decrypt", item)
		}
	}
	if d.f.BackupDests == nil {
		return
	}
	for _, cur := range d.live.Dests {
		if cur.Kind != backup.S3 || cur.OrgID != nil {
			continue
		}
		if slices.ContainsFunc(d.f.BackupDests, func(x Dest) bool { return x.Name == cur.Name }) {
			continue
		}
		switch n := d.live.DestSchedules[cur.ID]; {
		case n > 0:
			d.note("backup_dests.%s stays: %s use it", cur.Name, plural(n, "volume schedule"))
		case d.panelDest == cur.Name:
			d.note("backup_dests.%s stays: the panel backup uses it", cur.Name)
		default:
			d.remove(Change{Kind: "dest-delete", Tile: cur.Name, Old: cur.Bucket, Key: "dest:" + cur.Name})
		}
	}
}

// ---- domains ----

// domains creates and updates the instance domain resources and renames the
// ones a from: names. A root_domain change renames the instance resource on
// its own (SetSetting does it), so a domains: entry for the new root is that
// resource, not a second one: one rename, one impact line. A resource the
// block no longer names is a removal row, or a note while tiles sit under it.
func (d *differ) domains() {
	instance := map[string]store.DomainResource{}
	for _, r := range d.live.Domains {
		if r.Level == domainres.Instance {
			instance[r.Host] = r
		}
	}
	renames := map[string]string{} // new host -> old host, both rename kinds
	var rootFrom, rootTo string    // set when root_domain moves the instance resource at the old root
	liveRoot := rootHost(d.live.eff("root_domain"))
	fileRoot, _ := d.f.setting("root_domain")
	if fileRoot != "" {
		from, to := liveRoot, rootHost(fileRoot)
		if _, has := instance[from]; has && from != to && from != "" {
			renames[to], rootFrom, rootTo = from, from, to
		}
	}
	for _, fd := range d.f.Domains {
		if fd.From == "" {
			continue
		}
		// The root resource and settings.root_domain are one thing (a rename
		// of the resource also sets the setting): the file names one root.
		if _, ok := instance[fd.From]; ok && fd.From == liveRoot && fileRoot != "" && fd.Host != rootHost(fileRoot) {
			d.block("domains: %s renames the root domain %s, but settings.root_domain says %s; they must name the same one", fd.Host, fd.From, rootHost(fileRoot))
		}
		if fd.Host != rootTo {
			renames[fd.Host] = fd.From
		}
	}
	named := map[string]bool{}
	for _, fd := range d.f.Domains {
		named[fd.Host] = true
		old, renaming := renames[fd.Host]
		if renaming {
			named[old] = true
		}
		_, haveOld := instance[old]
		_, haveNew := instance[fd.Host]
		row, have := instance[fd.Host]
		moved := false // a rename's target is a new host: it gets the add path's checks
		switch {
		case renaming && haveOld && haveNew:
			d.block("domains: %s and %s both exist; delete one by hand first", old, fd.Host)
			continue
		case renaming && !haveOld && !haveNew:
			d.block("domains: neither %s nor %s exists", old, fd.Host)
			continue
		case renaming && haveOld:
			row, have, moved = instance[old], true, true
			d.renameTarget("domains", fd.Host)
			if fd.Host != rootTo { // the root_domain row carries the line
				c := Change{
					Kind:   "domain-rename",
					Tile:   fd.Host,
					Old:    old,
					New:    fd.Host,
					Impact: d.renameImpact("instance domain", old, fd.Host, d.live.DomainHosts[row.ID]),
				}
				if old == liveRoot && fileRoot == "" {
					c.Note = "also sets root_domain"
				}
				d.add(c)
			}
		}
		spec := domainres.Spec{
			Level:               domainres.Instance,
			Host:                fd.Host,
			IncludeEnvOnDefault: fd.IncludeEnvOnDefault,
			ACMEEmail:           fd.ACMEEmail,
		}
		// An existing resource is not squatting anything: its slug check is a
		// create-only one, and export then plan has to read clean.
		orgs := d.live.Orgs
		if have && !moved {
			orgs = nil
		}
		want, err := domainres.Prepare(spec, "", orgs)
		if err != nil {
			d.block("domains: %s: %v", fd.Host, err)
			continue
		}
		if !have {
			if i := slices.IndexFunc(d.live.Domains, func(r store.DomainResource) bool { return r.Host == want.Host }); i >= 0 {
				d.block("domains: %s is already a domain resource", want.Host)
				continue
			}
			if route.Holds(d.finalRoutes(), want.Host) {
				d.block("domains: %s is an external route", want.Host)
				continue
			}
			d.add(Change{Kind: "domain", New: want.Host})
			continue
		}
		if row.IncludeEnvOnDefault != want.IncludeEnvOnDefault {
			d.add(Change{
				Kind:  "domain-update",
				Tile:  want.Host,
				Field: "include_env_on_default",
				Old:   strconv.FormatBool(row.IncludeEnvOnDefault),
				New:   strconv.FormatBool(want.IncludeEnvOnDefault),
			})
		}
		if row.ACMEEmail != want.ACMEEmail {
			d.add(Change{Kind: "domain-update", Tile: want.Host, Field: "acme_email", Old: row.ACMEEmail, New: want.ACMEEmail})
		}
	}
	if d.f.Domains == nil {
		return
	}
	for _, host := range slices.Sorted(maps.Keys(instance)) {
		r := instance[host]
		if named[host] {
			continue
		}
		if host == rootFrom {
			continue // root_domain renames it away
		}
		if n := d.live.DomainHosts[r.ID]; n > 0 {
			d.note("domains.%s stays: %s sit under it", host, plural(n, "tile domain"))
			continue
		}
		d.remove(Change{Kind: "domain-delete", Tile: host, Key: "domain:" + host})
	}
}

// renameTarget blocks a host a rename or a root_domain move cannot take, the
// refusals of the add path and of the rename verb: another resource, an
// external route, the panel's own host, a tile domain. what names the source.
func (d *differ) renameTarget(what, host string) {
	if d.checked[host] {
		return
	}
	if d.checked == nil {
		d.checked = map[string]bool{}
	}
	d.checked[host] = true
	switch {
	case slices.ContainsFunc(d.live.Domains, func(r store.DomainResource) bool { return r.Host == host }):
		d.block("%s: %s is already a domain resource", what, host)
	case route.Holds(d.finalRoutes(), host):
		d.block("%s: %s is an external route", what, host)
	case d.panelDomain != "" && host == installspec.CleanHost(d.panelDomain):
		d.block("%s: %s is the panel domain", what, host)
	case slices.Contains(d.live.Taken, host):
		d.block("%s: %s is already a tile domain", what, host)
	}
}

// ---- routes ----

// finalRoutes are the routes once the file applies: live routes it does not
// name stay (a removal is ticked), the ones it names take its spelling.
func (d *differ) finalRoutes() []store.Route {
	by := map[string]store.Route{}
	for _, r := range d.live.Routes {
		by[r.Host] = r
	}
	for _, fr := range d.f.Routes {
		by[fr.Host] = store.Route{Host: fr.Host, Mode: fr.Mode, Target: fr.Target, Insecure: fr.Insecure}
	}
	return slices.Collect(maps.Values(by))
}

// routes creates, updates and offers to delete the external routes. The
// service has no route update, so a changed route is a delete plus a create
// there. Blockers are what the create would refuse.
func (d *differ) routes() {
	liveBy := map[string]store.Route{}
	for _, r := range d.live.Routes {
		liveBy[r.Host] = r
	}
	// The panel check looks at what the file touches, and at every route when
	// the panel domain moves: a live route over the panel host that the file
	// never mentions is not this plan's to refuse. Only a pass-through route
	// takes more than its own name (layer4 answers before HTTP); any other
	// route loses just the panel's name, so it is an exact match.
	panel := installspec.CleanHost(d.panelDomain)
	panelMoves := norm("panel_domain", d.panelDomain) != norm("panel_domain", d.live.eff("panel_domain"))
	for _, r := range d.finalRoutes() {
		touched := panelMoves || slices.ContainsFunc(d.f.Routes, func(x Route) bool { return x.Host == r.Host })
		switch {
		case panel == "" || !touched:
		case r.Mode == "passthrough" && route.Overlap(r.Host, panel):
			d.block("routes: the pass-through route for %s covers the panel domain %s", r.Host, panel)
		case r.Mode != "passthrough" && r.Host == panel:
			d.block("routes: %s is shadowed by the panel domain %s", r.Host, panel)
		}
	}
	for i, fr := range d.f.Routes {
		cur, ok := liveBy[fr.Host]
		// create-only: a route that is live and keeps its mode is not this
		// plan's to refuse (SetSettings lets the provider go), or export then
		// plan would block for good.
		switch {
		case ok && cur.Mode == fr.Mode:
		case strings.HasPrefix(fr.Host, "*.") && fr.Mode != "passthrough" && d.dnsProvider == "":
			d.block("routes: %s: a wildcard host needs a DNS provider, or the passthrough mode", fr.Host)
		case fr.Mode == "passthrough" && d.live.TLSOff:
			d.block("routes: %s: pass-through needs HTTPS on", fr.Host)
		}
		for _, o := range d.f.Routes[i+1:] {
			if route.Overlap(fr.Host, o.Host) {
				d.block("routes: %s overlaps %s", fr.Host, o.Host)
			}
		}
		if !ok {
			for _, t := range d.live.Taken {
				if route.Holds([]store.Route{{Host: fr.Host}}, t) {
					d.block("routes: %s is already a tile domain or domain resource", fr.Host)
					break
				}
			}
			for _, l := range d.live.Routes {
				if route.Overlap(fr.Host, l.Host) {
					d.block("routes: %s overlaps the route for %s", fr.Host, l.Host)
				}
			}
			d.add(Change{Kind: "route", Tile: fr.Host, New: fr.Target, Note: fr.Mode})
			continue
		}
		for _, f := range []struct{ name, old, new string }{
			{"mode", cur.Mode, fr.Mode},
			{"target", cur.Target, fr.Target},
			{"insecure", strconv.FormatBool(cur.Insecure), strconv.FormatBool(fr.Insecure)},
		} {
			if f.old != f.new {
				d.add(Change{Kind: "route-update", Tile: fr.Host, Field: f.name, Old: f.old, New: f.new})
			}
		}
	}
	if d.f.Routes == nil {
		return
	}
	for _, cur := range d.live.Routes {
		if !slices.ContainsFunc(d.f.Routes, func(r Route) bool { return r.Host == cur.Host }) {
			d.remove(Change{Kind: "route-delete", Tile: cur.Host, Old: cur.Target, Key: "route:" + cur.Host})
		}
	}
}

// ---- orgs ----

func (d *differ) orgLive(s string) (store.Org, bool) {
	i := slices.IndexFunc(d.live.Orgs, func(o store.Org) bool { return o.Slug == s })
	if i < 0 {
		return store.Org{}, false
	}
	return d.live.Orgs[i], true
}

func pathOf(p string) string {
	if p == "" {
		return OrgFilePath
	}
	return p
}

// connectorName is the server connector's name for an id; "" for none or an
// org's own connector.
func (d *differ) connectorName(id string) string {
	i := slices.IndexFunc(d.live.Connectors, func(c ConnectorLive) bool { return c.Connector.ID == id })
	if i < 0 {
		return ""
	}
	return d.live.Connectors[i].Connector.Name
}

// orgs creates the orgs the file names that do not exist (an impact line:
// an auto-applied plan has no approver to own them) and binds each to its
// own org file through a server connector shared with it. A bound org the
// file no longer binds is a removal row; orgs are never removed.
func (d *differ) orgs() {
	for _, fo := range d.f.Orgs {
		item := "orgs." + fo.Slug
		if fo.Connector != "" {
			c, ok := d.connectorLive(fo.Connector)
			switch {
			case !ok:
				d.block("%s: no server connector named %s", item, fo.Connector)
			case !d.shared(c, fo.Slug):
				d.block("%s: connector %s is not shared with it; share it under connectors:", item, fo.Connector)
			}
		}
		cur, exists := d.orgLive(fo.Slug)
		if !exists {
			d.createOrg(fo)
			continue
		}
		if fo.Name != "" && fo.Name != cur.Name {
			d.note("%s is named %q; the name in the file only applies at creation, the org file renames it", item, cur.Name)
		}
		if fo.Repo == "" {
			if cur.ConfigRepo != "" {
				d.unbind(cur)
			}
			continue
		}
		for _, f := range []struct{ name, old, new string }{
			{"repo", cur.ConfigRepo, fo.Repo},
			{"branch", cur.ConfigBranch, fo.Branch},
			{"path", pathOf(cur.ConfigPath), pathOf(fo.Path)},
			{"connector", d.connectorName(cur.ConfigConnectorID), fo.Connector},
			{"auto", strconv.FormatBool(cur.ConfigAuto), strconv.FormatBool(fo.Auto)},
		} {
			if f.old != f.new {
				d.add(Change{Kind: "org-bind", Tile: fo.Slug, Field: f.name, Old: f.old, New: f.new})
			}
		}
	}
	if d.f.Orgs == nil {
		return
	}
	for _, o := range d.live.Orgs {
		if o.ConfigRepo != "" && !slices.ContainsFunc(d.f.Orgs, func(x Org) bool { return x.Slug == o.Slug }) {
			d.unbind(o)
		}
	}
}

func (d *differ) unbind(o store.Org) {
	d.remove(Change{Kind: "org-unbind", Tile: o.Slug, Old: o.ConfigRepo, Key: "org-binding:" + o.Slug})
}

// createOrg plans a new org. The slug is the name's slug, as every org's is
// (leaf/org.Rename); a slug a foreign domain leads with is refused.
func (d *differ) createOrg(fo Org) {
	item := "orgs." + fo.Slug
	name := fo.Name
	if name == "" {
		name = fo.Slug
	}
	if slug.Make(name) != fo.Slug {
		d.block("%s: the name %q makes the slug %q; name it so it makes %q", item, name, slug.Make(name), fo.Slug)
		return
	}
	for _, c := range d.live.Claims {
		if slug.OfHost(c.Host) == fo.Slug {
			d.block("%s: another organization's domain %s leads with %q", item, c.Host, fo.Slug)
			break
		}
	}
	c := Change{
		Kind:   "org-create",
		Tile:   fo.Slug,
		New:    name,
		Impact: fmt.Sprintf("creates org %s; you become its owner", fo.Slug),
	}
	if fo.Repo != "" {
		c.Note = "bound to " + fo.Repo
	}
	d.add(c)
}
