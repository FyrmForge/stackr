package service

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/publicsuffix"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/dnscap"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// dnscustom.go is custom DNS records (RFC "Change scope and custom DNS
// records", Part 2): TXT, MX, CNAME, A and AAAA records declared on an app
// hostname (they live and die with it) or on a managed domain itself.
// Every surface runs the same checks (checkCustom): the value fits its
// type, the name is stackr's to write (a managed domain the org may use,
// inside its scope), an app's records stay on its own name and below, a
// name is never both an app hostname and a custom record (TXT and MX
// aside), and a CNAME gets its name to itself. The records sync writes
// them and puts them back when they change outside stackr.

type (
	// CustomRecordIn is one record as a form, the API or a stack file
	// gives it. Name is relative to its owner ("" = the owner itself).
	CustomRecordIn struct {
		Name  string `json:"name"`
		Type  string `json:"type"`
		Value string `json:"value"`
	}
	// CustomRecord is a declared record as read back. Host is its full
	// name. Note is how an add landed ("taken over", "added (1 other
	// there stays)"). Written: stackr has it in the zone. PutBackAt: it
	// changed outside stackr and stackr put it back then.
	CustomRecord struct {
		ID        string     `json:"id"`
		Name      string     `json:"name"`
		Type      string     `json:"type"`
		Value     string     `json:"value"`
		Host      string     `json:"host"`
		Note      string     `json:"note,omitempty"`
		Written   bool       `json:"written"`
		PutBackAt *time.Time `json:"put_back_at,omitempty"`
	}
	// CustomRecords is an owner's declared records and where they are
	// written: "managed by fyrmforge.dev via Namecheap (server)"; Source
	// "" while no managed domain covers the owner (records cannot be
	// added).
	CustomRecords struct {
		Source  string         `json:"source"`
		Records []CustomRecord `json:"records"`
	}
)

// nameClash is decision 7's refusal, the same both ways: a name is an app
// hostname or a custom record, never both, except TXT and MX alongside.
func nameClash(host, typ string) error {
	return errs.Conflictf("%s is an app hostname, so it cannot also have a custom %s record; only TXT and MX can sit next to an app", host, typ)
}

// alongside says whether a custom record of typ may share an app hostname.
func alongside(typ string) bool { return typ == "TXT" || typ == "MX" }

// recordZone is the managed domain stackr writes host's records in, for a
// tile of orgID: the longest match, inside its scope, available to the
// org. Anything else is a conflict naming host and the domain.
func (o *Orchestrator) recordZone(ctx context.Context, orgID, host string) (store.DNSDomain, error) {
	zones, err := o.dns.All(ctx)
	if err != nil {
		return store.DNSDomain{}, err
	}
	z, ok := dnscap.ZoneOf(host, zones)
	if !ok {
		for _, d := range zones {
			if dnscap.Name(host, d.Name) != "" {
				return z, errs.Conflictf("%s is outside the part of %s stackr manages (only under %s); add the record at your DNS host", host, d.Name, d.Scope)
			}
		}
		domain, err := publicsuffix.EffectiveTLDPlusOne(host)
		if err != nil {
			domain = host
		}
		return z, errs.Conflictf("%s is not in a managed domain: stackr does not manage %s", host, domain)
	}
	if !dnscap.MayUse(z, orgID) {
		slug := orgID
		if s := o.orgSlugs(ctx, []string{orgID}); len(s) == 1 {
			slug = s[0]
		}
		return z, errs.Conflictf("%s is managed but not available to org %s", z.Name, slug)
	}
	return z, nil
}

// recordSource is where z's records are written, as the drawer and the
// plan say it: "Managed by fyrmforge.dev via Namecheap (server)".
func (o *Orchestrator) recordSource(ctx context.Context, z store.DNSDomain) string {
	return "Managed by " + z.Name + " " + o.viaOf(ctx, z.ConnectorID)
}

// viaOf is the connector a managed domain is written through, as the
// screens say it: "via Namecheap (server)".
func (o *Orchestrator) viaOf(ctx context.Context, connectorID string) string {
	level, via := "org", "a DNS connector"
	if c, err := o.store.Connectors.Get(ctx, connectorID); err == nil {
		via = o.label(c.Kind)
		if c.OrgID == nil {
			level = "server"
		}
	}
	return fmt.Sprintf("via %s (%s)", via, level)
}

