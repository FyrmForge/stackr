package dnscap

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/libdns/libdns"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// TTL is every record stackr makes: short, so a repoint lands in minutes.
const TTL = 300

// Want is a record stackr should have on a managed domain: an A or AAAA
// for a tile domain's host, to its ingress node.
type Want struct {
	DNSDomainID string
	Zone        string
	Name        string // relative to Zone, "@" the apex
	Type        string // A or AAAA
	Value       string
	DomainID    string // the tile domain
	// Hold keeps whatever stackr has at Zone and Name as it is: the
	// serving node has no address to point at right now, and deleting a
	// live record over that would take the host down.
	Hold bool
}

func (w Want) key() string { return w.Zone + "|" + strings.ToLower(w.Name) + "|" + w.Type }

func rowKey(r store.DNSRecord) string {
	return r.Zone + "|" + strings.ToLower(r.Name) + "|" + r.Type
}

// Accounts opens a connector's provider by connector id.
type Accounts func(ctx context.Context, connectorID string, orgID *string) (Provider, error)

// RecordTaken is the refusal for a name that has a record stackr did not
// make: Replace (the panel's "Replace it", --replace) takes it over.
type RecordTaken struct{ Hosts []string }

func (e *RecordTaken) Error() string {
	return strings.Join(e.Hosts, ", ") + " already has a DNS record stackr did not create; replace it to take it over"
}

// ZoneError is one zone's failed reconcile.
type ZoneError struct {
	Zone string
	Err  error
}

func (e *ZoneError) Error() string { return "DNS for " + e.Zone + ": " + e.Err.Error() }
func (e *ZoneError) Unwrap() error { return e.Err }

// Name is host relative to zone: "@" for the apex, "" when host is not in
// the zone.
func Name(host, zone string) string {
	host, zone = Normal(host), Normal(zone)
	if host == zone {
		return "@"
	}
	if n, ok := strings.CutSuffix(host, "."+zone); ok {
		return n
	}
	return ""
}

// ZoneOf is the managed domain host sits in (the longest that matches) and
// whether there is one.
func ZoneOf(host string, zones []store.DNSDomain) (store.DNSDomain, bool) {
	best, ok := store.DNSDomain{}, false
	for _, z := range zones {
		if Name(host, z.Name) != "" && len(z.Name) > len(best.Name) {
			best, ok = z, true
		}
	}
	return best, ok
}

// WithZone runs fn with zone's lock held: fn's provider is the raw one, so
// a read and the write it decides stay one step. A provider may rewrite the
// whole zone on every write; a writer between the two would be lost.
func (l *Leaf) WithZone(_ context.Context, p Provider, zone string, fn func(Provider) error) error {
	defer l.lock(zone)()
	return fn(p)
}

// blocks is a live record that stops stackr putting an address on its
// name: an address of its own, or a CNAME, which no other record shares.
func blocks(r libdns.RR, name string) bool {
	return strings.EqualFold(r.Name, name) && (r.Type == "A" || r.Type == "AAAA" || r.Type == "CNAME")
}

func ours(r libdns.RR, rows []store.DNSRecord, zone string) bool {
	return slices.ContainsFunc(rows, func(x store.DNSRecord) bool {
		return x.Zone == zone && strings.EqualFold(x.Name, r.Name) && x.Type == r.Type && x.Value == r.Data
	})
}

// foreign is the live records at name stackr did not make.
func foreign(live []libdns.Record, rows []store.DNSRecord, zone, name string) []libdns.Record {
	var out []libdns.Record
	for _, r := range live {
		rr := r.RR()
		if blocks(rr, name) && !ours(rr, rows, zone) {
			out = append(out, rr)
		}
	}
	return out
}

func fqdn(zone, name string) string {
	if name == "@" {
		return zone
	}
	return name + "." + zone
}

// Taken says whether host's name in zone has a record stackr did not make.
func (l *Leaf) Taken(ctx context.Context, a Provider, zone, host string) (bool, error) {
	l.recMu.Lock()
	defer l.recMu.Unlock()
	rows, err := l.recs.List(ctx)
	if err != nil {
		return false, err
	}
	name := Name(host, zone)
	var taken bool
	err = l.WithZone(ctx, a, zone, func(p Provider) error {
		live, err := p.Get(ctx, zone)
		taken = len(foreign(live, rows, zone, name)) > 0
		return err
	})
	return taken, err
}

