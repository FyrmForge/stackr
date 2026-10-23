package dnscap

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/libdns/libdns"
	"golang.org/x/net/publicsuffix"
)

// namecheapAPI is Namecheap's production endpoint. The sandbox one
// (api.sandbox.namecheap.com) is a test's to pass.
const namecheapAPI = "https://api.namecheap.com/xml.response"

// namecheap is a Namecheap account. Every command goes through call,
// which keeps Namecheap's error numbers and the key out of URLs. Not
// libdns/namecheap: it splits a zone at the first TLD whose letters it ends
// in (x.eco as x.e + co), drops the zone's mail setting on every write and
// loses the error numbers.
type namecheap struct {
	c        Creds
	endpoint string
	http     *http.Client
}

func newNamecheap(c Creds, endpoint string) *namecheap {
	if endpoint == "" {
		endpoint = namecheapAPI
	}
	return &namecheap{
		c:        c,
		endpoint: endpoint,
		http:     &http.Client{Timeout: 30 * time.Second},
	}
}

// ncResponse is the part of Namecheap's XML answer stackr reads.
type ncResponse struct {
	Status string `xml:"Status,attr"`
	Errors []struct {
		Number string `xml:"Number,attr"`
		Text   string `xml:",chardata"`
	} `xml:"Errors>Error"`
	Domains []struct {
		Name     string `xml:"Name,attr"`
		IsOurDNS bool   `xml:"IsOurDNS,attr"`
	} `xml:"CommandResponse>DomainGetListResult>Domain"`
	Total int       `xml:"CommandResponse>Paging>TotalItems"`
	Hosts zoneHosts `xml:"CommandResponse>DomainDNSGetHostsResult"`
}