// customOwner is whose record checkCustom checks: an app hostname (App,
// Host its host) or a managed domain (Zone).
type customOwner struct {
	App   bool
	Host  string
	Org   string          // the app's org
	Zone  store.DNSDomain // the domain's row
	Hosts []string        // other app hostnames a stack file is adding
}

// checkCustom is every check a custom record takes before it is saved
// (decisions 1, 2, 4, 6, 7, 8 and the value's type): the record as stored,
// the zone it is written in and how it lands there (Fit's note). live
// false skips the provider read (Fit).
func (o *Orchestrator) checkCustom(ctx context.Context, own customOwner, in CustomRecordIn, live bool) (store.CustomRecord, store.DNSDomain, string, error) {
	name, err := dnscap.CustomName(in.Name)
	if err != nil {
		return store.CustomRecord{}, store.DNSDomain{}, "", err
	}
	typ, value, err := dnscap.CustomValue(in.Type, in.Value)
	if err != nil {
		return store.CustomRecord{}, store.DNSDomain{}, "", err
	}
	rec := store.CustomRecord{Name: name, Type: typ, Value: value}
	owner := own.Host
	if !own.App {
		owner = own.Zone.Name
	}
	host := dnscap.CustomFQDN(name, owner)
	z := own.Zone
	if own.App {
		if z, err = o.recordZone(ctx, own.Org, host); err != nil {
			return rec, z, "", err
		}
	} else if !dnscap.InScope(z, dnscap.Name(host, z.Name)) {
		return rec, z, "", errs.Conflictf("%s is outside the part of %s stackr manages (only under %s); add the record at your DNS host", host, z.Name, z.Scope)
	}
	ds, err := o.domains.List(ctx)
	if err != nil {
		return rec, z, "", err
	}
	hosts := slices.Clone(own.Hosts)
	for _, d := range ds {
		hosts = append(hosts, d.Host)
	}
	for _, h := range hosts {
		switch {
		case !strings.EqualFold(h, host):
		case own.App && !strings.EqualFold(h, own.Host):
			return rec, z, "", errs.Conflictf("%s is another app's hostname; an app's records stay on its own name and below", host)
		case !alongside(typ):
			return rec, z, "", nameClash(host, typ)
		}
	}
	if !live {
		return rec, z, "", nil
	}
	p, err := o.dnsAccounts(ctx, z.ConnectorID, z.OrgID)
	if err != nil {
		return rec, z, "", err
	}
	note, err := o.dns.Fit(ctx, p, z.Name, dnscap.Name(host, z.Name), typ, value)
	return rec, z, note, err
}

// checkNotCustom refuses host as an app hostname while a custom record
// other than TXT or MX is declared on that name (decision 7's other way).
func (o *Orchestrator) checkNotCustom(ctx context.Context, host string) error {
	host = dnscap.Normal(host)
	ds, err := o.domains.List(ctx)
	if err != nil {
		return err
	}
	for _, d := range ds {
		for _, r := range d.Records {
			if !alongside(r.Type) && dnscap.CustomFQDN(r.Name, d.Host) == host {
				return nameClash(host, r.Type)
			}
		}
	}
	zones, err := o.dns.All(ctx)
	if err != nil {
		return err
	}
	for _, z := range zones {
		for _, r := range z.Records {
			if !alongside(r.Type) && dnscap.CustomFQDN(r.Name, z.Name) == host {
				return nameClash(host, r.Type)
			}
		}
	}
	return nil
}

// checkMovedRecords runs a tile domain's records through checkCustom at
// host, its new name: they move with it. One provider read, only with an
// MX among them: the new zone's mail setting may drop it, refused as an
// add is. Any other clash there is the records sync's to report.
func (o *Orchestrator) checkMovedRecords(ctx context.Context, t Tile, host string, rs []store.CustomRecord) error {
	if len(rs) == 0 {
		return nil
	}
	org, err := o.tileOrg(ctx, t)
	if err != nil {
		return err
	}
	var mx *store.DNSDomain // the zone an MX lands in
	for _, r := range rs {
		_, z, _, err := o.checkCustom(ctx, customOwner{App: true, Host: dnscap.Normal(host), Org: org}, CustomRecordIn{r.Name, r.Type, r.Value}, false)
		if err != nil {
			return err
		}
		if r.Type == "MX" && mx == nil {
			mx = &z
		}
	}
	if mx == nil {
		return nil
	}
	p, err := o.dnsAccounts(ctx, mx.ConnectorID, mx.OrgID)
	if err != nil {
		return err
	}
	return o.dns.MXRefused(ctx, p, mx.Name)
}