// Reconcile brings the records stackr made to wants: new ones made,
// changed ones repointed, unwanted ones deleted. Nothing happens, and no
// provider is called, when the table already matches. A name with a record
// stackr did not make is skipped (*RecordTaken) unless replace holds its
// host: then that record goes and stackr's takes its place. A record
// someone edited since is theirs now: left alone. Records of a zone no
// longer managed are dropped from the table only; Forget deleted them.
func (l *Leaf) Reconcile(ctx context.Context, wants []Want, replace map[string]bool, acct Accounts) error {
	l.recMu.Lock()
	defer l.recMu.Unlock()
	rows, err := l.recs.List(ctx)
	if err != nil {
		return err
	}
	type plan struct {
		dnsDomainID string
		add         []Want            // new, or a moved row's new value
		move        []store.DNSRecord // rows whose value changes, the old value
		drop        []store.DNSRecord
	}
	zones := map[string]*plan{}
	at := func(zone, id string) *plan {
		if zones[zone] == nil {
			zones[zone] = &plan{}
		}
		if id != "" {
			zones[zone].dnsDomainID = id
		}
		return zones[zone]
	}
	want := map[string]Want{}
	held := map[string]bool{}
	for _, w := range wants {
		if w.Hold {
			held[w.Zone+"|"+strings.ToLower(w.Name)] = true
			continue
		}
		if _, dup := want[w.key()]; !dup { // ponytail: one host on two ingress nodes points at the first
			want[w.key()] = w
		}
	}
	have := map[string]store.DNSRecord{}
	for _, r := range rows {
		have[rowKey(r)] = r
		w, ok := want[rowKey(r)]
		switch {
		case !ok && held[r.Zone+"|"+strings.ToLower(r.Name)]:
		case !ok:
			id := ""
			if r.DNSDomainID != nil {
				id = *r.DNSDomainID
			}
			at(r.Zone, id).drop = append(at(r.Zone, id).drop, r)
		case w.Value != r.Value:
			p := at(r.Zone, w.DNSDomainID)
			p.move, p.add = append(p.move, r), append(p.add, w)
		case r.DomainID == nil || *r.DomainID != w.DomainID:
			r.DomainID = &w.DomainID
			if err := l.recs.Update(ctx, r); err != nil {
				return err
			}
		}
	}
	for _, w := range wants {
		if _, ok := have[w.key()]; !ok && !w.Hold && want[w.key()] == w {
			p := at(w.Zone, w.DNSDomainID)
			p.add = append(p.add, w)
		}
	}
	var taken []string
	var errsOut []error
	for _, zone := range slices.Sorted(maps.Keys(zones)) {
		p := zones[zone]
		t, err := l.apply(ctx, zone, p.dnsDomainID, p.add, p.move, p.drop, rows, replace, acct)
		taken = append(taken, t...)
		if err != nil {
			errsOut = append(errsOut, &ZoneError{Zone: zone, Err: err})
		}
	}
	if len(taken) > 0 {
		slices.Sort(taken)
		errsOut = append(errsOut, &RecordTaken{Hosts: taken})
	}
	return errors.Join(errsOut...)
}