// call runs one API command. The key goes in the POST body, never the URL,
// so a transport error cannot print it.
func (n *namecheap) call(ctx context.Context, command string, extra url.Values) (ncResponse, error) {
	var out ncResponse
	q := url.Values{
		"ApiUser":  {n.c.User},
		"ApiKey":   {n.c.Key},
		"UserName": {n.c.User},
		"ClientIp": {n.c.ClientIP},
		"Command":  {command},
	}
	for k, v := range extra {
		q[k] = v
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.endpoint, strings.NewReader(q.Encode()))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := n.http.Do(req)
	if err != nil {
		return out, fmt.Errorf("namecheap: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return out, fmt.Errorf("namecheap: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("namecheap: HTTP %d", resp.StatusCode)
	}
	if err := xml.Unmarshal(body, &out); err != nil {
		return out, fmt.Errorf("namecheap: unreadable answer: %w", err)
	}
	if len(out.Errors) > 0 {
		e := out.Errors[0]
		return out, &APIError{Number: e.Number, Text: strings.TrimSpace(e.Text)}
	}
	if !strings.EqualFold(out.Status, "OK") {
		return out, fmt.Errorf("namecheap: status %q", out.Status)
	}
	return out, nil
}

// Zones is every domain on the account (namecheap.domains.getList, 100 a
// page).
func (n *namecheap) Zones(ctx context.Context) ([]Zone, error) {
	var zs []Zone
	for page := 1; ; page++ {
		r, err := n.call(ctx, "namecheap.domains.getList", url.Values{
			"PageSize": {"100"},
			"Page":     {strconv.Itoa(page)},
		})
		if err != nil {
			return nil, err
		}
		for _, d := range r.Domains {
			zs = append(zs, Zone{Name: Normal(d.Name), OurDNS: d.IsOurDNS})
		}
		if len(r.Domains) == 0 || len(zs) >= r.Total {
			return zs, nil
		}
	}
}

// TakeOver points the domain at Namecheap's own DNS
// (namecheap.domains.dns.setDefault).
func (n *namecheap) TakeOver(ctx context.Context, zone string) error {
	sld, tld, err := Split(zone)
	if err != nil {
		return err
	}
	_, err = n.call(ctx, "namecheap.domains.dns.setDefault", url.Values{"SLD": {sld}, "TLD": {tld}})
	return err
}

// Split is a registered domain's SLD and TLD as Namecheap takes them, the
// TLD by the public suffix list (fyrmforge.co.uk is fyrmforge + co.uk). A
// name under a registered domain (shop.example.com) is refused: Namecheap's
// zones are whole domains.
func Split(zone string) (sld, tld string, err error) {
	zone = Normal(zone)
	tld, _ = publicsuffix.PublicSuffix(zone)
	sld, ok := strings.CutSuffix(zone, "."+tld)
	if !ok || sld == "" || strings.Contains(sld, ".") {
		return "", "", fmt.Errorf("namecheap: %q is not a registered domain", zone)
	}
	return sld, tld, nil
}

// host is one record as getHosts returns it and setHosts takes it back.
// Every field is sent back as read, so a record stackr does not touch keeps
// its TTL and MX preference.
type host struct {
	Name    string `xml:"Name,attr"`
	Type    string `xml:"Type,attr"`
	Address string `xml:"Address,attr"`
	MXPref  string `xml:"MXPref,attr"`
	TTL     string `xml:"TTL,attr"`
}

// zoneHosts is a zone's records and its mail setting (EmailType: FWD is
// Namecheap's forwarding, whose MX records getHosts does not list).
// setHosts without it would turn the mail off.
type zoneHosts struct {
	EmailType string `xml:"EmailType,attr"`
	Hosts     []host `xml:"host"`
}

func (n *namecheap) getHosts(ctx context.Context, zone string) (zoneHosts, error) {
	sld, tld, err := Split(zone)
	if err != nil {
		return zoneHosts{}, err
	}
	r, err := n.call(ctx, "namecheap.domains.dns.getHosts", url.Values{"SLD": {sld}, "TLD": {tld}})
	return r.Hosts, err
}

// setHosts replaces the zone's records with z: Namecheap has no call for
// one record, so every change is a read, an edit and this, under the zone's
// lock.
func (n *namecheap) setHosts(ctx context.Context, zone string, z zoneHosts) error {
	sld, tld, err := Split(zone)
	if err != nil {
		return err
	}
	q := url.Values{"SLD": {sld}, "TLD": {tld}}
	if z.EmailType != "" {
		q.Set("EmailType", z.EmailType)
	}
	for i, h := range z.Hosts {
		k := strconv.Itoa(i + 1)
		addr := h.Address
		if h.Type == "CAA" { // read as "0 issue ca.example", written as three fields
			if f := strings.SplitN(addr, " ", 3); len(f) == 3 {
				q.Set("Flag"+k, f[0])
				q.Set("Tag"+k, f[1])
				addr = f[2]
			}
		}
		q.Set("HostName"+k, h.Name)
		q.Set("RecordType"+k, h.Type)
		q.Set("Address"+k, addr)
		if h.TTL != "" {
			q.Set("TTL"+k, h.TTL)
		}
		if h.MXPref != "" && h.Type == "MX" {
			q.Set("MXPref"+k, h.MXPref)
		}
	}
	_, err = n.call(ctx, "namecheap.domains.dns.setHosts", q)
	return err
}

func toHost(r libdns.Record) host {
	rr := r.RR()
	h := host{Name: rr.Name, Type: rr.Type, Address: rr.Data, TTL: strconv.Itoa(int(rr.TTL.Seconds()))}
	if rr.TTL == 0 {
		h.TTL = ""
	}
	if mx, ok := r.(libdns.MX); ok {
		h.MXPref, h.Address = strconv.Itoa(int(mx.Preference)), mx.Target
	}
	return h
}

func (h host) record() libdns.Record {
	ttl, _ := strconv.Atoi(h.TTL)
	return libdns.RR{Name: h.Name, Type: h.Type, Data: h.Address, TTL: time.Duration(ttl) * time.Second}
}

// same: one record, by name, type and value.
func (h host) same(o host) bool {
	return strings.EqualFold(h.Name, o.Name) && strings.EqualFold(h.Type, o.Type) && h.Address == o.Address
}

func (n *namecheap) Get(ctx context.Context, zone string) ([]libdns.Record, error) {
	z, err := n.getHosts(ctx, zone)
	out := make([]libdns.Record, 0, len(z.Hosts))
	for _, h := range z.Hosts {
		out = append(out, h.record())
	}
	return out, err
}

// edit is a read, fn on the records, and a write when fn changed them.
func (n *namecheap) edit(ctx context.Context, zone string, fn func([]host) ([]host, bool)) error {
	z, err := n.getHosts(ctx, zone)
	if err != nil {
		return err
	}
	hs, changed := fn(z.Hosts)
	if !changed {
		return nil
	}
	z.Hosts = hs
	return n.setHosts(ctx, zone, z)
}

// Append adds rs; a record already there (name, type, value) stays one.
func (n *namecheap) Append(ctx context.Context, zone string, rs []libdns.Record) ([]libdns.Record, error) {
	return rs, n.edit(ctx, zone, func(hs []host) ([]host, bool) {
		changed := false
		for _, r := range rs {
			h := toHost(r)
			if !slices.ContainsFunc(hs, h.same) {
				hs, changed = append(hs, h), true
			}
		}
		return hs, changed
	})
}

// Set makes rs the only records of their names and types.
func (n *namecheap) Set(ctx context.Context, zone string, rs []libdns.Record) ([]libdns.Record, error) {
	return rs, n.edit(ctx, zone, func(hs []host) ([]host, bool) {
		var add []host
		for _, r := range rs {
			add = append(add, toHost(r))
		}
		hs = slices.DeleteFunc(hs, func(h host) bool {
			return slices.ContainsFunc(add, func(a host) bool {
				return strings.EqualFold(a.Name, h.Name) && strings.EqualFold(a.Type, h.Type)
			})
		})
		return append(hs, add...), true
	})
}

// Delete removes the records matching rs by name, type and value; others
// of the same name are left.
func (n *namecheap) Delete(ctx context.Context, zone string, rs []libdns.Record) ([]libdns.Record, error) {
	var gone []libdns.Record
	err := n.edit(ctx, zone, func(hs []host) ([]host, bool) {
		out := slices.DeleteFunc(hs, func(h host) bool {
			hit := slices.ContainsFunc(rs, func(r libdns.Record) bool { return toHost(r).same(h) })
			if hit {
				gone = append(gone, h.record())
			}
			return hit
		})
		return out, len(gone) > 0
	})
	return gone, err
}