// customWants is the custom records stackr should have now: each tile
// domain's in the managed domain the record's name sits in (one its org
// may use), and each managed domain's own inside its scope.
func (o *Orchestrator) customWants(ctx context.Context, zones []store.DNSDomain) ([]dnscap.Want, error) {
	ds, err := o.domains.List(ctx)
	if err != nil {
		return nil, err
	}
	orgOf := map[string]string{}
	var out []dnscap.Want
	for _, d := range ds {
		if len(d.Records) == 0 || strings.HasPrefix(d.Host, "*.") {
			continue
		}
		org, ok := orgOf[d.TileID]
		if !ok {
			t, err := o.tiles.Get(ctx, d.TileID)
			if err != nil {
				return nil, err
			}
			if org, err = o.tileOrg(ctx, t); err != nil {
				return nil, err
			}
			orgOf[d.TileID] = org
		}
		for _, r := range d.Records {
			host := dnscap.CustomFQDN(r.Name, d.Host)
			z, ok := dnscap.ZoneOf(host, zones)
			if !ok || !dnscap.MayUse(z, org) {
				continue
			}
			out = append(out, dnscap.Want{
				DNSDomainID: z.ID, Zone: z.Name, Name: dnscap.Name(host, z.Name),
				Type: r.Type, Value: r.Value, DomainID: d.ID, Kind: store.RecordCustom,
			})
		}
	}
	for _, z := range zones {
		for _, r := range z.Records {
			name := dnscap.Name(dnscap.CustomFQDN(r.Name, z.Name), z.Name)
			if dnscap.InScope(z, name) {
				out = append(out, dnscap.Want{DNSDomainID: z.ID, Zone: z.Name, Name: name, Type: r.Type, Value: r.Value, Kind: store.RecordCustom})
			}
		}
	}
	return out, nil
}

// customOut is rs as read back, owner's (a host or a zone), with what the
// records table says of each: written, put back.
func (o *Orchestrator) customOut(ctx context.Context, rs []store.CustomRecord, owner string, domainID *string) ([]CustomRecord, error) {
	rows, err := o.dns.Records(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]CustomRecord, 0, len(rs))
	for _, r := range rs {
		c := CustomRecord{ID: r.ID, Name: r.Name, Type: r.Type, Value: r.Value, Host: dnscap.CustomFQDN(r.Name, owner)}
		for _, x := range rows {
			if x.Kind == store.RecordCustom && deref(x.DomainID) == deref(domainID) && x.Type == r.Type &&
				strings.EqualFold(zoneHost(x.Name, x.Zone), c.Host) && dnscap.SameValue(r.Type, x.Value, r.Value) {
				c.Written, c.PutBackAt = true, x.PutBackAt
			}
		}
		out = append(out, c)
	}
	return out, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// tileDomain is tile's domain id, refused as not found when it is
// another tile's.
func (o *Orchestrator) tileDomain(ctx context.Context, tileID, id string) (Domain, Tile, string, error) {
	d, err := o.domains.Get(ctx, id)
	if err != nil {
		return d, Tile{}, "", err
	}
	if d.TileID != tileID {
		return d, Tile{}, "", errs.ErrNotFound
	}
	t, err := o.tiles.Get(ctx, tileID)
	if err != nil {
		return d, t, "", err
	}
	org, err := o.tileOrg(ctx, t)
	return d, t, org, err
}

// TileDomainRecords is the custom records declared on a tile domain's
// host, and where they are written.
func (o *Orchestrator) TileDomainRecords(ctx context.Context, tileID, id string) (CustomRecords, error) {
	d, _, org, err := o.tileDomain(ctx, tileID, id)
	if err != nil {
		return CustomRecords{}, err
	}
	out := CustomRecords{}
	if !strings.HasPrefix(d.Host, "*.") {
		if z, err := o.recordZone(ctx, org, d.Host); err == nil {
			out.Source = o.recordSource(ctx, z)
		}
	}
	out.Records, err = o.customOut(ctx, d.Records, d.Host, &d.ID)
	return out, err
}