// apply is one zone's changes under its lock, then the table.
func (l *Leaf) apply(ctx context.Context, zone, dnsDomainID string, add []Want, move, drop []store.DNSRecord,
	rows []store.DNSRecord, replace map[string]bool, acct Accounts) ([]string, error) {
	d, err := l.rows.Get(ctx, dnsDomainID)
	if dnsDomainID == "" || errors.Is(err, errs.ErrNotFound) {
		// the zone is no longer managed: nothing to call, forget the rows
		for _, r := range drop {
			if err := l.recs.Delete(ctx, r.ID); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a, err := acct(ctx, d.ConnectorID, d.OrgID)
	if err != nil {
		return nil, err
	}
	var taken []string
	var made []Want
	gone := map[string]bool{} // row ids whose record is off the zone
	err = l.WithZone(ctx, a, zone, func(pv Provider) error {
		live, err := pv.Get(ctx, zone)
		if err != nil {
			return err
		}
		isLive := func(r store.DNSRecord) bool {
			return slices.ContainsFunc(live, func(x libdns.Record) bool {
				rr := x.RR()
				return strings.EqualFold(rr.Name, r.Name) && rr.Type == r.Type && rr.Data == r.Value
			})
		}
		var del, put []libdns.Record
		for _, r := range drop {
			if isLive(r) {
				del = append(del, rr(r.Name, r.Type, r.Value))
			}
			gone[r.ID] = true
		}
		// a moved row whose old record is gone or edited: its name is
		// judged like a new one
		others := slices.DeleteFunc(slices.Clone(rows), func(r store.DNSRecord) bool {
			return slices.ContainsFunc(move, func(m store.DNSRecord) bool { return m.ID == r.ID && !isLive(m) })
		})
		for _, r := range move {
			if isLive(r) {
				del = append(del, rr(r.Name, r.Type, r.Value))
			}
		}
		for _, w := range add {
			f := foreign(live, others, zone, w.Name)
			if len(f) > 0 && !replace[fqdn(zone, w.Name)] {
				taken = append(taken, fqdn(zone, w.Name))
				continue
			}
			del = append(del, f...)
			put = append(put, rr(w.Name, w.Type, w.Value))
			made = append(made, w)
		}
		if len(del) > 0 {
			if _, err := pv.Delete(ctx, zone, del); err != nil {
				return err
			}
		}
		if len(put) > 0 {
			if _, err := pv.Append(ctx, zone, put); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return taken, err
	}
	for _, r := range drop {
		if err := l.recs.Delete(ctx, r.ID); err != nil {
			return taken, err
		}
	}
	for _, w := range made {
		i := slices.IndexFunc(move, func(r store.DNSRecord) bool { return rowKey(r) == w.key() })
		if i >= 0 {
			r := move[i]
			r.Value, r.DomainID = w.Value, &w.DomainID
			if err := l.recs.Update(ctx, r); err != nil {
				return taken, err
			}
			continue
		}
		if err := l.recs.Create(ctx, store.DNSRecord{
			ID: uuid.NewString(), DNSDomainID: &d.ID, Zone: zone, Name: w.Name, Type: w.Type,
			Value: w.Value, TTL: TTL, DomainID: &w.DomainID, CreatedAt: time.Now().UTC(),
		}); err != nil {
			return taken, err
		}
	}
	// a moved row left behind (its name now someone else's) is no longer ours
	for _, r := range move {
		if !slices.ContainsFunc(made, func(w Want) bool { return w.key() == rowKey(r) }) {
			if err := l.recs.Delete(ctx, r.ID); err != nil {
				return taken, err
			}
		}
	}
	return taken, nil
}

func rr(name, typ, value string) libdns.Record {
	return libdns.RR{Name: name, Type: typ, Data: value, TTL: TTL * time.Second}
}

// Records is every record stackr made.
func (l *Leaf) Records(ctx context.Context) ([]store.DNSRecord, error) { return l.recs.List(ctx) }

// Forget deletes the records stackr made in zone, from the provider then
// the table: what unmanaging a domain does before the domain row goes.
func (l *Leaf) Forget(ctx context.Context, a Provider, zone string) error {
	l.recMu.Lock()
	defer l.recMu.Unlock()
	rows, err := l.recs.List(ctx)
	if err != nil {
		return err
	}
	rows = slices.DeleteFunc(rows, func(r store.DNSRecord) bool { return r.Zone != zone })
	if len(rows) == 0 {
		return nil
	}
	var del []libdns.Record
	for _, r := range rows {
		del = append(del, rr(r.Name, r.Type, r.Value))
	}
	// Delete matches name, type and value: a record edited since stays.
	if err := l.WithZone(ctx, a, zone, func(p Provider) error {
		_, err := p.Delete(ctx, zone, del)
		return err
	}); err != nil {
		return err
	}
	for _, r := range rows {
		if err := l.recs.Delete(ctx, r.ID); err != nil {
			return err
		}
	}
	return nil
}
