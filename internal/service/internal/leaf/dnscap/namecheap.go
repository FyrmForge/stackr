package dnscap

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/libdns/libdns"
	ncdns "github.com/libdns/namecheap"
)

// namecheapAPI is Namecheap's production endpoint. The sandbox one
// (api.sandbox.namecheap.com) is a test's to pass.
const namecheapAPI = "https://api.namecheap.com/xml.response"

// namecheap is a Namecheap account. Records go through libdns/namecheap;
// the domain list and the switch to Namecheap DNS, which it lacks, go
// through call, which keeps Namecheap's error numbers. ClientIP is always
// set, so the library never asks a third party for this machine's address.
type namecheap struct {
	c        Creds
	endpoint string
	http     *http.Client
	lib      *ncdns.Provider
}

func newNamecheap(c Creds, endpoint string) *namecheap {
	if endpoint == "" {
		endpoint = namecheapAPI
	}
	return &namecheap{
		c:        c,
		endpoint: endpoint,
		http:     &http.Client{Timeout: 30 * time.Second},
		lib:      &ncdns.Provider{APIKey: c.Key, User: c.User, APIEndpoint: endpoint, ClientIP: c.ClientIP},
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
	Total int `xml:"CommandResponse>Paging>TotalItems"`
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
	defer resp.Body.Close()
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
// (namecheap.domains.dns.setDefault). A Namecheap domain is a registered
// one: the first label is the SLD, the rest the TLD (co.uk included).
func (n *namecheap) TakeOver(ctx context.Context, zone string) error {
	sld, tld, ok := strings.Cut(Normal(zone), ".")
	if !ok {
		return fmt.Errorf("namecheap: %q is not a domain", zone)
	}
	_, err := n.call(ctx, "namecheap.domains.dns.setDefault", url.Values{"SLD": {sld}, "TLD": {tld}})
	return err
}

func (n *namecheap) Get(ctx context.Context, zone string) ([]libdns.Record, error) {
	return n.lib.GetRecords(ctx, zone)
}

func (n *namecheap) Append(ctx context.Context, zone string, rs []libdns.Record) ([]libdns.Record, error) {
	return n.lib.AppendRecords(ctx, zone, rs)
}

func (n *namecheap) Set(ctx context.Context, zone string, rs []libdns.Record) ([]libdns.Record, error) {
	return n.lib.SetRecords(ctx, zone, rs)
}

func (n *namecheap) Delete(ctx context.Context, zone string, rs []libdns.Record) ([]libdns.Record, error) {
	return n.lib.DeleteRecords(ctx, zone, rs)
}