// AddTileDomainRecord declares a custom record on a tile domain's host and
// writes it. The record is saved before the write: a provider error is
// returned with it, and the records sync tries again.
func (o *Orchestrator) AddTileDomainRecord(ctx context.Context, tileID, id string, in CustomRecordIn) (CustomRecord, error) {
	d, _, org, err := o.tileDomain(ctx, tileID, id)
	if err != nil {
		return CustomRecord{}, err
	}
	if strings.HasPrefix(d.Host, "*.") {
		return CustomRecord{}, errs.Invalidf("name", "A wildcard host takes no custom records.")
	}
	rec, z, note, err := o.checkCustom(ctx, customOwner{App: true, Host: d.Host, Org: org}, in, true)
	if err != nil {
		return CustomRecord{}, err
	}
	if hasRecord(d.Records, rec) {
		return CustomRecord{}, errs.Conflictf("That record is already there.")
	}
	rec.ID = uuid.NewString()
	s, err := specOf(d)
	if err != nil {
		return CustomRecord{}, err
	}
	s.Records = append(slices.Clone(d.Records), rec)
	if _, err := o.domains.Update(ctx, d, s, o.dns01(ctx, s.Host)); err != nil {
		return CustomRecord{}, err
	}
	out := CustomRecord{ID: rec.ID, Name: rec.Name, Type: rec.Type, Value: rec.Value, Host: dnscap.CustomFQDN(rec.Name, d.Host), Note: note}
	err = dnsErrFor(o.reconcileDNS(ctx, nil), z.Name, d.Host)
	out.Written = err == nil
	return out, err
}

// RemoveTileDomainRecord drops a custom record from a tile domain; stackr
// deletes it from the zone.
func (o *Orchestrator) RemoveTileDomainRecord(ctx context.Context, tileID, id, recordID string) error {
	d, _, _, err := o.tileDomain(ctx, tileID, id)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(d.Records, func(r store.CustomRecord) bool { return r.ID == recordID })
	if i < 0 {
		return errs.ErrNotFound
	}
	s, err := specOf(d)
	if err != nil {
		return err
	}
	s.Records = slices.Delete(slices.Clone(d.Records), i, i+1)
	if _, err := o.domains.Update(ctx, d, s, o.dns01(ctx, s.Host)); err != nil {
		return err
	}
	zone := ""
	if z, ok := dnscap.ZoneOf(d.Host, o.managedZones(ctx)); ok {
		zone = z.Name
	}
	return dnsErrFor(o.reconcileDNS(ctx, nil), zone, d.Host)
}

func hasRecord(rs []store.CustomRecord, r store.CustomRecord) bool {
	return slices.ContainsFunc(rs, func(x store.CustomRecord) bool {
		return x.Name == r.Name && x.Type == r.Type && dnscap.SameValue(r.Type, x.Value, r.Value)
	})
}

// managedZones is every managed domain; none when they cannot be read.
func (o *Orchestrator) managedZones(ctx context.Context) []store.DNSDomain {
	zs, _ := o.dns.All(ctx)
	return zs
}

// managedRow is connector id's managed domain.
func (o *Orchestrator) managedRow(ctx context.Context, orgID *string, id, domain string) (store.DNSDomain, error) {
	c, _, err := o.dnsAccount(ctx, orgID, id)
	if err != nil {
		return store.DNSDomain{}, err
	}
	rows, err := o.dns.ByConnector(ctx, c.ID)
	if err != nil {
		return store.DNSDomain{}, err
	}
	i := slices.IndexFunc(rows, func(r store.DNSDomain) bool { return r.Name == dnscap.Normal(domain) })
	if i < 0 {
		return store.DNSDomain{}, errs.ErrNotFound
	}
	return rows[i], nil
}

// DNSDomainRecords is the custom records declared on a managed domain
// itself (an apex MX, a verification TXT).
func (o *Orchestrator) DNSDomainRecords(ctx context.Context, orgID *string, id, domain string) (CustomRecords, error) {
	z, err := o.managedRow(ctx, orgID, id, domain)
	if err != nil {
		return CustomRecords{}, err
	}
	out := CustomRecords{Source: o.recordSource(ctx, z)}
	out.Records, err = o.customOut(ctx, z.Records, z.Name, nil)
	return out, err
}

