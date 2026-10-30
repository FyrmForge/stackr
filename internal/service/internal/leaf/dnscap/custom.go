package dnscap

import (
	"context"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/libdns/libdns"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Custom records: the TXT, MX, CNAME, A and AAAA records people declare on
// an app hostname or a managed domain. stackr writes them with its short
// TTL, puts them back when they change outside stackr, and adds TXT, MX, A
// and AAAA next to whatever else the name has. A CNAME wants its name to
// itself, both ways.

// CustomTypes is the record types a custom record may be.
var CustomTypes = []string{"TXT", "MX", "CNAME", "A", "AAAA"}

var (
	nameLabelRE = regexp.MustCompile(`^(\*|_?[a-z0-9]([a-z0-9_-]{0,61}[a-z0-9])?)$`)
	hostLabelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

// CustomName is a custom record's name as stored: relative to its owner,
// lower case, "" for the owner itself ("@" reads the same). Letters,
// digits, dashes and underscores (_dmarc), a dot between labels.
func CustomName(name string) (string, error) {
	name = strings.Trim(strings.ToLower(strings.TrimSpace(name)), ".")
	if name == "" || name == "@" {
		return "", nil
	}
	for _, l := range strings.Split(name, ".") {
		if !nameLabelRE.MatchString(l) {
			return "", errs.Invalidf("name", "Use letters, digits, dashes and underscores, dot between names.")
		}
	}
	return name, nil
}

// CustomValue checks value for typ and returns both as stored: an A is an
// IPv4, an AAAA an IPv6, a CNAME a host, an MX "priority host", a TXT any
// text on one line.
func CustomValue(typ, value string) (string, string, error) {
	typ, value = strings.ToUpper(strings.TrimSpace(typ)), strings.TrimSpace(value)
	if !slices.Contains(CustomTypes, typ) {
		return "", "", errs.Invalidf("type", "Pick TXT, MX, CNAME, A or AAAA.")
	}
	switch typ {
	case "A":
		a, err := netip.ParseAddr(value)
		if err != nil || !a.Is4() {
			return "", "", errs.Invalidf("value", "An A record points at an IPv4 address.")
		}
		return typ, a.String(), nil
	case "AAAA":
		a, err := netip.ParseAddr(value)
		if err != nil || !a.Is6() || a.Is4In6() {
			return "", "", errs.Invalidf("value", "An AAAA record points at an IPv6 address.")
		}
		return typ, a.String(), nil
	case "CNAME":
		h, ok := targetHost(value)
		if !ok {
			return "", "", errs.Invalidf("value", "A CNAME points at a host name, like shops.example.net.")
		}
		return typ, h, nil
	case "MX":
		pref, target, _ := strings.Cut(strings.Join(strings.Fields(value), " "), " ")
		n, err := strconv.Atoi(pref)
		h, ok := targetHost(target)
		if err != nil || n < 0 || n > 65535 || !ok {
			return "", "", errs.Invalidf("value", "An MX is a priority and a mail host, like 10 mx.example.net.")
		}
		return typ, strconv.Itoa(n) + " " + h, nil
	}
	if value == "" || len(value) > 2048 || strings.ContainsAny(value, "\r\n") {
		return "", "", errs.Invalidf("value", "A TXT record is one line of text, up to 2048 characters.")
	}
	return typ, strings.Trim(value, `"`), nil
}

// targetHost is a CNAME's or MX's target as stored: lower case, no
// trailing dot.
func targetHost(v string) (string, bool) {
	v = strings.TrimSuffix(strings.ToLower(v), ".")
	if v == "" || !strings.Contains(v, ".") {
		return "", false
	}
	for _, l := range strings.Split(v, ".") {
		if !hostLabelRE.MatchString(l) && !nameLabelRE.MatchString(l) {
			return "", false
		}
	}
	return v, true
}

// sameData says whether two values of type typ are one record's: a CNAME's
// and an MX's host match without case or trailing dot.
func sameData(typ, a, b string) bool {
	if t := strings.ToUpper(typ); t == "CNAME" || t == "MX" {
		return strings.EqualFold(strings.TrimSuffix(a, "."), strings.TrimSuffix(b, "."))
	}
	return a == b
}

// CustomFQDN is a custom record's full name: name under owner.
func CustomFQDN(name, owner string) string {
	if name == "" {
		return Normal(owner)
	}
	return name + "." + Normal(owner)
}

// RecordClash refuses a custom record its name cannot hold: a CNAME where
// the name has other records, or a record where it has a CNAME.
type RecordClash struct {
	Host, Type string
	With       []string // "TYPE value" of the records there
}

func (e *RecordClash) Error() string {
	if e.Type == "CNAME" {
		return fmt.Sprintf("%s already has other records (%s); a CNAME needs the name to itself", e.Host, strings.Join(e.With, ", "))
	}
	return fmt.Sprintf("%s has a CNAME (%s); a name with a CNAME takes no other record", e.Host, strings.Join(e.With, ", "))
}

// As makes errs.IsConflict see it: the user acts on it.
func (e *RecordClash) As(target any) bool {
	c, ok := target.(*errs.Conflict)
	if ok {
		*c = errs.Conflict{Msg: e.Error()}
	}
	return ok
}

// clash is what keeps a custom record of typ off name in live: for a CNAME
// every other record there, for anything else a CNAME there. nil = none.
func clash(live []libdns.Record, zone, name, typ, value string) error {
	var with []string
	for _, r := range live {
		x := r.RR()
		if !strings.EqualFold(x.Name, name) || (strings.EqualFold(x.Type, typ) && sameData(typ, x.Data, value)) {
			continue
		}
		if typ == "CNAME" || strings.EqualFold(x.Type, "CNAME") {
			with = append(with, x.Type+" "+x.Data)
		}
	}
	if len(with) == 0 {
		return nil
	}
	return &RecordClash{Host: fqdn(zone, name), Type: typ, With: with}
}

// Fit is how a custom record lands at name (relative to zone): "taken
// over" when the zone already has the same record, "added (N others there
// stay)" next to others, "" on a name of its own; a *RecordClash when the
// name cannot hold it. A read, no write.
func (l *Leaf) Fit(ctx context.Context, p Provider, zone, name, typ, value string) (string, error) {
	live, err := l.Locked(p).Get(ctx, zone)
	if err != nil {
		return "", err
	}
	if name == "" {
		name = "@"
	}
	others := 0
	for _, r := range live {
		x := r.RR()
		if !strings.EqualFold(x.Name, name) {
			continue
		}
		if strings.EqualFold(x.Type, typ) && sameData(typ, x.Data, value) {
			return "taken over", nil
		}
		others++
	}
	if err := clash(live, zone, name, typ, value); err != nil {
		return "", err
	}
	switch others {
	case 0:
		return "", nil
	case 1:
		return "added (1 other there stays)", nil
	}
	return fmt.Sprintf("added (%d others there stay)", others), nil
}

// SetRecords replaces the custom records declared on managed domain id.
func (l *Leaf) SetRecords(ctx context.Context, id string, rs []store.CustomRecord) (store.DNSDomain, error) {
	d, err := l.rows.Get(ctx, id)
	if err != nil {
		return d, err
	}
	d.Records = rs
	return d, l.rows.Update(ctx, d)
}

// HandedBack is the records a narrowing gave back to the user.
func (l *Leaf) HandedBack(ctx context.Context) ([]store.DNSRecord, error) {
	return l.recs.ListHandedBack(ctx)
}