// AddDNSDomainRecord declares a custom record on a managed domain and
// writes it; saved before the write, as AddTileDomainRecord.
func (o *Orchestrator) AddDNSDomainRecord(ctx context.Context, orgID *string, id, domain string, in CustomRecordIn) (CustomRecord, error) {
	z, err := o.managedRow(ctx, orgID, id, domain)
	if err != nil {
		return CustomRecord{}, err
	}
	rec, _, note, err := o.checkCustom(ctx, customOwner{Zone: z}, in, true)
	if err != nil {
		return CustomRecord{}, err
	}
	if hasRecord(z.Records, rec) {
		return CustomRecord{}, errs.Conflictf("That record is already there.")
	}
	rec.ID = uuid.NewString()
	if _, err := o.dns.SetRecords(ctx, z.ID, append(slices.Clone(z.Records), rec)); err != nil {
		return CustomRecord{}, err
	}
	out := CustomRecord{ID: rec.ID, Name: rec.Name, Type: rec.Type, Value: rec.Value, Host: dnscap.CustomFQDN(rec.Name, z.Name), Note: note}
	err = dnsErrFor(o.reconcileDNS(ctx, nil), z.Name, "")
	out.Written = err == nil
	return out, err
}

// RemoveDNSDomainRecord drops a managed domain's custom record; stackr
// deletes it from the zone.
func (o *Orchestrator) RemoveDNSDomainRecord(ctx context.Context, orgID *string, id, domain, recordID string) error {
	z, err := o.managedRow(ctx, orgID, id, domain)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(z.Records, func(r store.CustomRecord) bool { return r.ID == recordID })
	if i < 0 {
		return errs.ErrNotFound
	}
	if _, err := o.dns.SetRecords(ctx, z.ID, slices.Delete(slices.Clone(z.Records), i, i+1)); err != nil {
		return err
	}
	return dnsErrFor(o.reconcileDNS(ctx, nil), z.Name, "")
}

// CheckFileRecords is the stack file's DNS check of one domain entry: its
// records: (each as checkCustom takes it) and dns: managed (managed, with
// no records, still needs a managed domain covering host). have is the
// row's records today (kept ids, no provider read for those); hosts the
// other hostnames the file adds. It returns the records as stored and a
// note per new one.
func (o *Orchestrator) CheckFileRecords(ctx context.Context, orgID, host string, recs, have []store.CustomRecord, managed bool, hosts []string) ([]store.CustomRecord, []string, error) {
	if err := o.checkNotCustom(ctx, host); err != nil {
		return nil, nil, err
	}
	if len(recs) == 0 && !managed {
		return nil, nil, nil
	}
	if len(recs) > 0 && strings.HasPrefix(host, "*.") {
		return nil, nil, errs.Invalidf("records", "a wildcard host takes no records")
	}
	z, err := o.recordZone(ctx, orgID, dnscap.Normal(host))
	if err != nil {
		return nil, nil, err
	}
	var out []store.CustomRecord
	notes := []string{o.recordSource(ctx, z)}
	for _, in := range recs {
		rec, _, _, err := o.checkCustom(ctx, customOwner{App: true, Host: dnscap.Normal(host), Org: orgID, Hosts: hosts}, CustomRecordIn{in.Name, in.Type, in.Value}, false)
		if err != nil {
			return nil, nil, err
		}
		if hasRecord(out, rec) {
			return nil, nil, errs.Invalidf("records", "%s %s is declared twice", rec.Type, dnscap.CustomFQDN(rec.Name, host))
		}
		if i := slices.IndexFunc(have, func(x store.CustomRecord) bool { return hasRecord([]store.CustomRecord{x}, rec) }); i >= 0 {
			out = append(out, have[i])
			continue
		}
		_, _, note, err := o.checkCustom(ctx, customOwner{App: true, Host: dnscap.Normal(host), Org: orgID, Hosts: hosts}, CustomRecordIn{in.Name, in.Type, in.Value}, true)
		if err != nil {
			return nil, nil, err
		}
		rec.ID = uuid.NewString()
		out = append(out, rec)
		if note != "" {
			notes = append(notes, rec.Type+" "+dnscap.CustomFQDN(rec.Name, host)+": "+note)
		}
	}
	return out, notes, nil
}
